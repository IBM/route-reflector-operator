package node

import (
	"context"

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

	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	clientv3 "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/options"
)

var log = logf.Log.WithName("controller_node")

const (
	routeReflectorLabel = "route-reflector"
	zoneLabel           = "failure-domain.beta.kubernetes.io/zone"
	workerIDLabel       = "ibm-cloud.kubernetes.io/worker-id"
	clusterID           = "0.0.0.1"
)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

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
	return &ReconcileNode{client: mgr.GetClient(), calico: c, rconfig: rc, scheme: mgr.GetScheme()}
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
			return true
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
	client  client.Client
	calico  clientv3.Interface
	rconfig *rest.Config
	scheme  *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Node object and makes changes based on the state read
// and what is in the Node.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileNode) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	p := reconcileImplParams{
		request: request,
		client:  r.client,

		calico: reconcileCalicoParams{
			BGPConfigurations: r.calico.BGPConfigurations(),
			BGPPeers:          r.calico.BGPPeers(),
			Nodes:             r.calico.Nodes(),
		},

		rconfig: r.rconfig,
		scheme:  r.scheme,
	}
	result, err := p.reconcileImpl(request)
	return result, err
}

type reconcileImplKubernetesClient interface {
	Get(context.Context, client.ObjectKey, runtime.Object) error
	List(context.Context, runtime.Object, ...client.ListOption) error
	Update(context.Context, runtime.Object, ...client.UpdateOption) error
}

type reconcileCalicoParams struct {
	BGPConfigurations reconcileImplCalicoBGPConfigurations
	BGPPeers          reconcileImplCalicoBGPPeers
	Nodes             reconcileImplCalicoNodes
}

type reconcileImplCalicoBGPConfigurations interface {
	Create(context.Context, *apiv3.BGPConfiguration, options.SetOptions) (*apiv3.BGPConfiguration, error)
	Update(context.Context, *apiv3.BGPConfiguration, options.SetOptions) (*apiv3.BGPConfiguration, error)
	Get(context.Context, string, options.GetOptions) (*apiv3.BGPConfiguration, error)
}

type reconcileImplCalicoBGPPeers interface {
	Create(context.Context, *apiv3.BGPPeer, options.SetOptions) (*apiv3.BGPPeer, error)
	Update(context.Context, *apiv3.BGPPeer, options.SetOptions) (*apiv3.BGPPeer, error)
	Delete(context.Context, string, options.DeleteOptions) (*apiv3.BGPPeer, error)
	Get(context.Context, string, options.GetOptions) (*apiv3.BGPPeer, error)
}

type reconcileImplCalicoNodes interface {
	Update(context.Context, *apiv3.Node, options.SetOptions) (*apiv3.Node, error)
	Get(context.Context, string, options.GetOptions) (*apiv3.Node, error)
}

type reconcileImplParams struct {
	request reconcile.Request

	client  reconcileImplKubernetesClient
	calico  reconcileCalicoParams
	rconfig *rest.Config
	scheme  *runtime.Scheme
}

func (p *reconcileImplParams) reconcileImpl(request reconcile.Request) (reconcile.Result, error) {
	_ = log.WithValues("route-reflector-operator", "AutoScaler")
	log.Info("Reconciling Node")

	// Fetch the Node instance
	instance := &corev1.Node{}
	err := p.client.Get(context.Background(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if isLabeled(instance) && instance.GetDeletionTimestamp() != nil {
		log.Info("Oh noes! Loesing a rOUte-rEfLEcToR")
	}

	return reconcile.Result{}, nil
}

func isLabeled(node *corev1.Node) bool {
	return node.Labels[routeReflectorLabel] == "true"
}

// When the nodes fails some healthchecks? E.g. reboot
func isReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return true
		}
	}
	return false
}

// Cordoning sets this to true
func isSchedulable(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	return true
}

// Cordoning sets this taint
func isTainted(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node.kubernetes.io/unschedulable" {
			return true
		}
	}
	return false
}
