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
	workerIDLabel                 = "ibm-cloud.kubernetes.io/worker-id"
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

		// Fetch RR nodes
		rrNodes := &corev1.NodeList{}
		label, _ := labels.Parse("calico-route-reflector")
		if err = p.client.List(context.Background(), rrNodes, &client.ListOptions{
			LabelSelector: label,
		}); err != nil {
			log.Error(err, "Failed to fetch NodeList")
			return reconcile.Result{}, nil
		}

		// Fetch calico pods to disable Mesh_ sessions
		calicoPods := &corev1.PodList{}
		label, _ = labels.Parse("k8s-app=calico-node")
		err = p.client.List(context.TODO(), calicoPods, &client.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			return reconcile.Result{}, err
		}

		// To avoid a network disruption of 1-2s on the whole overlay network,
		// the op. disables all of the BIRD Mesh_ sessions to the RRs

		// i.e. kubectl exec birdcl disable Mesh_
		if _, err := p.disableMeshSessionsToRrs(calicoPods, rrNodes); err != nil {
			return reconcile.Result{}, err
		}

		// Wait for sessions to reestablish
		time.Sleep(6 * time.Second)

		if success, err := p.verifyMeshSessionsDisabled(calicoPods, rrNodes); err != nil {
			log.Error(err, "Failed to verify RR sessions")
			return reconcile.Result{}, err
		} else if !success {
			log.Info("Some sessions haven't converged yet, requeueing")
			return reconcile.Result{Requeue: true}, nil
		}

		// The full-mesh can be safely teared down now.
		p.disableFullMesh()

	} // End of disable full-mesh workflow

	return reconcile.Result{}, nil
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
		// Empty default BGPConfiguration means NodeToNodeMesh is not disable,
		// migration workflow hasn't been executed yet
	} else if bgpconfig == nil {
		return true, nil
	}
	return false, nil
}

func (p *reconcileImplParams) disableMeshSessionsToRrs(pods *corev1.PodList, rrNodes *corev1.NodeList) (bool, error) {

	var cmds [][]string

	for _, rrNode := range rrNodes.Items {
		cmds = append(cmds, []string{
			"birdcl",
			"-s",
			"/var/run/calico/bird.ctl",
			"disable",
			birdMeshSessionName(&rrNode),
		})
	}

	log.Info(fmt.Sprintf("Disabling %v Mesh_ sessions on %v Nodes", len(rrNodes.Items), len(pods.Items)))
	for _, pod := range pods.Items {
		for _, cmd := range cmds {
			p.execOnPod(&pod, cmd)
		} // cmd
	} // pod

	return true, nil
}

// Verifies if Mesh_ sessions to the RRs are in a non Established state
// and Node_ sessions in the Established state
func (p *reconcileImplParams) verifyMeshSessionsDisabled(pods *corev1.PodList, rrNodes *corev1.NodeList) (bool, error) {

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

	log.Info(fmt.Sprintf("Verifying %v-%v Mesh_ and Node_ sessions on %v Nodes", len(rrNodes.Items), len(rrNodes.Items), len(pods.Items)))
	for _, pod := range pods.Items {
		for _, rrNode := range rrNodes.Items {
			if pod.Spec.NodeName != rrNode.GetName() {

				// Checking the Mesh_ session to not be Established
				cmd[5] = birdMeshSessionName(&rrNode)
				if birdOutput, err = p.execOnPod(&pod, cmd); err != nil {
					log.Error(err, fmt.Sprintf("Exec of %s on %s/%s failed", cmd, pod.Spec.NodeName, pod.GetName()))
					return false, err
				} else if strings.Count(birdOutput, "Established") != 0 {
					log.Info(fmt.Sprintf("VERIFY of [Node_][%s] ERROR on %s", rrNode.GetName(), pod.Spec.NodeName))
					return false, nil
				}

				// Checking the Node_ session to be Established
				cmd[5] = birdNodeSessionName(&rrNode)
				if birdOutput, err = p.execOnPod(&pod, cmd); err != nil {
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
