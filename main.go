package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	nsModeLabel  = "gatekeeper.platform.glueops.dev/mode" // customer | platform
	modeCustomer = "customer"
	modePlatform = "platform"
	defaultPort  = "8080"

	conditionReady = "Ready"

	// Condition reasons, in the precedence order summarize applies.
	reasonInvalidSpec          = "InvalidSpec"
	reasonPolicyViolation      = "PolicyViolation"
	reasonReadForbidden        = "ReadForbidden"
	reasonTerminal             = "Terminal"
	reasonEvaluationFailed     = "EvaluationFailed"
	reasonDependenciesNotReady = "DependenciesNotReady"
	reasonReady                = "Ready"

	defaultReconcileInterval = 30 * time.Second
	minReconcileInterval     = time.Second
	// Gates are evaluated serially, so this bounds how far a sweep can be dragged
	// out by unresponsive dependencies. Keep it well under the staleness alert's
	// threshold divided by the expected Gate count.
	perGateTimeout = 10 * time.Second
	maxMessageLen  = 1024
)

// The Gate CRD's identity. Not configurable: a different group or plural means a
// different CRD, which means a different crd.yaml and ClusterRole.
var gateGVR = schema.GroupVersionResource{
	Group:    "platform.glueops.dev",
	Version:  "v1alpha2",
	Resource: "gates",
}

type Server struct {
	kube kubernetes.Interface
	dyn  dynamic.Interface

	// Gate CRD location (configurable)
	gateGVR schema.GroupVersionResource

	// platform-mode cross-namespace allowlist
	platformAllowedNS       []string
	platformAllowedPrefixes []string

	reconcileInterval time.Duration

	// Written by the reconcile loop, read by /metrics. These are the only mutable
	// fields; everything else is fixed after main().
	lastSweep     atomic.Int64 // unix time of the last sweep that wrote every Gate
	gatesUnknown  atomic.Int64 // Gates whose last condition was Unknown
	sweepDuration atomic.Int64 // milliseconds the last sweep took
}

type EvalResult struct {
	ID       string `json:"id"`
	Ready    bool   `json:"ready"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Terminal bool   `json:"terminal,omitempty"`
}

func main() {
	_ = godotenv.Load()

	cfg, err := loadKubeConfig()
	if err != nil {
		log.Fatalf("kube config: %v", err)
	}

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	s := &Server{
		kube:    kube,
		dyn:     dyn,
		gateGVR: gateGVR,
		// Empty by default: an operator that forgets these env vars gets no
		// cross-namespace access rather than a silently working allowlist.
		platformAllowedNS:       parseCSV(os.Getenv("GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES")),
		platformAllowedPrefixes: parseCSV(os.Getenv("GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES")),
		reconcileInterval:       reconcileInterval(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go s.reconcileLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/metrics", s.handleMetrics)

	srv := &http.Server{Addr: ":" + envOr("PORT", defaultPort), Handler: mux}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("gatekeeper listening on %s, reconciling every %s", srv.Addr, s.reconcileInterval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func loadKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
		if err != nil {
			return nil, err
		}
	}
	// client-go defaults to 5 QPS / burst 10, which throttles a sweep that reads
	// every check of every Gate and makes sweep duration scale badly.
	cfg.QPS = 50
	cfg.Burst = 100
	return cfg, nil
}

func reconcileInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GATEKEEPER_RECONCILE_INTERVAL"))
	if raw == "" {
		return defaultReconcileInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("warning: bad GATEKEEPER_RECONCILE_INTERVAL %q, using %s: %v", raw, defaultReconcileInterval, err)
		return defaultReconcileInterval
	}
	// Below a second the loop would hammer the API server rather than reconcile;
	// zero or negative would spin it with no pause at all. To stop reconciling,
	// scale the Deployment to zero.
	if d < minReconcileInterval {
		log.Printf("warning: GATEKEEPER_RECONCILE_INTERVAL %q is below the %s floor, using %s",
			raw, minReconcileInterval, defaultReconcileInterval)
		return defaultReconcileInterval
	}
	return d
}

// --- metrics

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	fmt.Fprint(w, "# HELP gatekeeper_last_successful_sweep_timestamp_seconds Unix time of the last sweep that listed the Gates and wrote a status to every one of them.\n")
	fmt.Fprint(w, "# TYPE gatekeeper_last_successful_sweep_timestamp_seconds gauge\n")
	fmt.Fprintf(w, "gatekeeper_last_successful_sweep_timestamp_seconds %d\n", s.lastSweep.Load())

	// A live gatekeeper writing Unknown to everything is a distinct failure from a
	// dead one: the sweeps succeed, but no Gate carries a usable answer and every
	// gated rollout waits. Kept separate so it can be alerted on with its own
	// threshold rather than collapsed into sweep liveness.
	fmt.Fprint(w, "# HELP gatekeeper_gates_unevaluated Gates whose last evaluation could not reach a verdict.\n")
	fmt.Fprint(w, "# TYPE gatekeeper_gates_unevaluated gauge\n")
	fmt.Fprintf(w, "gatekeeper_gates_unevaluated %d\n", s.gatesUnknown.Load())

	// Gates are evaluated serially, so this is what distinguishes "wedged" from
	// "slow" when the staleness alert fires.
	fmt.Fprint(w, "# HELP gatekeeper_sweep_duration_seconds Wall time of the last completed sweep.\n")
	fmt.Fprint(w, "# TYPE gatekeeper_sweep_duration_seconds gauge\n")
	fmt.Fprintf(w, "gatekeeper_sweep_duration_seconds %.3f\n", float64(s.sweepDuration.Load())/1000)
}

// --- reconcile

func (s *Server) reconcileLoop(ctx context.Context) {
	for {
		s.reconcileOnce(ctx)

		// Wait after the sweep rather than between sweep starts, so a slow sweep
		// can never overlap the next one.
		delay := s.reconcileInterval
		if jitter := int64(s.reconcileInterval / 4); jitter > 0 {
			delay += time.Duration(rand.Int63n(jitter))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// reconcileOnce evaluates every Gate in the cluster and publishes the signals
// /metrics exposes.
func (s *Server) reconcileOnce(ctx context.Context) {
	started := time.Now()

	list, err := s.dyn.Resource(s.gateGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("sweep: list gates: %v", err)
		return
	}

	// Many Gates share a namespace, so resolve each namespace's mode once per sweep.
	modes := map[string]string{}
	var wrote, failed, unknown int

	for i := range list.Items {
		gate := &list.Items[i]
		status, err := s.reconcileGate(ctx, gate, modes)
		if err != nil {
			// A Gate deleted mid-sweep is not a failure. This can't be used to
			// infer that the sweep worked, though: a CRD installed without its
			// status subresource 404s every patch, which would otherwise look
			// like a cluster where every Gate was deleted at once. The wrote > 0
			// requirement below is what catches that.
			if !apierrors.IsNotFound(err) {
				failed++
				log.Printf("sweep: gate %s/%s: %v", gate.GetNamespace(), gate.GetName(), err)
			}
			continue
		}
		wrote++
		if status == metav1.ConditionUnknown {
			unknown++
		}
	}

	s.gatesUnknown.Store(int64(unknown))
	s.sweepDuration.Store(time.Since(started).Milliseconds())

	if failed == 0 && (wrote > 0 || len(list.Items) == 0) {
		s.lastSweep.Store(time.Now().Unix())
	}
	log.Printf("sweep: %d gates, %d written, %d unevaluated, %d failed, %s",
		len(list.Items), wrote, unknown, failed, time.Since(started).Round(time.Millisecond))
}

// reconcileGate evaluates one Gate and writes its status, returning the condition
// status it published.
func (s *Server) reconcileGate(ctx context.Context, gate *unstructured.Unstructured, modes map[string]string) (status metav1.ConditionStatus, err error) {
	// One bad Gate must not take down the sweep.
	defer func() {
		if r := recover(); r != nil {
			status, err = metav1.ConditionUnknown, fmt.Errorf("panic evaluating gate: %v", r)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, perGateTimeout)
	defer cancel()

	ns := gate.GetNamespace()
	mode, cached := modes[ns]
	if !cached {
		resolved, modeErr := s.namespaceMode(ctx, ns)
		if modeErr != nil {
			// Policy is undecidable, so make no claim about readiness rather than
			// defaulting to a mode and evaluating under the wrong rules.
			return metav1.ConditionUnknown, s.writeStatus(ctx, gate, metav1.ConditionUnknown,
				reasonEvaluationFailed, fmt.Sprintf("namespace mode lookup failed: %v", modeErr), nil)
		}
		mode = resolved
		modes[ns] = mode
	}

	results := s.evaluateGate(ctx, mode, ns, gate)

	var reason, message string
	status, reason, message = summarize(results)
	return status, s.writeStatus(ctx, gate, status, reason, message, results)
}

func (s *Server) namespaceMode(ctx context.Context, ns string) (string, error) {
	obj, err := s.kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if strings.ToLower(strings.TrimSpace(obj.Labels[nsModeLabel])) == modePlatform {
		return modePlatform, nil
	}
	return modeCustomer, nil
}

func (s *Server) platformNSAllowed(ns string) bool {
	for _, allowed := range s.platformAllowedNS {
		if ns == allowed {
			return true
		}
	}
	for _, p := range s.platformAllowedPrefixes {
		if strings.HasPrefix(ns, p) {
			return true
		}
	}
	return false
}

// --- evaluation

func (s *Server) evaluateGate(ctx context.Context, mode, gateNS string, gate *unstructured.Unstructured) []EvalResult {
	checks, found, err := unstructured.NestedSlice(gate.Object, "spec", "checks")
	if err != nil || !found {
		return []EvalResult{{ID: "spec", Reason: reasonInvalidSpec, Message: "missing spec.checks"}}
	}

	results := make([]EvalResult, 0, len(checks))
	for i, c := range checks {
		checkMap, ok := c.(map[string]any)
		if !ok {
			results = append(results, EvalResult{
				ID:      fmt.Sprintf("check-%d", i),
				Reason:  reasonInvalidSpec,
				Message: "check item is not an object",
			})
			continue
		}

		id := strOr(checkMap["id"], fmt.Sprintf("check-%d", i))

		if n := countCheckTypes(checkMap); n != 1 {
			results = append(results, EvalResult{
				ID:      id,
				Reason:  reasonInvalidSpec,
				Message: fmt.Sprintf("expected exactly 1 check type, got %d", n),
			})
			continue
		}

		// A check's namespace defaults to the Gate's, not the caller's.
		targetNS := gateNS
		if nsRaw, ok := checkMap["namespace"]; ok {
			if ns := strings.TrimSpace(fmt.Sprint(nsRaw)); ns != "" {
				targetNS = ns
			}
		}

		if targetNS != gateNS {
			if mode != modePlatform {
				results = append(results, EvalResult{
					ID:      id,
					Reason:  reasonPolicyViolation,
					Message: fmt.Sprintf("cross-namespace checks not allowed in customer mode (target=%s)", targetNS),
				})
				continue
			}
			if !s.platformNSAllowed(targetNS) {
				results = append(results, EvalResult{
					ID:      id,
					Reason:  reasonPolicyViolation,
					Message: fmt.Sprintf("target namespace not allowed in platform mode (target=%s)", targetNS),
				})
				continue
			}
		}

		results = append(results, s.evalOne(ctx, targetNS, id, checkMap))
	}

	return results
}

// summarize rolls per-check results up into the Gate's Ready condition. It is the
// only place the condition's status, reason and message are derived.
//
// Reasons are applied in a fixed precedence so the condition names the most
// actionable problem rather than whichever check happened to sort first: a spec
// error the author can fix outranks a policy violation, which outranks
// gatekeeper's own missing RBAC, which outranks a dependency that will never
// become ready, which outranks a dependency we simply could not evaluate.
func summarize(results []EvalResult) (metav1.ConditionStatus, string, string) {
	if len(results) == 0 {
		return metav1.ConditionFalse, reasonInvalidSpec, "gate has no checks"
	}

	for _, reason := range []string{
		reasonInvalidSpec,
		reasonPolicyViolation,
		reasonReadForbidden,
		reasonTerminal,
		reasonEvaluationFailed,
		reasonDependenciesNotReady,
	} {
		for _, r := range results {
			if r.Ready || r.Reason != reason {
				continue
			}
			// "Could not evaluate" is not a claim that the dependency is unready.
			status := metav1.ConditionFalse
			if reason == reasonEvaluationFailed {
				status = metav1.ConditionUnknown
			}
			return status, reason, truncate(fmt.Sprintf("%s: %s", r.ID, r.Message))
		}
	}

	// A not-ready result whose reason isn't in the table above would otherwise fall
	// through to Ready, turning an unrecognised failure into a passing gate. Fail
	// closed instead.
	for _, r := range results {
		if !r.Ready {
			return metav1.ConditionFalse, reasonDependenciesNotReady,
				truncate(fmt.Sprintf("%s: %s", r.ID, r.Message))
		}
	}

	return metav1.ConditionTrue, reasonReady, fmt.Sprintf("%d checks passed", len(results))
}

func countCheckTypes(m map[string]any) int {
	keys := []string{
		"deploymentAvailable",
		"statefulSetReady",
		"jobComplete",
		"serviceReadyEndpoints",
		"podLabelReady",
		"argoApplicationHealthy",
		"fluxHelmReleaseReady",
	}
	n := 0
	for _, k := range keys {
		if _, ok := m[k]; ok {
			n++
		}
	}
	return n
}

func (s *Server) evalOne(ctx context.Context, ns, id string, check map[string]any) EvalResult {
	if block, ok := check["deploymentAvailable"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return specErr(id, "deploymentAvailable.name required")
		}
		minAvail := int64(numOr(block["minAvailableReplicas"], 1))

		d, err := s.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return readErr(id, fmt.Sprintf("deployment %s/%s", ns, name), "apps/deployments", err)
		}
		ready, msg := deploymentReady(d, minAvail)
		return plain(id, ready, msg)
	}

	if block, ok := check["statefulSetReady"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return specErr(id, "statefulSetReady.name required")
		}
		minReady := int64(numOr(block["minReadyReplicas"], 1))
		reqUpd := boolOr(block["requireUpdatedRevision"], true)

		sts, err := s.kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return readErr(id, fmt.Sprintf("statefulset %s/%s", ns, name), "apps/statefulsets", err)
		}
		ready, msg := statefulSetReady(sts, minReady, reqUpd)
		return plain(id, ready, msg)
	}

	if block, ok := check["jobComplete"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return specErr(id, "jobComplete.name required")
		}

		j, err := s.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return readErr(id, fmt.Sprintf("job %s/%s", ns, name), "batch/jobs", err)
		}
		ready, msg, terminal := jobComplete(j)
		return withTerminal(id, ready, msg, terminal)
	}

	if block, ok := check["serviceReadyEndpoints"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return specErr(id, "serviceReadyEndpoints.name required")
		}
		minReady := int64(numOr(block["minReadyAddresses"], 1))

		if _, err := s.kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
			return readErr(id, fmt.Sprintf("service %s/%s", ns, name), "services", err)
		}
		readyAddrs, err := countReadyAddressesForService(ctx, s.kube, ns, name)
		if err != nil {
			return readErr(id, fmt.Sprintf("endpointslices for service %s/%s", ns, name), "discovery.k8s.io/endpointslices", err)
		}
		if readyAddrs >= minReady {
			return plain(id, true, fmt.Sprintf("ready addresses %d >= %d", readyAddrs, minReady))
		}
		return plain(id, false, fmt.Sprintf("ready addresses %d < %d", readyAddrs, minReady))
	}

	if block, ok := check["podLabelReady"].(map[string]any); ok {
		selector := strOr(block["selector"], "")
		if selector == "" {
			return specErr(id, "podLabelReady.selector required")
		}
		minReady := int64(numOr(block["minReadyPods"], 1))

		pods, err := s.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return readErr(id, fmt.Sprintf("pods in %s matching %q", ns, selector), "pods", err)
		}
		readyCount := int64(0)
		for i := range pods.Items {
			if podReady(&pods.Items[i]) {
				readyCount++
			}
		}
		if readyCount >= minReady {
			return plain(id, true, fmt.Sprintf("ready pods %d >= %d", readyCount, minReady))
		}
		return plain(id, false, fmt.Sprintf("ready pods %d < %d", readyCount, minReady))
	}

	if raw, exists := check["argoApplicationHealthy"]; exists {
		block, ok := raw.(map[string]any)
		if !ok {
			return specErr(id, "argoApplicationHealthy must be an object")
		}
		name := strOr(block["name"], "")
		if name == "" {
			return specErr(id, "argoApplicationHealthy.name required")
		}
		requireSynced := boolOr(block["requireSynced"], true)
		requireHealthy := boolOr(block["requireHealthy"], true)

		gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
		app, err := s.dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return readErr(id, fmt.Sprintf("argo application %s/%s", ns, name), "argoproj.io/applications", err)
		}
		ready, msg, terminal, upstreamErr := argoAppReady(app, requireSynced, requireHealthy)
		if upstreamErr != nil {
			// The Application's own status is malformed; that is not the Gate
			// author's mistake, so it must not be reported as InvalidSpec.
			return EvalResult{ID: id, Reason: reasonEvaluationFailed, Message: truncate(upstreamErr.Error())}
		}
		return withTerminal(id, ready, msg, terminal)
	}

	if raw, exists := check["fluxHelmReleaseReady"]; exists {
		block, ok := raw.(map[string]any)
		if !ok {
			return specErr(id, "fluxHelmReleaseReady must be an object")
		}
		name := strOr(block["name"], "")
		if name == "" {
			return specErr(id, "fluxHelmReleaseReady.name required")
		}
		requireCurrentGeneration := boolOr(block["requireCurrentGeneration"], true)

		gvr := schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
		hr, err := s.dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return readErr(id, fmt.Sprintf("helmrelease %s/%s", ns, name), "helm.toolkit.fluxcd.io/helmreleases", err)
		}
		ready, msg, terminal, upstreamErr := fluxHelmReleaseReady(hr, requireCurrentGeneration)
		if upstreamErr != nil {
			return EvalResult{ID: id, Reason: reasonEvaluationFailed, Message: truncate(upstreamErr.Error())}
		}
		return withTerminal(id, ready, msg, terminal)
	}

	return specErr(id, "no recognized check type set")
}

// --- result constructors

func plain(id string, ready bool, msg string) EvalResult {
	r := EvalResult{ID: id, Ready: ready, Message: truncate(msg)}
	if !ready {
		r.Reason = reasonDependenciesNotReady
	} else {
		r.Reason = reasonReady
	}
	return r
}

func withTerminal(id string, ready bool, msg string, terminal bool) EvalResult {
	r := plain(id, ready, msg)
	if terminal {
		r.Terminal = true
		r.Reason = reasonTerminal
	}
	return r
}

func specErr(id, msg string) EvalResult {
	return EvalResult{ID: id, Reason: reasonInvalidSpec, Message: truncate(msg)}
}

// readErr classifies an API read failure. A missing dependency is a normal
// not-ready; a forbidden read means gatekeeper's own ClusterRole is short, which
// an operator must fix; anything else is indeterminate and must not be reported
// as a definite answer, or a single API blip would flip a healthy Gate to False.
func readErr(id, what, rule string, err error) EvalResult {
	switch {
	case apierrors.IsNotFound(err):
		return EvalResult{ID: id, Reason: reasonDependenciesNotReady, Message: truncate(what + " not found")}
	case apierrors.IsForbidden(err):
		return EvalResult{
			ID:      id,
			Reason:  reasonReadForbidden,
			Message: truncate(fmt.Sprintf("gatekeeper cannot read %s: grant %s in its ClusterRole", what, rule)),
		}
	default:
		return EvalResult{ID: id, Reason: reasonEvaluationFailed, Message: truncate(fmt.Sprintf("%s read failed: %v", what, err))}
	}
}

// --- readiness helpers

func deploymentReady(d *appsv1.Deployment, minAvail int64) (bool, string) {
	avail := int64(d.Status.AvailableReplicas)
	if avail < minAvail {
		return false, fmt.Sprintf("availableReplicas %d < %d", avail, minAvail)
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true, "deployment Available=True"
		}
	}
	return false, "deployment Available condition not True"
}

func statefulSetReady(sts *appsv1.StatefulSet, minReady int64, requireUpdated bool) (bool, string) {
	ready := int64(sts.Status.ReadyReplicas)
	if ready < minReady {
		return false, fmt.Sprintf("readyReplicas %d < %d", ready, minReady)
	}
	if requireUpdated {
		if sts.Status.UpdateRevision != "" && sts.Status.CurrentRevision != "" &&
			sts.Status.UpdateRevision != sts.Status.CurrentRevision {
			return false, fmt.Sprintf("revision not fully updated (current=%s update=%s)", sts.Status.CurrentRevision, sts.Status.UpdateRevision)
		}
	}
	return true, "statefulset ready"
}

// jobComplete reports Failed=True as terminal: a failed Job will not become
// complete on its own, so a poller that treats it as "not ready yet" burns its
// whole timeout waiting for something that cannot happen.
func jobComplete(j *batchv1.Job) (ready bool, msg string, terminal bool) {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true, "job Complete=True", false
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, withReason("job Failed=True", c.Reason), true
		}
	}
	return false, "job not complete yet", false
}

// argoAppReady evaluates an Argo CD Application's health and sync status.
//
// Nothing here is terminal. Argo CD's health is a rollup computed from an
// Application's resources, not a statement that its controller has stopped trying:
// Progressing and Degraded both occur mid-rollout, and Missing is simply the state
// of an Application whose resources are not live yet, which is exactly what a Gate
// guarding a not-yet-bootstrapped platform stack should wait for. Reporting any of
// them as terminal would fail the gated sync — and because a Gate is re-evaluated
// on every sync, a Degraded dependency would keep failing it.
func argoAppReady(app *unstructured.Unstructured, requireSynced, requireHealthy bool) (ready bool, msg string, terminal bool, err error) {
	if requireHealthy {
		healthStatus, found, nestedErr := unstructured.NestedString(app.Object, "status", "health", "status")
		if nestedErr != nil {
			return false, "", false, fmt.Errorf("status.health.status has unexpected type: %w", nestedErr)
		}
		if !found {
			return false, "status.health.status not present (application may still be initializing)", false, nil
		}
		if healthStatus != "Healthy" {
			return false, fmt.Sprintf("health status is %q, want Healthy", healthStatus), false, nil
		}
	}

	if requireSynced {
		syncStatus, found, nestedErr := unstructured.NestedString(app.Object, "status", "sync", "status")
		if nestedErr != nil {
			return false, "", false, fmt.Errorf("status.sync.status has unexpected type: %w", nestedErr)
		}
		if !found {
			return false, "status.sync.status not present (application may still be initializing)", false, nil
		}
		if syncStatus != "Synced" {
			return false, fmt.Sprintf("sync status is %q, want Synced", syncStatus), false, nil
		}
	}

	if !requireHealthy && !requireSynced {
		return true, "no conditions required", false, nil
	}

	var parts []string
	if requireHealthy {
		parts = append(parts, "Healthy")
	}
	if requireSynced {
		parts = append(parts, "Synced")
	}
	return true, fmt.Sprintf("argo application %s", strings.Join(parts, " and ")), false, nil
}

// fluxHelmReleaseReady evaluates a Flux helm.toolkit.fluxcd.io/v2 HelmRelease.
//
// Why this exists rather than pointing deploymentAvailable at the workload the
// HelmRelease manages: a Deployment check reads whatever is currently in the cluster,
// so during a failed upgrade it happily reports Ready against the *previous* revision's
// pods while the release behind them is broken. That is a false green on precisely the
// failure this check is for.
//
// Two conditions are evaluated, in this order:
//
//  1. Generation currency. helm-controller writes status.observedGeneration when it has
//     acted on the current spec. If it lags metadata.generation the Ready condition
//     describes an older revision and must not be trusted. Flux uses -1 for "never
//     reconciled". Callers that genuinely want the last-known state regardless can set
//     requireCurrentGeneration: false.
//
//  2. Stalled. When helm-controller exhausts spec.upgrade.remediation.retries it sets
//     Stalled=True and stops retrying until the spec, values or chart version change --
//     an interval tick does not clear it. This is reported as terminal, because the two
//     need different responses: not-Ready means keep polling, Stalled means a human has
//     to run `flux reconcile helmrelease <name> --reset`. A poller that cannot tell them
//     apart burns its whole timeout on a release that was never going to recover.
func fluxHelmReleaseReady(hr *unstructured.Unstructured, requireCurrentGeneration bool) (ready bool, msg string, terminal bool, err error) {
	if requireCurrentGeneration {
		gen, genFound, nestedErr := unstructured.NestedInt64(hr.Object, "metadata", "generation")
		if nestedErr != nil {
			return false, "", false, fmt.Errorf("metadata.generation has unexpected type: %w", nestedErr)
		}
		observed, obsFound, nestedErr := unstructured.NestedInt64(hr.Object, "status", "observedGeneration")
		if nestedErr != nil {
			return false, "", false, fmt.Errorf("status.observedGeneration has unexpected type: %w", nestedErr)
		}
		if !obsFound || observed < 0 {
			return false, "status.observedGeneration not set (helmrelease not yet reconciled)", false, nil
		}
		if genFound && observed < gen {
			return false, fmt.Sprintf("status.observedGeneration %d behind metadata.generation %d (reconcile in progress)", observed, gen), false, nil
		}
	}

	conds, found, nestedErr := unstructured.NestedSlice(hr.Object, "status", "conditions")
	if nestedErr != nil {
		return false, "", false, fmt.Errorf("status.conditions has unexpected type: %w", nestedErr)
	}
	if !found || len(conds) == 0 {
		return false, "status.conditions not present (helmrelease may still be initializing)", false, nil
	}

	// Returns the condition's status and reason. Deliberately not its message: see
	// the note above about not re-publishing upstream text.
	condStatus := func(want string) (status, reason string, found bool) {
		for _, raw := range conds {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if strOr(c["type"], "") != want {
				continue
			}
			return strOr(c["status"], ""), strOr(c["reason"], ""), true
		}
		return "", "", false
	}

	// Stalled is checked before Ready so the message says why, not just "not ready".
	if status, reason, ok := condStatus("Stalled"); ok && status == "True" {
		return false, withReason("helmrelease Stalled=True (will not retry until spec changes)", reason), true, nil
	}

	status, reason, ok := condStatus("Ready")
	if !ok {
		return false, "no Ready condition present (helmrelease may still be initializing)", false, nil
	}
	if status != "True" {
		return false, withReason(fmt.Sprintf("Ready=%s", status), reason), false, nil
	}
	return true, "helmrelease Ready=True", false, nil
}

// withReason appends an upstream condition's reason, which controllers draw from a
// bounded vocabulary. Upstream *messages* are not appended: they wrap arbitrary
// third-party text (a Helm error quoting an admission webhook, which may quote the
// rejected object's field values), and a Gate's status is readable by anyone who
// can read its namespace and is re-published into the Argo CD UI.
func withReason(msg, reason string) string {
	if reason == "" {
		return msg
	}
	return msg + " (" + reason + ")"
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// countReadyAddressesForService counts the ready backends behind a Service.
//
// The EndpointSlice controller writes one slice per addressType, so a dual-stack
// Service has an IPv4 slice and an IPv6 slice describing the *same* pods. Summing
// across slices would therefore count every backend once per address family and
// satisfy minReadyAddresses with half the pods. Taking the maximum across families
// is correct on IPv4-only, IPv6-only and dual-stack clusters without hard-coding a
// family. FQDN slices are ignored: the EndpointSlice controller doesn't produce
// them for selector-based Services, so one appearing means something else wrote it
// and letting it win the maximum would change what the count means.
func countReadyAddressesForService(ctx context.Context, kube kubernetes.Interface, ns, svcName string) (int64, error) {
	lbl := fmt.Sprintf("kubernetes.io/service-name=%s", svcName)
	slices, err := kube.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: lbl})
	if err != nil {
		return 0, err
	}

	perFamily := map[discoveryv1.AddressType]int64{}
	for i := range slices.Items {
		es := &slices.Items[i]
		if es.AddressType != discoveryv1.AddressTypeIPv4 && es.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}
		perFamily[es.AddressType] += readyEndpoints(es)
	}

	var ready int64
	for _, n := range perFamily {
		if n > ready {
			ready = n
		}
	}
	return ready, nil
}

// readyEndpoints counts ready endpoints, not addresses. Endpoints[].Addresses is
// the address list for a single endpoint — upstream requires at least one entry
// and tells consumers to use the first — so summing its length would count one pod
// as several.
func readyEndpoints(es *discoveryv1.EndpointSlice) int64 {
	var n int64
	for _, ep := range es.Endpoints {
		// A nil Ready is "ready" per upstream, but controller-managed slices always
		// set it, and failing closed is the right direction for a gate.
		if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
			n++
		}
	}
	return n
}

// --- status

func (s *Server) writeStatus(ctx context.Context, gate *unstructured.Unstructured,
	status metav1.ConditionStatus, reason, message string, results []EvalResult) error {

	gen := gate.GetGeneration()
	prevStatus, prevTransition := readyConditionOf(gate)

	transition := metav1.Now().UTC().Format(time.RFC3339)
	// metav1 semantics: lastTransitionTime marks the last time the status changed,
	// not the last time it was observed. Inherit it unless the status flipped.
	if prevStatus == string(status) && prevTransition != "" {
		transition = prevTransition
	}

	// Log transitions only. Every sweep re-evaluating every Gate makes per-sweep
	// logging pure volume, while the question worth answering later is when a Gate
	// changed and why — which status.results overwrites.
	if prevStatus != string(status) {
		log.Printf("gate %s/%s: %s -> %s (%s) %s",
			gate.GetNamespace(), gate.GetName(), orNone(prevStatus), status, reason, message)
	}

	patch := map[string]any{
		"status": map[string]any{
			"observedGeneration": gen,
			"conditions": []any{map[string]any{
				"type":               conditionReady,
				"status":             string(status),
				"reason":             reason,
				"message":            message,
				"lastTransitionTime": transition,
				"observedGeneration": gen,
			}},
			"results": resultsToUnstructured(results),
		},
	}

	b, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}

	_, err = s.dyn.Resource(s.gateGVR).Namespace(gate.GetNamespace()).Patch(
		ctx, gate.GetName(), types.MergePatchType, b, metav1.PatchOptions{}, "status",
	)
	return err
}

func readyConditionOf(gate *unstructured.Unstructured) (status, lastTransitionTime string) {
	conds, found, err := unstructured.NestedSlice(gate.Object, "status", "conditions")
	if err != nil || !found {
		return "", ""
	}
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strOr(c["type"], "") != conditionReady {
			continue
		}
		return strOr(c["status"], ""), strOr(c["lastTransitionTime"], "")
	}
	return "", ""
}

func resultsToUnstructured(in []EvalResult) []any {
	out := make([]any, 0, len(in))
	for _, r := range in {
		item := map[string]any{
			"id":      r.ID,
			"ready":   r.Ready,
			"reason":  r.Reason,
			"message": r.Message,
		}
		if r.Terminal {
			item["terminal"] = true
		}
		out = append(out, item)
	}
	return out
}

// --- small helpers

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func truncate(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen-3] + "..."
}

// parseCSV drops empty entries, which matters for the allowlists: a trailing comma
// must not produce an empty prefix, since strings.HasPrefix(ns, "") matches every
// namespace.
func parseCSV(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func strOr(v any, def string) string {
	if v == nil {
		return def
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" {
		return def
	}
	return s
}

func numOr(v any, def int) int {
	if v == nil {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return def
	}
}

func boolOr(v any, def bool) bool {
	if v == nil {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}
