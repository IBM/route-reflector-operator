package node

import (
	"context"
	"fmt"
	"os"
	"time"

	routereflectorv1 "github.com/IBM/route-reflector-operator/pkg/apis/routereflector/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	//logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/IBM/route-reflector-operator/bgppeer"
	"github.com/IBM/route-reflector-operator/datastores"
	"github.com/IBM/route-reflector-operator/topologies"

	clientv3 "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/options"

	"github.com/go-logr/logr"
	"github.com/mhmxs/calico-route-reflector-operator/controllers"
	calicoApi "github.com/projectcalico/libcalico-go/lib/apis/v3"
	calicoClient "github.com/projectcalico/libcalico-go/lib/clientv3"
	calicoErrors "github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/prometheus/common/log"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

//var log = logf.Log.WithName("controller_node")

const (
	routeReflectorLabel           = "route-reflector"
	zoneLabel                     = "failure-domain.beta.kubernetes.io/zone"
	workerIDLabel                 = "ibm-cloud.kubernetes.io/worker-id"
	clusterID                     = "0.0.0.1"
	routeReflectorConfigNameSpace = "kube-system"
	routeReflectorConfigName      = "route-reflector-operator"
)

type s struct {
	s string
}

var routeReflectorsUnderOperation = map[types.UID]bool{}

var notReadyTaints = map[string]bool{
	"node.kubernetes.io/not-ready":                   false,
	"node.kubernetes.io/unreachable":                 false,
	"node.kubernetes.io/out-of-disk":                 false,
	"node.kubernetes.io/memory-pressure":             false,
	"node.kubernetes.io/disk-pressure":               false,
	"node.kubernetes.io/network-unavailable":         false,
	"node.kubernetes.io/unschedulable":               false,
	"node.cloudprovider.kubernetes.io/uninitialized": false,
}

var (
	nodeNotFound = ctrl.Result{}
	nodeCleaned  = ctrl.Result{Requeue: true}
	nodeReverted = ctrl.Result{Requeue: true}
	finished     = ctrl.Result{}

	nodeGetError          = ctrl.Result{}
	nodeCleanupError      = ctrl.Result{}
	nodeListError         = ctrl.Result{}
	nodeRevertError       = ctrl.Result{}
	nodeRevertUpdateError = ctrl.Result{}
	nodeUpdateError       = ctrl.Result{}
	rrListError           = ctrl.Result{}
	rrPeerListError       = ctrl.Result{}
	bgpPeerError          = ctrl.Result{Requeue: true}
	bgpPeerRemoveError    = ctrl.Result{Requeue: true}
)

// RouteReflectorConfigReconciler reconciles a RouteReflectorConfig object
type RouteReflectorConfigReconciler struct {
	client.Client
	CalicoClient       calicoClient.Interface
	Log                logr.Logger
	Scheme             *runtime.Scheme
	NodeLabelKey       string
	IncompatibleLabels map[string]*string
	Topology           topologies.Topology
	Datastore          datastores.Datastore
	BGPPeer            bgppeer.BGPPeer
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
	//var rc *rest.Config

	if c, err = clientv3.NewFromEnv(); err != nil {
		return nil
	}

	// for pods/exec
	// if rc, err = rest.InClusterConfig(); err != nil {
	// 	log.Error(err, "Failed to get *rest.Config")
	// 	return nil
	// }

	topologyConfig := topologies.Config{
		NodeLabelKey: "calico-route-reflector",
		ZoneLabel:    "topology.kubernetes.io/zone",
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

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	return &RouteReflectorConfigReconciler{
		Client:             mgr.GetClient(),
		CalicoClient:       c,
		Log:                ctrl.Log.WithName("controllers").WithName("RouteReflectorConfig"),
		Scheme:             mgr.GetScheme(),
		NodeLabelKey:       topologyConfig.NodeLabelKey,
		IncompatibleLabels: incompatibleLabels,
		Topology:           topology,
		Datastore:          datastore,
		BGPPeer:            *bgppeer.NewBGPPeer(c),
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
var _ reconcile.Reconciler = &RouteReflectorConfigReconciler{}

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
func (r *RouteReflectorConfigReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = r.Log.WithValues("routereflectorconfig", req.Name)

	currentNode := corev1.Node{}
	if err := r.Client.Get(context.Background(), req.NamespacedName, &currentNode); err != nil && !errors.IsNotFound(err) {
		log.Errorf("Unable to fetch node %s because of %s", req.Name, err.Error())
		return nodeGetError, err
	} else if errors.IsNotFound(err) {
		log.Debugf("Node not found %s", req.Name)
		return nodeNotFound, nil
	} else if err == nil && r.Topology.IsRouteReflector(string(currentNode.GetUID()), currentNode.GetLabels()) && currentNode.GetDeletionTimestamp() != nil ||
		!isNodeReady(&currentNode) || !isNodeSchedulable(&currentNode) || !r.isNodeCompatible(&currentNode) {
		// Node is deleted right now or has some issues, better to remove form RRs
		if err := r.removeRRStatus(req, &currentNode); err != nil {
			log.Errorf("Unable to cleanup label on %s because of %s", req.Name, err.Error())
			return nodeCleanupError, err
		}

		log.Infof("Label was removed from node %s time to re-reconcile", req.Name)
		return nodeCleaned, nil
	}

	listOptions := r.Topology.NewNodeListOptions(currentNode.GetLabels())
	log.Debugf("List options are %v", listOptions)
	nodeList := corev1.NodeList{}
	if err := r.Client.List(context.Background(), &nodeList, &listOptions); err != nil {
		log.Errorf("Unable to list nodes because of %s", err.Error())
		return nodeListError, err
	}
	log.Debugf("Total number of nodes %d", len(nodeList.Items))

	readyNodes, actualRRNumber, nodes := r.collectNodeInfo(nodeList.Items)
	log.Infof("Nodes are ready %d", readyNodes)
	log.Infof("Actual number of healthy route reflector nodes are %d", actualRRNumber)

	expectedRRNumber := r.Topology.CalculateExpectedNumber(readyNodes)
	log.Infof("Expected number of route reflector nodes are %d", expectedRRNumber)

	for n, isReady := range nodes {
		if status, ok := routeReflectorsUnderOperation[n.GetUID()]; ok {
			// Node was under operation, better to revert it
			var err error
			if status {
				err = r.Datastore.RemoveRRStatus(n)
			} else {
				err = r.Datastore.AddRRStatus(n)
			}
			if err != nil {
				log.Errorf("Failed to revert node %s because of %s", n.GetName(), err.Error())
				return nodeRevertError, err
			}

			log.Infof("Revert route reflector label on %s to %t", n.GetName(), !status)
			if err := r.Client.Update(context.Background(), n); err != nil && !errors.IsNotFound(err) {
				log.Errorf("Failed to revert update node %s because of %s", n.GetName(), err.Error())
				return nodeRevertUpdateError, err
			}

			delete(routeReflectorsUnderOperation, n.GetUID())

			return nodeReverted, nil
		}

		if !isReady || expectedRRNumber == actualRRNumber {
			continue
		}

		if diff := expectedRRNumber - actualRRNumber; diff != 0 {
			if updated, err := r.updateRRStatus(n, diff); err != nil {
				log.Errorf("Unable to update node %s because of %s", n.GetName(), err.Error())
				return nodeUpdateError, err
			} else if updated && diff > 0 {
				actualRRNumber++
			} else if updated && diff < 0 {
				actualRRNumber--
			}
		}
	}

	if expectedRRNumber != actualRRNumber {
		log.Errorf("Actual number %d is different than expected %d", actualRRNumber, expectedRRNumber)
	}

	rrLables := client.HasLabels{r.NodeLabelKey}
	rrListOptions := client.ListOptions{}
	rrLables.ApplyToList(&rrListOptions)
	log.Debugf("RR list options are %v", rrListOptions)

	rrList := corev1.NodeList{}
	if err := r.Client.List(context.Background(), &rrList, &rrListOptions); err != nil {
		log.Errorf("Unable to list route reflectors because of %s", err.Error())
		return rrListError, err
	}
	log.Debugf("Route reflectors are: %v", rrList.Items)

	existingBGPPeers, err := r.BGPPeer.ListBGPPeers()
	if err != nil {
		log.Errorf("Unable to list BGP peers because of %s", err.Error())
		return rrPeerListError, err
	}

	log.Debugf("Existing BGPeers are: %v", existingBGPPeers.Items)

	currentBGPPeers := r.Topology.GenerateBGPPeers(rrList.Items, nodes, existingBGPPeers)
	log.Debugf("Current BGPeers are: %v", currentBGPPeers)

	err = retry(5, 2*time.Second, func() (err error) {

		for _, bp := range currentBGPPeers {
			var updateBGPPeer *calicoApi.BGPPeer

			if updateBGPPeer, err = r.CalicoClient.BGPPeers().Get(context.Background(), bp.Name, options.GetOptions{}); err != nil {
				if _, exists := err.(calicoErrors.ErrorResourceDoesNotExist); !exists {
					log.Errorf("Unable to fetch update BGPPeer: %s", err.Error())
				} else {
					err = nil
					if err = r.BGPPeer.SaveBGPPeer(&bp); err != nil {
						log.Errorf("Unable to save new BGPPeer: %s", err.Error())
					}
				}
			} else {
				updateBGPPeer.Spec.NodeSelector = bp.Spec.NodeSelector
				updateBGPPeer.Spec.PeerSelector = bp.Spec.PeerSelector
				err = nil
				if err = r.BGPPeer.SaveBGPPeer(updateBGPPeer); err != nil {
					log.Errorf("Unable to save updated BGPPeer: %s", err.Error())
				}
			}
		}
		return

	})

	if err != nil {
		log.Errorf("Unable to refresh BGPPeers: %s", err.Error())
		return bgpPeerError, nil
	}

	for _, p := range existingBGPPeers.Items {
		if !findBGPPeer(currentBGPPeers, p.GetName()) {
			log.Debugf("Removing BGPPeer: %s", p.GetName())
			if err := r.BGPPeer.RemoveBGPPeer(&p); err != nil {
				log.Errorf("Unable to remove BGPPeer because of %s", err.Error())
				return bgpPeerRemoveError, nil
			}
		}
	}

	// Fetch the RouteReflector instance(s)
	routereflector := &routereflectorv1.RouteReflector{}
	err = r.Client.Get(context.Background(), client.ObjectKey{
		Namespace: routeReflectorConfigNameSpace,
		Name:      routeReflectorConfigName,
	}, routereflector)

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

	if routereflector.Status.AutoScalerConverged != true {

		log.Info("Setting AutoScalerConverged state to true")

		routereflector.Status.AutoScalerConverged = true
		if err = r.Client.Status().Update(context.Background(), routereflector, &client.UpdateOptions{}); err != nil {
			log.Error(err, "Failed to update AutoScalerConverged state")
			return reconcile.Result{}, err
		}
	}

	log.Infof("FINISHED!!!!")
	return finished, nil
}

func retry(attempts int, sleep time.Duration, f func() error) (err error) {
	for i := 0; ; i++ {
		err = f()
		if err == nil {
			return
		}

		if i >= (attempts - 1) {
			break
		}

		time.Sleep(sleep)

		log.Errorf("retrying after error:", err.Error())
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func (r *RouteReflectorConfigReconciler) removeRRStatus(req ctrl.Request, node *corev1.Node) error {
	routeReflectorsUnderOperation[node.GetUID()] = false

	if err := r.Datastore.RemoveRRStatus(node); err != nil {
		log.Errorf("Unable to cleanup RR status %s because of %s", node.GetName(), err.Error())
		return err
	}

	log.Infof("Removing route reflector label from %s", node.GetName())
	if err := r.Client.Update(context.Background(), node); err != nil {
		log.Errorf("Unable to cleanup node %s because of %s", node.GetName(), err.Error())
		return err
	}

	delete(routeReflectorsUnderOperation, node.GetUID())

	return nil
}

func (r *RouteReflectorConfigReconciler) updateRRStatus(node *corev1.Node, diff int) (bool, error) {
	if labeled := r.Topology.IsRouteReflector(string(node.GetUID()), node.GetLabels()); labeled && diff < 0 {
		return true, r.Datastore.RemoveRRStatus(node)
	} else if labeled || diff <= 0 {
		return false, nil
	}

	routeReflectorsUnderOperation[node.GetUID()] = true

	if err := r.Datastore.AddRRStatus(node); err != nil {
		log.Errorf("Unable to add RR status %s because of %s", node.GetName(), err.Error())
		return false, err
	}

	log.Infof("Adding route reflector label to %s", node.GetName())
	if err := r.Client.Update(context.Background(), node); err != nil {
		log.Errorf("Unable to update node %s because of %s", node.GetName(), err.Error())
		return false, err
	}

	delete(routeReflectorsUnderOperation, node.GetUID())

	return true, nil
}

func (r *RouteReflectorConfigReconciler) collectNodeInfo(allNodes []corev1.Node) (readyNodes int, actualReadyNumber int, filtered map[*corev1.Node]bool) {
	filtered = map[*corev1.Node]bool{}

	for i, n := range allNodes {
		isOK := isNodeReady(&n) && isNodeSchedulable(&n) && r.isNodeCompatible(&n)

		filtered[&allNodes[i]] = isOK
		if isOK {
			readyNodes++
			if r.Topology.IsRouteReflector(string(n.GetUID()), n.GetLabels()) {
				actualReadyNumber++
			}
		}
	}

	return
}

func (r *RouteReflectorConfigReconciler) isNodeCompatible(node *corev1.Node) bool {
	for k, v := range node.GetLabels() {
		if iv, ok := r.IncompatibleLabels[k]; ok && (iv == nil || *iv == v) {
			return false
		}
	}

	return true
}

func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == "True"
		}
	}

	return false
}

func isNodeSchedulable(node *corev1.Node) bool {
	if node.Spec.Unschedulable == true {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if _, ok := notReadyTaints[taint.Key]; ok {
			return false
		}
	}

	return true
}

func findBGPPeer(peers []calicoApi.BGPPeer, name string) bool {
	for _, p := range peers {
		if p.GetName() == name {
			return true
		}
	}

	return false
}

//
//
//
//

func (r *ReconcileNode) reconcileOLD(request reconcile.Request) (reconcile.Result, error) {

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
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
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
