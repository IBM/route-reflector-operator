package node

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	clientv3 "github.com/projectcalico/libcalico-go/lib/clientv3"
	calicoErrors "github.com/projectcalico/libcalico-go/lib/errors"
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
	_ = log.WithValues("route-reflector-operator", request.NamespacedName)
	log.Info("Reconciling Node")

	if yes, err := r.isRrEnabled(); err != nil {
		log.Error(err, "Failed to isRrEnabled")
		return reconcile.Result{}, nil
	} else if !yes {
		// Execute migration to RR workflow

		log.Info("route-reflector-operator", "====================================================================", "")
		log.Info("route-reflector-operator", "Calico NodeToNodeMesh is ENABLED, starting migration to RRs workflow", "")
		log.Info("route-reflector-operator", "====================================================================", "")

		// Fetch nodes for RR selection
		nodes := &corev1.NodeList{}
		if err = r.client.List(context.Background(), nodes, &client.ListOptions{}); err != nil {
			log.Error(err, "Failed to fetch NodeList")
			return reconcile.Result{}, nil
		}

		// a calico bgppeers object is created
		r.enableAllToRrSessions()
		// Node_ sessions are added to BIRD
		rrNodes, _ := r.selectRrNodes(nodes)

		// Network disruption on RR nodes as they switch to RR function
		for _, rrNode := range rrNodes {
			r.changeNodeToRr(rrNode)
		}

		// Fetch calico pods to disable Mesh_ sessions
		calicoPods := &corev1.PodList{}
		label, _ := labels.Parse("k8s-app=calico-node")
		err = r.client.List(context.TODO(), calicoPods, &client.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			return reconcile.Result{}, err
		}

		// To avoid a network disruption of 1-2s on the whole overlay network,
		// the op. disables all of the BIRD Mesh_ sessions to the RRs

		// i.e. kubectl exec birdcl disable Mesh_
		if _, err := r.disableMeshSessionsToRrs(calicoPods, rrNodes); err != nil {
			return reconcile.Result{}, err
		}

		// Wait for sessions to reestablish
		time.Sleep(6 * time.Second)

		if success, err := r.verifyMeshSessionsDisabled(calicoPods, rrNodes); err != nil || !success {
			log.Error(err, "Verification of RR sessions failed")
			return reconcile.Result{}, err
		}

		// The full-mesh can be safely teared down now.
		r.disableFullMesh()

	} // End of migration workflow

	// Fetch the Node instance
	instance := &corev1.Node{}
	err := r.client.Get(context.Background(), request.NamespacedName, instance)
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

func (r *ReconcileNode) isRrEnabled() (bool, error) {
	enable := true
	bgpconfig, err := r.calico.BGPConfigurations().Get(context.Background(), "default", options.GetOptions{})
	if _, ok := err.(calicoErrors.ErrorResourceDoesNotExist); err != nil && !ok {
		// Failed to get the BGP configuration (and not because it doesn't exist).
		// Exit.
		log.Error(err, "Failed to query current BGP settings")
		return true, nil
	}

	// Check if NodeToNodeMesh is enabled, if so it's not an RR enabled cluster
	if bgpconfig != nil && bgpconfig.Spec.NodeToNodeMeshEnabled == &enable {
		return false, nil
		// Empty default BGPConfiguration means NodeToNodeMesh is not disable,
		// migration workflow hasn't been executed yet
	} else if bgpconfig == nil {
		return false, nil
	}
	return true, nil
}

func (r *ReconcileNode) selectRrNodes(nodes *corev1.NodeList) ([]*corev1.Node, error) {
	var rrNodes []*corev1.Node
	rrPerZone := getRrPerZone(nodes)

	// FIXME
	// numberOfRRsPerZone := int((3 + len(nodes.Items)/100) / len(rrPerZone))

	var numberOfRRsPerZone int
	switch len(rrPerZone) {
	case 1:
		numberOfRRsPerZone = 3
	case 2:
		numberOfRRsPerZone = 2
	default:
		numberOfRRsPerZone = 1
	}

	for i := range nodes.Items {
		if isLabeled(&nodes.Items[i]) || !isReady(&nodes.Items[i]) || !isSchedulable(&nodes.Items[i]) || isTainted(&nodes.Items[i]) {
			continue
		}

		// Label it if more RRs are needed in the zone
		if rrPerZone[nodes.Items[i].GetLabels()[zoneLabel]] < numberOfRRsPerZone {
			log.Info("route-reflector-operator", "Selecting node for RR", nodes.Items[i].GetName())
			nodes.Items[i].Labels["route-reflector"] = "true"

			// && !errors.IsNotFound(err)
			if err := r.client.Update(context.Background(), &nodes.Items[i], &client.UpdateOptions{}); err != nil {
				log.Error(err, "Failed to update node with label")
				return nil, err
			}

			rrNodes = append(rrNodes, &nodes.Items[i])
			rrPerZone[nodes.Items[i].GetLabels()[zoneLabel]]++
		}
	}

	return rrNodes, nil
}

func getRrPerZone(nodes *corev1.NodeList) map[string]int {
	rrPerZone := map[string]int{}

	for _, node := range nodes.Items {
		if _, ok := rrPerZone[node.GetLabels()[zoneLabel]]; !ok {
			rrPerZone[node.GetLabels()[zoneLabel]] = 0
		}

		if isLabeled(&node) {
			rrPerZone[node.GetLabels()[zoneLabel]]++
		}
	}

	return rrPerZone
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

func (r *ReconcileNode) enableAllToRrSessions() (bool, error) {
	var err error

	bgppeer, err := r.calico.BGPPeers().Get(context.Background(), "peer-with-route-reflectors", options.GetOptions{})
	if _, ok := err.(calicoErrors.ErrorResourceDoesNotExist); err != nil && !ok {
		// Failed to get the BGP peer (and not because it doesn't exist).
		// Exit.
		log.Error(err, "Failed to query current BGP settings")
		return false, err
	}

	log.Info("Creating peer-with-route-reflectors BGPPeers Calico object")
	if bgppeer == nil {
		_, err = r.calico.BGPPeers().Create(context.Background(), &apiv3.BGPPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "peer-with-route-reflectors"},
			Spec: apiv3.BGPPeerSpec{
				NodeSelector: "all()",
				PeerSelector: "route-reflector == 'true'",
			},
		}, options.SetOptions{})

		if err != nil {
			log.Error(err, "Failed to create peer-with-route-reflectors BGPPeers")
		}
	}

	return true, nil
}

func (r *ReconcileNode) disableMeshSessionsToRrs(pods *corev1.PodList, rrNodes []*corev1.Node) (bool, error) {

	var cmds [][]string

	for _, rrNode := range rrNodes {
		cmds = append(cmds, []string{
			"birdcl",
			"-s",
			"/var/run/calico/bird.ctl",
			"disable",
			birdMeshSessionName(rrNode),
		})
	}

	log.Info(fmt.Sprintf("Disabling %v Mesh_ sessions on %v Nodes", len(rrNodes), len(pods.Items)))
	for _, pod := range pods.Items {
		for _, cmd := range cmds {
			r.execOnPod(&pod, cmd)
		} // cmd
	} // pod

	return true, nil
}

// Verifies if Mesh_ sessions to the RRs are in a non Established state
// and Node_ sessions in the Established state
func (r *ReconcileNode) verifyMeshSessionsDisabled(pods *corev1.PodList, rrNodes []*corev1.Node) (bool, error) {

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

	log.Info(fmt.Sprintf("Verifying %v-%v Mesh_ and Node_ sessions on %v Nodes", len(rrNodes), len(rrNodes), len(pods.Items)))
	for _, pod := range pods.Items {
		for _, rrNode := range rrNodes {
			if pod.Spec.NodeName != rrNode.GetName() {

				// Checking the Mesh_ session to not be Established
				cmd[5] = birdMeshSessionName(rrNode)
				if birdOutput, err = r.execOnPod(&pod, cmd); err != nil {
					log.Error(err, fmt.Sprintf("Exec of %s on %s/%s failed", cmd, pod.Spec.NodeName, pod.GetName()))
					return false, err
				} else if strings.Count(birdOutput, "Established") != 0 {
					log.Info(fmt.Sprintf("VERIFY of [Node_][%s] ERROR on %s", rrNode.GetName(), pod.Spec.NodeName))
					return false, nil
				}

				// Checking the Node_ session to be Established
				cmd[5] = birdNodeSessionName(rrNode)
				if birdOutput, err = r.execOnPod(&pod, cmd); err != nil {
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

func (r *ReconcileNode) execOnPod(pod *corev1.Pod, cmd []string) (string, error) {
	var err error

	var clientset *kubernetes.Clientset
	if clientset, err = kubernetes.NewForConfig(r.rconfig); err != nil {
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
	if exec, err = remotecommand.NewSPDYExecutor(r.rconfig, "POST", req.URL()); err != nil {
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

// Add a Cluster ID to Calico node object
// The BIRD sessions are reestablished with the 'rr client' option
// This results in a 1.8-2.2s network disruption on the RR node
func (r *ReconcileNode) changeNodeToRr(node *corev1.Node) (bool, error) {
	if isLabeled(node) {
		var err error
		var cnode *apiv3.Node

		if cnode, err = r.calico.Nodes().Get(context.Background(), node.Labels[workerIDLabel], options.GetOptions{}); err != nil {
			log.Error(err, "Failed to get Calico node object")
			return false, err
		}
		cnode.Spec.BGP.RouteReflectorClusterID = clusterID
		if _, err = r.calico.Nodes().Update(context.Background(), cnode, options.SetOptions{}); err != nil {
			log.Error(err, "Failed to updatee Calico node object with RouteReflectorClusterID")
			return false, err
		}
		log.Info(fmt.Sprintf("Node %s switched to RR function w/ the Cluster ID of %s", node.GetName(), clusterID))
		return true, nil
	}
	return false, nil
}

// Creates the default Calico BGPConfiguration with the nodeToNodeMeshEnable: false option
// or updates the existing one with the same
func (r *ReconcileNode) disableFullMesh() (bool, error) {
	var err error

	bgpconfig, err := r.calico.BGPConfigurations().Get(context.Background(), "default", options.GetOptions{})
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
		if _, err = r.calico.BGPConfigurations().Update(context.Background(), bgpconfig, options.SetOptions{}); err != nil {
			log.Error(err, "Failed to update default Calico BGPConfiguration w/ nodeToNodeMeshEnabled: false")
		}
	} else {
		log.Info("Creating default Calico BGPConfiguration w/ nodeToNodeMeshEnabled: false")
		disable := false
		_, err = r.calico.BGPConfigurations().Create(context.Background(), &apiv3.BGPConfiguration{
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
