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
	perGateTimeout           = 20 * time.Second
	maxMessageLen            = 1024
)

type Server struct {
	kube kubernetes.Interface
	dyn  dynamic.Interface

	// Gate CRD location (configurable)
	gateGVR schema.GroupVersionResource

	// platform-mode cross-namespace allowlist
	platformAllowedNS       map[string]bool
	platformAllowedPrefixes []string

	reconcileInterval time.Duration

	// lastSweep holds the unix time of the last fully successful sweep. It is the
	// only mutable field: written by the reconcile loop, read by /metrics.
	lastSweep atomic.Int64
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
		kube: kube,
		dyn:  dyn,
		gateGVR: schema.GroupVersionResource{
			Group:    envOr("GATEKEEPER_GATE_GROUP", "platform.glueops.dev"),
			Version:  envOr("GATEKEEPER_GATE_VERSION", "v1alpha2"),
			Resource: envOr("GATEKEEPER_GATE_RESOURCE", "gates"),
		},
		// Empty by default: an operator that forgets these env vars gets no
		// cross-namespace access rather than a silently working allowlist.
		platformAllowedNS:       parseCSVSet(os.Getenv("GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES")),
		platformAllowedPrefixes: parseCSVList(os.Getenv("GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES")),
		reconcileInterval:       reconcileInterval(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if s.reconcileInterval > 0 {
		go s.reconcileLoop(ctx)
	} else {
		log.Printf("reconcile loop disabled (GATEKEEPER_RECONCILE_INTERVAL=0)")
	}

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
	return d
}

// --- metrics

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, "# HELP gatekeeper_last_successful_sweep_timestamp_seconds Unix time of the last sweep in which every Gate was evaluated and its status written.\n")
	fmt.Fprint(w, "# TYPE gatekeeper_last_successful_sweep_timestamp_seconds gauge\n")
	fmt.Fprintf(w, "gatekeeper_last_successful_sweep_timestamp_seconds %d\n", s.lastSweep.Load())
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

// reconcileOnce evaluates every Gate in the cluster and records the sweep time if
// all of them were written successfully.
func (s *Server) reconcileOnce(ctx context.Context) {
	list, err := s.dyn.Resource(s.gateGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("sweep: list gates: %v", err)
		return
	}

	// Many Gates share a namespace, so resolve each namespace's mode once per sweep.
	modes := map[string]string{}
	failed := 0

	for i := range list.Items {
		gate := &list.Items[i]
		if err := s.reconcileGate(ctx, gate, modes); err != nil {
			// A Gate deleted mid-sweep is not a failure.
			if apierrors.IsNotFound(err) {
				continue
			}
			failed++
			log.Printf("sweep: gate %s/%s: %v", gate.GetNamespace(), gate.GetName(), err)
		}
	}

	if failed == 0 {
		s.lastSweep.Store(time.Now().Unix())
	}
	log.Printf("sweep: %d gates, %d write failures", len(list.Items), failed)
}

func (s *Server) reconcileGate(ctx context.Context, gate *unstructured.Unstructured, modes map[string]string) (err error) {
	// One bad Gate must not take down the sweep.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic evaluating gate: %v", r)
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
			return s.writeStatus(ctx, gate, metav1.ConditionUnknown, reasonEvaluationFailed,
				fmt.Sprintf("namespace mode lookup failed: %v", modeErr), nil)
		}
		mode = resolved
		modes[ns] = mode
	}

	results := s.evaluateGate(ctx, mode, ns, gate)

	// Dropping per-caller SubjectAccessReview means the API server audit log
	// attributes every read to gatekeeper's ServiceAccount; this is the only
	// remaining record of which Gate caused which reads.
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	log.Printf("evaluated gate %s/%s (mode=%s) checks=%s", ns, gate.GetName(), mode, strings.Join(ids, ","))

	status, reason, message := summarize(results)
	return s.writeStatus(ctx, gate, status, reason, message, results)
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
	if s.platformAllowedNS[ns] {
		return true
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
			if resultReason(r) != reason {
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

	return metav1.ConditionTrue, reasonReady, fmt.Sprintf("%d checks passed", len(results))
}

func resultReason(r EvalResult) string {
	if r.Ready {
		return reasonReady
	}
	if r.Reason != "" {
		return r.Reason
	}
	return reasonDependenciesNotReady
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
			if c.Message != "" {
				return false, "job Failed=True: " + c.Message, true
			}
			return false, "job Failed=True", true
		}
	}
	return false, "job not complete yet", false
}

// argoAppReady evaluates an Argo CD Application's health and sync status. A
// Degraded Application is terminal for the same reason a failed Job is: it will
// not recover without a change, so waiting out the timeout achieves nothing.
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
			isTerminal := healthStatus == "Degraded" || healthStatus == "Missing"
			return false, fmt.Sprintf("health status is %q, want Healthy", healthStatus), isTerminal, nil
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

	condStatus := func(want string) (string, string, bool) {
		for _, raw := range conds {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if strOr(c["type"], "") != want {
				continue
			}
			return strOr(c["status"], ""), strOr(c["message"], ""), true
		}
		return "", "", false
	}

	// Stalled is checked before Ready so the message says why, not just "not ready".
	if status, stalledMsg, ok := condStatus("Stalled"); ok && status == "True" {
		if stalledMsg != "" {
			return false, "helmrelease Stalled=True (will not retry until spec changes): " + stalledMsg, true, nil
		}
		return false, "helmrelease Stalled=True (will not retry until spec changes)", true, nil
	}

	status, readyMsg, ok := condStatus("Ready")
	if !ok {
		return false, "no Ready condition present (helmrelease may still be initializing)", false, nil
	}
	if status != "True" {
		if readyMsg != "" {
			return false, fmt.Sprintf("Ready=%s: %s", status, readyMsg), false, nil
		}
		return false, fmt.Sprintf("Ready=%s", status), false, nil
	}
	return true, "helmrelease Ready=True", false, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func countReadyAddressesForService(ctx context.Context, kube kubernetes.Interface, ns, svcName string) (int64, error) {
	lbl := fmt.Sprintf("kubernetes.io/service-name=%s", svcName)
	slices, err := kube.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: lbl})
	if err != nil {
		return 0, err
	}
	var ready int64
	for i := range slices.Items {
		ready += readyAddresses(&slices.Items[i])
	}
	return ready, nil
}

func readyAddresses(es *discoveryv1.EndpointSlice) int64 {
	var n int64
	for _, ep := range es.Endpoints {
		if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
			n += int64(len(ep.Addresses))
		}
	}
	return n
}

// --- status

func (s *Server) writeStatus(ctx context.Context, gate *unstructured.Unstructured,
	status metav1.ConditionStatus, reason, message string, results []EvalResult) error {

	gen := gate.GetGeneration()
	transition := metav1.Now().UTC().Format(time.RFC3339)
	// metav1 semantics: lastTransitionTime marks the last time the status changed,
	// not the last time it was observed. Inherit it unless the status flipped.
	if prevStatus, prevTransition := readyConditionOf(gate); prevStatus == string(status) && prevTransition != "" {
		transition = prevTransition
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

func parseCSVSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func parseCSVList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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
