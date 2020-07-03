package node

import (
	"context"
	"fmt"
	"os"

	routereflectorv1 "github.com/IBM/route-reflector-operator/pkg/apis/routereflector/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/mhmxs/calico-route-reflector-operator/bgppeer"
	"github.com/mhmxs/calico-route-reflector-operator/controllers"
	"github.com/mhmxs/calico-route-reflector-operator/datastores"
	"github.com/mhmxs/calico-route-reflector-operator/topologies"
	clientv3 "github.com/projectcalico/libcalico-go/lib/clientv3"
)

var log = logf.Log.WithName("controller_node")

const (
	routeReflectorLabel           = "route-reflector.ibm.com/rr-id"
	zoneLabel                     = "failure-domain.beta.kubernetes.io/zone"
	workerIDLabel                 = "ibm-cloud.kubernetes.io/worker-id"
	routeReflectorConfigNameSpace = "kube-system"
	routeReflectorConfigName      = "route-reflector-operator"
)

type s struct {
	s string
}

// Add creates a new Node Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	var err error
	var c clientv3.Interface
	var rc *rest.Config

	if c, err = clientv3.NewFromEnv(); err != nil {
		return nil
	}

	// for pods/exec
	if rc, err = rest.InClusterConfig(); err != nil {
		log.Error(err, "Failed to get *rest.Config")
		return nil
	}

	topologyConfig := topologies.Config{
		NodeLabelKey: routeReflectorLabel,
		ZoneLabel:    zoneLabel,
		ClusterID:    "224.0.0.0",
		Min:          3,
		Max:          20,
		Ration:       0.05,
	}
	topology := topologies.NewMultiTopology(topologyConfig)

	dsType := os.Getenv("DATASTORE_TYPE")
	var datastore datastores.Datastore
	switch dsType {
	case "kubernetes":
		datastore = datastores.NewKddDatastore(&topology)
	case "etcdv3":
		datastore = datastores.NewEtcdDatastore(&topology, c)
	default:
		panic("Datastore not supported " + dsType)
	}

	incompatibleLabels := map[string]*string{
		"dedicated": &(&s{s: "gateway"}).s,
	}

	return &ReconcileNode{
		client:  mgr.GetClient(),
		calico:  c,
		rconfig: rc,
		scheme:  mgr.GetScheme(),
		autoscaler: controllers.RouteReflectorConfigReconciler{
			Client:             mgr.GetClient(),
			CalicoClient:       c,
			Log:                logf.Log.WithName("controller_node"),
			Scheme:             mgr.GetScheme(),
			NodeLabelKey:       topologyConfig.NodeLabelKey,
			IncompatibleLabels: incompatibleLabels,
			Topology:           topology,
			Datastore:          datastore,
			BGPPeer:            *bgppeer.NewBGPPeer(c),
		},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("node-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Node
	err = c.Watch(&source.Kind{Type: &corev1.Node{}}, &handler.EnqueueRequestForObject{}, &predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileNode implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileNode{}

// ReconcileNode reconciles a Node object
type ReconcileNode struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client     client.Client
	calico     clientv3.Interface
	rconfig    *rest.Config
	scheme     *runtime.Scheme
	autoscaler controllers.RouteReflectorConfigReconciler
}

// Reconcile reads that state of the cluster for a Node object and makes changes based on the state read
// and what is in the Node.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileNode) Reconcile(request reconcile.Request) (reconcile.Result, error) {

	res, err := r.autoscaler.Reconcile(request)
	if err != nil {
		return res, err
	}

	if res.Requeue {
		return res, nil
	}

	// Fetch the RouteReflector instance(s)
	routereflector := &routereflectorv1.RouteReflector{}
	err = r.client.Get(context.Background(), client.ObjectKey{
		Namespace: routeReflectorConfigNameSpace,
		Name:      routeReflectorConfigName,
	}, routereflector)

	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Error(err, fmt.Sprintf("Custom Resource: %s/%s not found, can't update AutoScalerConverged state", routeReflectorConfigNameSpace, routeReflectorConfigName))
			// FIXME: handle missing Custom Resource (e.g. create it?)
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, fmt.Sprintf("Failed to read Custom Resource: %s/%s, can't update AutoScalerConverged state", routeReflectorConfigNameSpace, routeReflectorConfigName))
		return reconcile.Result{}, err
	}

	if routereflector.Status.AutoScalerConverged != true {

		log.Info("Setting AutoScalerConverged state to true")

		routereflector.Status.AutoScalerConverged = true
		if err = r.client.Status().Update(context.Background(), routereflector, &client.UpdateOptions{}); err != nil {
			log.Error(err, "Failed to update AutoScalerConverged state")
			return reconcile.Result{}, err
		}
	}

	return res, nil
}
