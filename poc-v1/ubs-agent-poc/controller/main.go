// ubs-agent-controller is a POC controller that implements version-managed
// agent deployments on top of *native* Kubernetes objects only -- no CRDs.
//
// Convention (this is the entire "API" a developer/CI-CD needs to know):
//
//	Every agent Deployment carries two labels:
//	  ubs.io/blueprint-id : the UBS blueprint this agent belongs to
//	  ubs.io/agent-name   : logical agent name, stable across versions
//
//	To ship a new version, CI/CD applies a NEW Deployment object with the
//	SAME two labels (Deployment *name* can/should differ, e.g. by including
//	the image tag or a build number -- this is a completely standard
//	CI/CD templating pattern, nothing bespoke).
//
// The controller:
//  1. Watches Deployments carrying ubs.io/blueprint-id.
//  2. Groups them by (namespace, blueprint-id, agent-name).
//  3. If a group has exactly one Deployment -> first-ever deploy: mint an
//     agent ID via the Graph API, wait for it to be Ready, register it.
//  4. If a group has two Deployments -> a version transition is in flight.
//     The older (by CreationTimestamp) is "old", the newer is "incoming".
//     Once "incoming" is Ready, mint+register its agent ID, THEN deregister
//     the old agent ID, THEN delete the old Deployment. Old is never
//     touched until incoming is fully verified and registered.
//
// State that must survive controller restarts is NOT kept in-memory or in
// a CRD/ConfigMap -- it is recomputed every reconcile from two sources of
// truth: (a) live Kubernetes Deployment objects, and (b) the UBS Blueprint
// API. The only controller-owned bookkeeping is a single annotation,
// ubs.io/agent-id, written onto a Deployment *after* that Deployment's
// agent ID is successfully registered -- so we always know which live
// object corresponds to which registered agent without inventing a second
// database.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue"
)

const (
	labelBlueprintID  = "ubs.io/blueprint-id"
	labelAgentName    = "ubs.io/agent-name"
	annotationAgentID = "ubs.io/agent-id"
)

// ---------------------------------------------------------------------------
// UBS / Graph API client (talks to the mock-api service in this POC; in a
// real deployment this would point at UBS's actual internal endpoints and
// carry the blueprint token read from the Deployment's referenced Secret).
// ---------------------------------------------------------------------------

type ubsClient struct {
	baseURL string
	http    *http.Client
}

func newUBSClient(baseURL string) *ubsClient {
	return &ubsClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

type blueprintState struct {
	BlueprintID string `json:"blueprintId"`
	AgentID     string `json:"agentId"`
	Image       string `json:"image"`
}

func (c *ubsClient) getBlueprint(blueprintID string) (*blueprintState, error) {
	resp, err := c.http.Get(fmt.Sprintf("%s/blueprint/%s", c.baseURL, blueprintID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var state blueprintState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (c *ubsClient) mintAgentID() (string, error) {
	resp, err := c.http.Post(fmt.Sprintf("%s/graph/agents", c.baseURL), "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AgentID string `json:"agentId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AgentID, nil
}

func (c *ubsClient) registerAgent(blueprintID, agentID, image string) error {
	payload, _ := json.Marshal(map[string]string{"agentId": agentID, "image": image})
	resp, err := c.http.Post(fmt.Sprintf("%s/blueprint/%s/register", c.baseURL, blueprintID), "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("register failed: status %d", resp.StatusCode)
	}
	return nil
}

func (c *ubsClient) deregisterAgent(agentID string) error {
	resp, err := c.http.Post(fmt.Sprintf("%s/agents/%s/deregister", c.baseURL, agentID), "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("deregister failed: status %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Controller
// ---------------------------------------------------------------------------

type controller struct {
	kube      kubernetes.Interface
	ubs       *ubsClient
	queue     workqueue.RateLimitingInterface
	lister    cache.Indexer
	namespace string
}

// groupKey identifies one logical agent lineage: a single blueprint's single
// agent, across however many Deployment versions currently exist for it.
type groupKey struct {
	namespace   string
	blueprintID string
	agentName   string
}

func (g groupKey) String() string {
	return fmt.Sprintf("%s/%s/%s", g.namespace, g.blueprintID, g.agentName)
}

func main() {
	var kubeconfig, mockAPI, namespace string
	if home := homedir.HomeDir(); home != "" {
		flag.StringVar(&kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "path to kubeconfig (out-of-cluster only)")
	}
	flag.StringVar(&mockAPI, "ubs-api", "http://localhost:5000", "base URL of the (mock) UBS/Graph API")
	flag.StringVar(&namespace, "namespace", "", "namespace to watch (empty = all namespaces)")
	flag.Parse()

	kubeClient, err := buildKubeClient(kubeconfig)
	if err != nil {
		log.Fatalf("failed to build kube client: %v", err)
	}

	c := &controller{
		kube:      kubeClient,
		ubs:       newUBSClient(mockAPI),
		queue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		namespace: namespace,
	}
	c.run()
}

func buildKubeClient(kubeconfigPath string) (kubernetes.Interface, error) {
	// Try in-cluster config first (this is how it runs in a real UK8s deployment,
	// where the controller runs as its own pod with a ServiceAccount).
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	// Fall back to local kubeconfig (this is how you run it against kind on a laptop).
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func (c *controller) run() {
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.kube,
		30*time.Second,
		informers.WithNamespace(c.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = blueprintLabelSelector()
		}),
	)
	deployInformer := factory.Apps().V1().Deployments()
	c.lister = deployInformer.Informer().GetIndexer()

	deployInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueFor(obj) },
		UpdateFunc: func(_, newObj interface{}) { c.enqueueFor(newObj) },
		DeleteFunc: func(obj interface{}) { c.enqueueFor(obj) },
	})

	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	factory.WaitForCacheSync(stop)

	log.Println("ubs-agent-controller started, watching Deployments labeled", labelBlueprintID)

	for c.processNextItem() {
	}
}

func (c *controller) enqueueFor(obj interface{}) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return
	}
	bp, hasBP := d.Labels[labelBlueprintID]
	agent, hasAgent := d.Labels[labelAgentName]
	if !hasBP || !hasAgent {
		return
	}
	key := groupKey{namespace: d.Namespace, blueprintID: bp, agentName: agent}
	c.queue.Add(key)
}

func (c *controller) processNextItem() bool {
	item, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(item)

	key := item.(groupKey)
	if err := c.reconcile(key); err != nil {
		log.Printf("[%s] reconcile error: %v (will retry)", key, err)
		c.queue.AddRateLimited(item)
		return true
	}
	c.queue.Forget(item)
	return true
}

// reconcile implements the phase logic described in the package doc comment.
func (c *controller) reconcile(key groupKey) error {
	deployments, err := c.listGroupDeployments(key)
	if err != nil {
		return err
	}
	if len(deployments) == 0 {
		return nil // nothing to do, likely just processed a delete
	}

	sort.Slice(deployments, func(i, j int) bool {
		return deployments[i].CreationTimestamp.Before(&deployments[j].CreationTimestamp)
	})

	switch len(deployments) {
	case 1:
		return c.reconcileFirstDeploy(key, deployments[0])
	case 2:
		return c.reconcileTransition(key, deployments[0], deployments[1])
	default:
		log.Printf("[%s] WARNING: %d deployments found for one agent lineage, expected 1 or 2 -- "+
			"skipping automated action, needs manual investigation", key, len(deployments))
		return nil
	}
}

func (c *controller) listGroupDeployments(key groupKey) ([]*appsv1.Deployment, error) {
	all, err := c.kube.AppsV1().Deployments(key.namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", labelBlueprintID, key.blueprintID, labelAgentName, key.agentName),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*appsv1.Deployment, 0, len(all.Items))
	for i := range all.Items {
		out = append(out, &all.Items[i])
	}
	return out, nil
}

// reconcileFirstDeploy handles the case where exactly one Deployment exists
// for this agent lineage -- either it's brand new, or it's the steady state
// after a previous successful transition.
func (c *controller) reconcileFirstDeploy(key groupKey, d *appsv1.Deployment) error {
	if d.Annotations[annotationAgentID] != "" {
		// Already registered in a previous reconcile. Steady state, nothing to do.
		return nil
	}
	if !isReady(d) {
		log.Printf("[%s] deployment %s not yet Ready, waiting", key, d.Name)
		return nil // will be re-triggered by the next Update event when it becomes Ready
	}

	image := containerImage(d)
	log.Printf("[%s] first deploy detected (%s), minting agent id", key, d.Name)

	agentID, err := c.ubs.mintAgentID()
	if err != nil {
		return fmt.Errorf("mint agent id: %w", err)
	}
	if err := c.ubs.registerAgent(key.blueprintID, agentID, image); err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	if err := c.patchAgentIDAnnotation(d, agentID); err != nil {
		return fmt.Errorf("annotate deployment: %w", err)
	}
	log.Printf("[%s] agent %s registered and live as v1 (%s)", key, agentID, d.Name)
	return nil
}

// reconcileTransition handles the case where two Deployments exist for the
// same agent lineage: "old" (already registered, currently serving) and
// "incoming" (the new version CI/CD just applied). Old is guaranteed to
// remain fully untouched until incoming is Ready AND successfully registered.
func (c *controller) reconcileTransition(key groupKey, old, incoming *appsv1.Deployment) error {
	if incoming.Annotations[annotationAgentID] != "" {
		// incoming already has an agent id -- meaning register() already
		// succeeded on a prior reconcile but we crashed/retried before
		// finishing cleanup. Resume from the deregister+delete step.
		return c.retireOld(key, old, incoming)
	}

	if !isReady(incoming) {
		log.Printf("[%s] incoming deployment %s not yet Ready -- old deployment %s left fully untouched",
			key, incoming.Name, old.Name)
		return nil
	}

	image := containerImage(incoming)
	log.Printf("[%s] incoming deployment %s is Ready, minting new agent id (old %s still fully live)",
		key, incoming.Name, old.Name)

	newAgentID, err := c.ubs.mintAgentID()
	if err != nil {
		return fmt.Errorf("mint agent id: %w", err)
	}
	if err := c.ubs.registerAgent(key.blueprintID, newAgentID, image); err != nil {
		// Old was NEVER touched. Safe to just retry later -- no partial state
		// to unwind on the Kubernetes side.
		return fmt.Errorf("register new agent: %w", err)
	}
	if err := c.patchAgentIDAnnotation(incoming, newAgentID); err != nil {
		return fmt.Errorf("annotate incoming deployment: %w", err)
	}
	log.Printf("[%s] new agent %s registered and now the active agent for blueprint %s",
		key, newAgentID, key.blueprintID)

	return c.retireOld(key, old, incoming)
}

// retireOld deregisters and deletes the old Deployment. By the time this is
// called, the new agent is already confirmed Ready and successfully
// registered -- this function is the ONLY place in the whole controller
// that deletes an agent Deployment.
func (c *controller) retireOld(key groupKey, old, incoming *appsv1.Deployment) error {
	oldAgentID := old.Annotations[annotationAgentID]
	if oldAgentID == "" {
		log.Printf("[%s] WARNING: old deployment %s has no recorded agent id, skipping deregister call "+
			"but still cleaning up the Kubernetes object", key, old.Name)
	} else {
		if err := c.ubs.deregisterAgent(oldAgentID); err != nil {
			// New agent is already live+registered and serving traffic, so
			// this is a non-fatal cleanup failure, not a rollout failure.
			// Surface it loudly and retry -- do NOT delete the old
			// Deployment until deregister is confirmed.
			return fmt.Errorf("deregister old agent %s (will retry, old deployment left running for now): %w", oldAgentID, err)
		}
		log.Printf("[%s] old agent %s deregistered", key, oldAgentID)
	}

	if err := c.kube.AppsV1().Deployments(key.namespace).Delete(context.TODO(), old.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete old deployment %s: %w", old.Name, err)
	}
	log.Printf("[%s] old deployment %s deleted -- exactly one version (%s) now running for this agent",
		key, old.Name, incoming.Name)
	return nil
}

func (c *controller) patchAgentIDAnnotation(d *appsv1.Deployment, agentID string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, annotationAgentID, agentID))
	_, err := c.kube.AppsV1().Deployments(d.Namespace).Patch(
		context.TODO(), d.Name, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	return err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func isReady(d *appsv1.Deployment) bool {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return d.Status.ReadyReplicas >= desired && desired > 0
}

func containerImage(d *appsv1.Deployment) string {
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return d.Spec.Template.Spec.Containers[0].Image
}

// blueprintLabelSelector returns "ubs.io/blueprint-id" (a bare "key exists"
// selector) so the informer only ever caches Deployments that opted into
// this controller's management, regardless of which blueprint/agent they
// belong to.
func blueprintLabelSelector() string {
	req, err := labels.NewRequirement(labelBlueprintID, selection.Exists, nil)
	if err != nil {
		panic(err) // static string, can never actually fail
	}
	return labels.NewSelector().Add(*req).String()
}
