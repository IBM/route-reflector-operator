package routereflector

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	routereflectorv1 "github.com/IBM/route-reflector-operator/pkg/apis/routereflector/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	clientv3 "github.com/projectcalico/libcalico-go/lib/clientv3"
	calicoErrors "github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/options"

	"github.com/mhmxs/calico-route-reflector-operator/bgppeer"
	"github.com/mhmxs/calico-route-reflector-operator/topologies"
)

var log = logf.Log.WithName("controller_routereflector")

const (
	routeReflectorLabel           = "route-reflector.ibm.com/rr-id"
	zoneLabel                     = "failure-domain.beta.kubernetes.io/zone"
	routeReflectorConfigNameSpace = "kube-system"
	routeReflectorConfigName      = "route-reflector-operator"
)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new RouteReflector Controller and adds it to the Manager. The Manager will set fields on the Controller
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

	// for RRs per Node
	topologyConfig := topologies.Config{
		NodeLabelKey: routeReflectorLabel,
		ZoneLabel:    zoneLabel,
		ClusterID:    "224.0.0.0",
		Min:          3,
		Max:          20,
		Ration:       0.05,
	}
	topology := topologies.NewMultiTopology(topologyConfig)
	bgppeer := *bgppeer.NewBGPPeer(c)

	return &ReconcileRouteReflector{client: mgr.GetClient(), calico: c, rconfig: rc, scheme: mgr.GetScheme(), topology: topology, bgppeer: bgppeer}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("routereflector-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource RouteReflector
	err = c.Watch(&source.Kind{Type: &routereflectorv1.RouteReflector{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner RouteReflector
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &routereflectorv1.RouteReflector{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileRouteReflector implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileRouteReflector{}

// ReconcileRouteReflector reconciles a RouteReflector object
type ReconcileRouteReflector struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	calico   clientv3.Interface
	rconfig  *rest.Config
	scheme   *runtime.Scheme
	topology topologies.Topology
	bgppeer  bgppeer.BGPPeer
}

// Reconcile reads that state of the cluster for a RouteReflector object and makes changes based on the state read
// and what is in the RouteReflector.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileRouteReflector) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	p := reconcileImplParams{
		request: request,
		client:  r.client,
		calico: reconcileCalicoParams{
			BGPConfigurations: r.calico.BGPConfigurations(),
			BGPPeers:          r.calico.BGPPeers(),
			Nodes:             r.calico.Nodes(),
		},

		rconfig:  r.rconfig,
		scheme:   r.scheme,
		topology: r.topology,
		bgppeer:  r.bgppeer,
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

	client   reconcileImplKubernetesClient
	calico   reconcileCalicoParams
	rconfig  *rest.Config
	scheme   *runtime.Scheme
	topology topologies.Topology
	bgppeer  bgppeer.BGPPeer
}

func (p *reconcileImplParams) reconcileImpl(request reconcile.Request) (reconcile.Result, error) {
	//func (r *ReconcileRouteReflector) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	_ = log.WithValues("route-reflector-operator", "MigrationWorkflow")
	log.Info("Reconciling RouteReflector")

	// Fetch the RouteReflector instance(s)
	routereflector := &routereflectorv1.RouteReflector{}
	err := p.client.Get(context.Background(), client.ObjectKey{
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

	var autoScalerConverged, fullMesh bool

	// The autoscaler controller should update the CR Status failed after its first successful reconcile loop
	if autoScalerConverged, err = p.isAutoScalerConverged(routereflector); err != nil {
		log.Error(err, "Failed to isAutoScalerConverged")
		return reconcile.Result{}, nil
	}

	// If full-mesh is enabled, we assume RR state is not yet enabled
	if fullMesh, err = p.isFullMeshEnabled(); err != nil {
		log.Error(err, "Failed to isRrEnabled")
		return reconcile.Result{}, nil
	}

	// Execute migration to RRs workflow
	if autoScalerConverged && fullMesh {

		log.Info("route-reflector-operator", "====================================================================", "")
		log.Info("route-reflector-operator", "RR AutoScaler CONVERGED, Calico NodeToNodeMesh is ENABLED,", "")
		log.Info("route-reflector-operator", "Starting migration to RRs workflow", "")
		log.Info("route-reflector-operator", "====================================================================", "")

		// Fetch NodeList for createNodeNameToPeersMapping()
		nodeList := &corev1.NodeList{}
		if err = p.client.List(context.Background(), nodeList, &client.ListOptions{}); err != nil {
			log.Error(err, "Failed to fetch NodeList")
			return reconcile.Result{}, err
		}
		// FIXME: use . to generate nodes
		// func (r *RouteReflectorConfigReconciler) collectNodeInfo(allNodes []corev1.Node) (readyNodes int, actualReadyNumber int, filtered map[*corev1.Node]bool)
		//
		// Create map for createNodeNameToPeersMapping()
		nodes := map[*corev1.Node]bool{}
		for i := range nodeList.Items {
			nodes[&nodeList.Items[i]] = true
		}

		var peersOfNode map[string]map[*corev1.Node]bool
		if peersOfNode, err = p.createNodeNameToPeersMapping(nodes); err != nil {
			return reconcile.Result{}, err
		}

		// log.Info(fmt.Sprintf("len(rrNodes)=%v len(nodes)=%v len(rrs.Items)=%v", len(nodesToRRs), len(nodes), len(rrs.Items)))
		// if len(nodesToRRs) < (len(nodes) - len(rrs.Items)) {
		// 	log.Error(err, "Some non-rr Nodes doesn't have an active yet RR, requeing CR reconcile")
		// 	return reconcile.Result{Requeue: true}, nil
		// }

		var rrcpods, nonRRcpods []*corev1.Pod
		if rrcpods, nonRRcpods, err = p.getCalicoPods(nodes); err != nil {
			return reconcile.Result{}, err
		}

		// To avoid a network disruption of ~2s on the whole overlay network,
		// the op. disables the BIRD Mesh_ sessions to the Clients of an RR
		// and then all of the sessions to the RRs of a Client
		// i.e. kubectl exec birdcl disable Mesh_

		// FIXME run disable of Client sessions on IKS 1.18 / Calico 3.13 and above only
		if _, err := p.disableMeshSessions(rrcpods, peersOfNode); err != nil {
			log.Error(err, "Failed to disable Mesh_ sessions to Clients")
			return reconcile.Result{}, err
		}

		if _, err := p.disableMeshSessions(nonRRcpods, peersOfNode); err != nil {
			log.Error(err, "Failed to disable Mesh_ sessions to RRs")
			return reconcile.Result{}, err
		}

		// Wait for sessions to reestablish
		time.Sleep(10 * time.Second)

		if success, err := p.verifyMeshSessionsDisabled(nonRRcpods, peersOfNode); err != nil {
			log.Error(err, "Failed to verify RR sessions")
			return reconcile.Result{}, err
		} else if !success {
			log.Info("Some sessions haven't converged yet, requeueing")
			return reconcile.Result{Requeue: true}, nil
		}

		// The full-mesh can be safely teared down now.
		_, dErr := p.disableFullMesh()
		if dErr != nil {
			log.Error(dErr, "Failed to disable full mesh")
		}

	} // End of disable full-mesh workflow

	return reconcile.Result{}, nil
}

// getRRsofNode returns Node object pointers of the RRs of a Node
// BGPPeers are used to resolve the node<>rr mapping
// Return is nil if node doesn't have active RRs
func getRRsofNode(nodes map[*corev1.Node]bool, existingPeers *apiv3.BGPPeerList, node *corev1.Node) (rrs map[*corev1.Node]bool) {
	rrIDs := map[string]bool{}

	for _, bp := range existingPeers.Items {

		// Skip rrs-to-rrs which has "has()" NodeSlector
		if strings.Contains(bp.Name, "rrs-to-rrs") {
			log.Info("Skipping %s", bp.Name)
			continue
		}

		// Get rr-id from PeerSelector in BGPeers where the NodeSelector matches the node
		if keyFieldOfSelector(bp.Spec.NodeSelector) == node.Name {
			peerSelector := keyFieldOfSelector(bp.Spec.PeerSelector)
			log.Info("Found rr-id:%s in BGPPeers:%s as existing RR of Node:%s", peerSelector, bp.Name, node.Name)
			rrIDs[peerSelector] = true
		}
	}

	// Find RR Node pointers by checking their rr-id in the rrIDs map
	for n := range nodes {
		if _, ok := rrIDs[n.GetLabels()[routeReflectorLabel]]; ok {
			log.Info("Found %s as existing RR of Node:%s", n.Name, node.Name)
			if rrs == nil {
				rrs = map[*corev1.Node]bool{}
			}
			rrs[n] = true
		}
	}

	return
}

func getClientsofRR(nodes map[*corev1.Node]bool, existingPeers *apiv3.BGPPeerList, node *corev1.Node) (clients map[*corev1.Node]bool) {
	clientNames := map[string]bool{}

	for _, bp := range existingPeers.Items {

		// Skip rrs-to-rrs which has "has()" NodeSlector
		if strings.Contains(bp.Name, "rrs-to-rrs") {
			log.Info("Skipping %s", bp.Name)
			continue
		}

		// Get clients NodeName from NodeSelector in BGPeers where the PeerSelector matches the RR's rr-id
		if keyFieldOfSelector(bp.Spec.PeerSelector) == node.GetLabels()[routeReflectorLabel] {
			nodeSelector := keyFieldOfSelector(bp.Spec.NodeSelector)
			log.Info("Found node:%s in BGPPeers:%s as existing Client of Node:%s", nodeSelector, bp.Name, node.Name)
			clientNames[nodeSelector] = true
		}
	}

	// Find Client Node pointers by their Name
	for n := range nodes {
		if _, ok := clientNames[n.GetName()]; ok {
			log.Info("Found %s as existing Client of Node:%s", n.Name, node.Name)
			if clients == nil {
				clients = map[*corev1.Node]bool{}
			}
			clients[n] = true
		}
	}

	return
}

func keyFieldOfSelector(s string) (key string) {
	return strings.ReplaceAll(strings.Split(s, "==")[1], "'", "")
}

func (p *reconcileImplParams) createNodeNameToPeersMapping(nodes map[*corev1.Node]bool) (map[string]map[*corev1.Node]bool, error) {
	var err error

	// Fetch existing BGPPeers for getRRsofNode()
	var existingBGPPeers *apiv3.BGPPeerList
	if existingBGPPeers, err = p.bgppeer.ListBGPPeers(); err != nil {
		log.Error(err, "Failed to fetch existing BGPPeers")
		return nil, err
	}
	log.Info("migration", "Number of BGPPeers at migration: %s", len(existingBGPPeers.Items))

	// Finally, getting the Node Name -> Client|RR pointers mapping using getClientsofRR and getRRsofNode
	// Used for constructing birdcl commands

	peersOfNode := map[string]map[*corev1.Node]bool{}

	for n := range nodes {
		if rrs := getRRsofNode(nodes, existingBGPPeers, n); rrs != nil {
			peersOfNode[n.Name] = rrs
		}
	}
	for n := range nodes {
		if clients := getClientsofRR(nodes, existingBGPPeers, n); clients != nil {
			peersOfNode[n.Name] = clients
		}
	}

	return peersOfNode, nil
}

func (p *reconcileImplParams) getCalicoPods(nodes map[*corev1.Node]bool) ([]*corev1.Pod, []*corev1.Pod, error) {
	// 1st: Fetch all calico Pods
	calicoPods := &corev1.PodList{}
	label, _ := labels.Parse("k8s-app=calico-node")
	err := p.client.List(context.TODO(), calicoPods, &client.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return nil, nil, err
	}

	// 2nd: Assort them to RR and non-RR Pods
	rrcpods := []*corev1.Pod{}
	nonRRcpods := []*corev1.Pod{}
	for n := range nodes {
		for j := range calicoPods.Items {
			if calicoPods.Items[j].Spec.NodeName == n.Name {
				if p.topology.IsRouteReflector("", n.GetLabels()) {
					rrcpods = append(rrcpods, &calicoPods.Items[j])
				} else {
					nonRRcpods = append(nonRRcpods, &calicoPods.Items[j])
				}
			}
		}
	}

	return rrcpods, nonRRcpods, nil
}

func (p *reconcileImplParams) isAutoScalerConverged(routereflector *routereflectorv1.RouteReflector) (bool, error) {
	return routereflector.Status.AutoScalerConverged, nil
}

func (p *reconcileImplParams) isFullMeshEnabled() (bool, error) {
	enable := true
	bgpconfig, err := p.calico.BGPConfigurations.Get(context.Background(), "default", options.GetOptions{})
	if _, ok := err.(calicoErrors.ErrorResourceDoesNotExist); err != nil && !ok {
		// Failed to get the BGP configuration (and not because it doesn't exist).
		// Exit.
		log.Error(err, "Failed to query current BGP settings")
		return true, nil
	}

	// Check if NodeToNodeMesh is enabled, if so it's not an RR enabled cluster
	if bgpconfig != nil && bgpconfig.Spec.NodeToNodeMeshEnabled == &enable {
		return true, nil
	} else if bgpconfig == nil {
		// Empty default BGPConfiguration means NodeToNodeMesh is enabled,
		// migration workflow hasn't been executed yet
		return true, nil
	}
	return false, nil
}

func (p *reconcileImplParams) disableMeshSessions(pods []*corev1.Pod, rrNodes map[string]map[*corev1.Node]bool) (bool, error) {
	log.Info(fmt.Sprintf("Disabling Mesh_ sessions in %v non-RR Pods", len(pods)))

	for _, pod := range pods {
		var cmds [][]string
		log.Info(fmt.Sprintf("Disabling Mesh_ session in Pod:%s", pod.Spec.NodeName))
		for rrNode := range rrNodes[pod.Spec.NodeName] {
			cmds = append(cmds, []string{
				"birdcl",
				"-s",
				"/var/run/calico/bird.ctl",
				"disable",
				birdMeshSessionName(rrNode),
			})
			log.Info("migration", "Adding ", birdMeshSessionName(rrNode), "to disable session list of Node:", pod.Spec.NodeName)
		}

		for _, cmd := range cmds {
			log.Info("Execing", "pod", pod, "cmd", cmd)
			_, pErr := p.execOnPod(pod, cmd)
			if pErr != nil {
				log.Error(pErr, "Failed to exec to pod")
				return false, pErr
			}
		} // cmd
	} // pod

	return true, nil
}

// Verifies if Mesh_ sessions to the RRs are in a non Established state
// and Node_ sessions in the Established state
func (p *reconcileImplParams) verifyMeshSessionsDisabled(pods []*corev1.Pod, rrNodes map[string]map[*corev1.Node]bool) (bool, error) {

	// To check whether the session is Established. 1 for yes, 0 for no
	cmd := []string{
		"birdcl",
		"-s",
		"/var/run/calico/bird.ctl",
		"show",
		"protocols",
		// index = 5, replace it with Mesh_x || Node_x
		"",
	}

	var birdOutput string
	var err error

	// log.Info(fmt.Sprintf("Verifying %v-%v Mesh_ and Node_ sessions on %v Nodes", len(rrNodes), len(rrNodes.Items), len(pods.Items)))
	for _, pod := range pods {
		for rrNode := range rrNodes[pod.Spec.NodeName] {
			if pod.Spec.NodeName != rrNode.GetName() {
				log.Info(fmt.Sprintf("Verifying Mesh_ and Node_ sessions of RR:%s in Pod:%s on Node:%s", rrNode.Name, pod.Name, pod.Spec.NodeName))

				// Checking the Mesh_ session to not be Established
				cmd[5] = birdMeshSessionName(rrNode)
				log.Info("Execing", "pod", pod, "cmd", cmd)
				if birdOutput, err = p.execOnPod(pod, cmd); err != nil {
					log.Error(err, fmt.Sprintf("Exec of %s on %s/%s failed", cmd, pod.Spec.NodeName, pod.GetName()))
					return false, err
				} else if strings.Count(birdOutput, "Established") != 0 {
					log.Info(fmt.Sprintf("VERIFY of [Mesh_][%s] ERROR on %s", rrNode.GetName(), pod.Spec.NodeName))
					return false, nil
				}

				// Checking the Node_ session to be Established
				cmd[5] = birdNodeSessionName(rrNode)
				log.Info("Execing", "pod", pod, "cmd", cmd)
				if birdOutput, err = p.execOnPod(pod, cmd); err != nil {
					log.Error(err, fmt.Sprintf("Exec of %s on %s/%s failed", cmd, pod.Spec.NodeName, pod.GetName()))
					return false, err
				} else if strings.Count(birdOutput, "Established") != 1 {
					log.Info(fmt.Sprintf("VERIFY of [Node_][%s] ERROR on %s", rrNode.GetName(), pod.Spec.NodeName))
					return false, nil
				}
			}
		}
	}

	return true, nil
}

func birdMeshSessionName(node *corev1.Node) string {
	return "Mesh_" + strings.Replace(node.GetName(), ".", "_", -1)
}

func birdNodeSessionName(node *corev1.Node) string {
	return "Node_" + strings.Replace(node.GetName(), ".", "_", -1)
}

func (p *reconcileImplParams) execOnPod(pod *corev1.Pod, cmd []string) (string, error) {
	var err error

	var clientset *kubernetes.Clientset
	if clientset, err = kubernetes.NewForConfig(p.rconfig); err != nil {
		log.Error(err, "Failed to get *kubernetes.Clientset")
		return "", err
	}

	req := clientset.CoreV1().RESTClient().Post().
		Namespace("kube-system").
		Resource("pods").
		Name(pod.GetName()).
		SubResource("exec")

	podscheme := runtime.NewScheme()
	if err := corev1.AddToScheme(podscheme); err != nil {
		log.Error(err, "Failed to AddToscheme")
		return "", err
	}
	parameterCodec := runtime.NewParameterCodec(podscheme)
	req.VersionedParams(&corev1.PodExecOptions{
		Command: cmd,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}, parameterCodec)

	var exec remotecommand.Executor
	if exec, err = remotecommand.NewSPDYExecutor(p.rconfig, "POST", req.URL()); err != nil {
		log.Error(err, "Failed to NewSPDYExecutor")
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    true,
	})
	if err != nil {
		log.Error(err, "Failed to exec.Stream")
		return "", err
	}

	return stdout.String() + stderr.String(), nil
}

// Creates the default Calico BGPConfiguration with the nodeToNodeMeshEnable: false option
// or updates the existing one with the same
func (p *reconcileImplParams) disableFullMesh() (bool, error) {
	var err error

	bgpconfig, err := p.calico.BGPConfigurations.Get(context.Background(), "default", options.GetOptions{})
	if _, ok := err.(calicoErrors.ErrorResourceDoesNotExist); err != nil && !ok {
		// Failed to get the BGP configuration (and not because it doesn't exist).
		// Exit.
		log.Error(err, "Failed to query current BGP settings")
		return false, err
	}

	disable := false
	if bgpconfig != nil {
		log.Info("Updating default Calico BGPConfiguration w/ nodeToNodeMeshEnabled: false")
		bgpconfig.Spec.NodeToNodeMeshEnabled = &disable
		if _, err = p.calico.BGPConfigurations.Update(context.Background(), bgpconfig, options.SetOptions{}); err != nil {
			log.Error(err, "Failed to update default Calico BGPConfiguration w/ nodeToNodeMeshEnabled: false")
		}
	} else {
		log.Info("Creating default Calico BGPConfiguration w/ nodeToNodeMeshEnabled: false")
		disable := false
		_, err = p.calico.BGPConfigurations.Create(context.Background(), &apiv3.BGPConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: apiv3.BGPConfigurationSpec{
				NodeToNodeMeshEnabled: &disable,
			},
		}, options.SetOptions{})
		if err != nil {
			log.Error(err, "Failed tp create default BGPConfiguration with 'NodeToNodeMeshEanble: false'")
		}
	}

	return true, nil
}
