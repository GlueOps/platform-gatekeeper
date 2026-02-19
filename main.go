package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"github.com/joho/godotenv"
)

const (
	nsModeLabel  = "gatekeeper.platform.onglueopshosted.com/mode" // customer | platform
	modeCustomer = "customer"
	modePlatform = "platform"
	defaultPort  = "8080"
)

type Server struct {
	kube kubernetes.Interface
	dyn  dynamic.Interface

	// Gate CRD location (configurable)
	gateGVR schema.GroupVersionResource

	// platform-mode cross-namespace allowlist
	platformAllowedNS       map[string]bool
	platformAllowedPrefixes []string
}

type Caller struct {
	Token  string
	User   string
	Groups []string
	Extra  map[string]authv1.ExtraValue
	NS     string // derived from user: system:serviceaccount:<ns>:<name>
}

type EvalResult struct {
	ID      string `json:"id"`
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

type EvalResponse struct {
	Gate      string       `json:"gate"`
	Namespace string       `json:"namespace"` // the Gate namespace (not necessarily caller namespace in platform mode)
	Ready     bool         `json:"ready"`
	Mode      string       `json:"mode"`
	Results   []EvalResult `json:"results"`
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

	gvr := schema.GroupVersionResource{
		Group:    envOr("GATEKEEPER_GATE_GROUP", "platform.onglueopshosted.com"),
		Version:  envOr("GATEKEEPER_GATE_VERSION", "v1alpha1"),
		Resource: envOr("GATEKEEPER_GATE_RESOURCE", "gates"),
	}

	allowed := parseCSVSet(envOr("GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES", "glueops-core,nonprod"))
	prefixes := parseCSVList(envOr("GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES", "glueops-core-"))

	s := &Server{
		kube:                   kube,
		dyn:                    dyn,
		gateGVR:                gvr,
		platformAllowedNS:      allowed,
		platformAllowedPrefixes: prefixes,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/check", s.handleCheck(false))
	mux.HandleFunc("/explain", s.handleCheck(true))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	addr := ":" + envOr("PORT", defaultPort)
	log.Printf("gatekeeper listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func loadKubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
}

func (s *Server) handleCheck(always200 bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		caller, err := s.authenticate(ctx, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		gateName := strings.TrimSpace(r.URL.Query().Get("gate"))
		if gateName == "" {
			http.Error(w, "missing ?gate=", http.StatusBadRequest)
			return
		}

		// Determine namespace mode from the caller namespace label
		mode, err := s.namespaceMode(ctx, caller.NS)
		if err != nil {
			http.Error(w, fmt.Sprintf("namespace mode lookup: %v", err), http.StatusBadGateway)
			return
		}

		// Gate namespace can be overridden in platform mode via ?ns=
		gateNS := strings.TrimSpace(r.URL.Query().Get("ns"))
		if gateNS == "" {
			gateNS = caller.NS
		}

		// Policy for Gate lookup namespace
		if mode == modeCustomer && gateNS != caller.NS {
			http.Error(w, "cross-namespace gate lookup not allowed in customer mode", http.StatusForbidden)
			return
		}
		if mode == modePlatform && gateNS != caller.NS && !s.platformNSAllowed(gateNS) {
			http.Error(w, fmt.Sprintf("gate namespace not allowed in platform mode (ns=%s)", gateNS), http.StatusForbidden)
			return
		}

		// Delegated authz: caller must be allowed to GET the Gate object in gateNS
		if err := s.requireSAR(ctx, caller, gateNS, s.gateGVR.Group, s.gateGVR.Resource, "get", gateName); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		// Load Gate in gateNS
		gateObj, err := s.dyn.Resource(s.gateGVR).Namespace(gateNS).Get(ctx, gateName, metav1.GetOptions{})
		if err != nil {
			http.Error(w, fmt.Sprintf("gate not found: %v", err), http.StatusNotFound)
			return
		}

		results, ready, evalErr := s.evaluateGate(ctx, caller, mode, gateNS, gateObj)

		resp := EvalResponse{
			Gate:      gateName,
			Namespace: gateNS,
			Ready:     ready,
			Mode:      mode,
			Results:   results,
		}

		// Update Gate.status best-effort (does not change response)
		_ = s.updateGateStatus(ctx, gateNS, gateObj, ready, results)

		// Return
		if evalErr != nil {
			// policy/authz/spec errors should be clear to operator
			http.Error(w, evalErr.Error(), http.StatusForbidden)
			return
		}

		status := http.StatusOK
		if !always200 && !ready {
			status = http.StatusConflict
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	}
}

func (s *Server) authenticate(ctx context.Context, r *http.Request) (*Caller, error) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return nil, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return nil, errors.New("missing bearer token")
	}

	tr := &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token}}
	out, err := s.kube.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("tokenreview failed: %w", err)
	}
	if !out.Status.Authenticated {
		return nil, errors.New("token not authenticated")
	}

	user := out.Status.User.Username
	ns, err := namespaceFromSAUser(user)
	if err != nil {
		return nil, fmt.Errorf("unsupported user format: %v", err)
	}

	return &Caller{
		Token:  token,
		User:   user,
		Groups: out.Status.User.Groups,
		Extra:  out.Status.User.Extra,
		NS:     ns,
	}, nil
}

func namespaceFromSAUser(username string) (string, error) {
	// expected: system:serviceaccount:<ns>:<sa>
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return "", errors.New("not a serviceaccount user")
	}
	remaining := strings.TrimPrefix(username, prefix)
	parts := strings.Split(remaining, ":")
	if len(parts) != 2 || parts[0] == "" {
		return "", errors.New("bad serviceaccount username")
	}
	return parts[0], nil
}

func (s *Server) namespaceMode(ctx context.Context, ns string) (string, error) {
	obj, err := s.kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	mode := strings.ToLower(strings.TrimSpace(obj.Labels[nsModeLabel]))
	if mode == modePlatform {
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

func (s *Server) evaluateGate(ctx context.Context, caller *Caller, mode, gateNS string, gate *unstructured.Unstructured) ([]EvalResult, bool, error) {
	checks, found, err := unstructured.NestedSlice(gate.Object, "spec", "checks")
	if err != nil || !found {
		return []EvalResult{{ID: "spec", Ready: false, Message: "missing spec.checks"}}, false, errors.New("invalid gate spec")
	}

	strict := true
	if v, ok, _ := unstructured.NestedBool(gate.Object, "spec", "strict"); ok {
		strict = v
	}

	results := make([]EvalResult, 0, len(checks))
	allReady := true

	for i, c := range checks {
		checkMap, ok := c.(map[string]any)
		if !ok {
			results = append(results, EvalResult{
				ID:      fmt.Sprintf("check-%d", i),
				Ready:   false,
				Message: "check item is not an object",
			})
			allReady = false
			if strict {
				return results, false, errors.New("invalid check object")
			}
			continue
		}

		id := strOr(checkMap["id"], fmt.Sprintf("check-%d", i))

		// Enforce exactly one check type set
		if n := countCheckTypes(checkMap); n != 1 {
			results = append(results, EvalResult{
				ID:      id,
				Ready:   false,
				Message: fmt.Sprintf("invalid check: expected exactly 1 check type, got %d", n),
			})
			allReady = false
			if strict {
				return results, false, errors.New("invalid check: multiple or zero check types")
			}
			continue
		}

		// target namespace defaults to gateNS (NOT caller.NS) so central platform callers work
		targetNS := gateNS
		if nsRaw, ok := checkMap["namespace"]; ok {
			if ns := strings.TrimSpace(fmt.Sprint(nsRaw)); ns != "" {
				targetNS = ns
			}
		}

		// policy: customer mode => same namespace only (both Gate and deps)
		if mode == modeCustomer && targetNS != gateNS {
			results = append(results, EvalResult{
				ID:      id,
				Ready:   false,
				Message: fmt.Sprintf("cross-namespace checks not allowed in customer mode (target=%s)", targetNS),
			})
			allReady = false
			if strict {
				return results, false, errors.New("policy violation")
			}
			continue
		}

		// policy: platform mode => allow deps in allowlist/prefix list
		if mode == modePlatform && targetNS != gateNS && !s.platformNSAllowed(targetNS) {
			results = append(results, EvalResult{
				ID:      id,
				Ready:   false,
				Message: fmt.Sprintf("target namespace not allowed in platform mode (target=%s)", targetNS),
			})
			allReady = false
			if strict {
				return results, false, errors.New("policy violation")
			}
			continue
		}

		ready, msg, specErr, authErr := s.evalOne(ctx, caller, targetNS, checkMap)
		if authErr != nil {
			results = append(results, EvalResult{ID: id, Ready: false, Message: authErr.Error()})
			allReady = false
			if strict {
				return results, false, authErr
			}
			continue
		}
		if specErr != nil {
			results = append(results, EvalResult{ID: id, Ready: false, Message: specErr.Error()})
			allReady = false
			if strict {
				return results, false, specErr
			}
			continue
		}

		results = append(results, EvalResult{ID: id, Ready: ready, Message: msg})
		if !ready {
			allReady = false
		}
	}

	return results, allReady, nil
}

func countCheckTypes(m map[string]any) int {
	keys := []string{
		"deploymentAvailable",
		"statefulSetReady",
		"jobComplete",
		"serviceReadyEndpoints",
		"podLabelReady",
		"argoApplicationHealthy",
	}
	n := 0
	for _, k := range keys {
		if _, ok := m[k]; ok {
			n++
		}
	}
	return n
}

func (s *Server) evalOne(ctx context.Context, caller *Caller, ns string, check map[string]any) (ready bool, msg string, specErr error, authErr error) {
	// Determine which check type is set

	if block, ok := check["deploymentAvailable"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return false, "", errors.New("deploymentAvailable.name required"), nil
		}
		minAvail := int64(numOr(block["minAvailableReplicas"], 1))

		if err := s.requireSAR(ctx, caller, ns, "apps", "deployments", "get", name); err != nil {
			return false, "", nil, err
		}

		d, err := s.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("deployment get failed: %v", err), nil, nil
		}
		return deploymentReady(d, minAvail)
	}

	if block, ok := check["statefulSetReady"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return false, "", errors.New("statefulSetReady.name required"), nil
		}
		minReady := int64(numOr(block["minReadyReplicas"], 1))
		reqUpd := boolOr(block["requireUpdatedRevision"], true)

		if err := s.requireSAR(ctx, caller, ns, "apps", "statefulsets", "get", name); err != nil {
			return false, "", nil, err
		}

		sts, err := s.kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("statefulset get failed: %v", err), nil, nil
		}
		return statefulSetReady(sts, minReady, reqUpd)
	}

	if block, ok := check["jobComplete"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return false, "", errors.New("jobComplete.name required"), nil
		}

		if err := s.requireSAR(ctx, caller, ns, "batch", "jobs", "get", name); err != nil {
			return false, "", nil, err
		}

		j, err := s.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("job get failed: %v", err), nil, nil
		}
		return jobComplete(j)
	}

	if block, ok := check["serviceReadyEndpoints"].(map[string]any); ok {
		name := strOr(block["name"], "")
		if name == "" {
			return false, "", errors.New("serviceReadyEndpoints.name required"), nil
		}
		minReady := int64(numOr(block["minReadyAddresses"], 1))

		if err := s.requireSAR(ctx, caller, ns, "", "services", "get", name); err != nil {
			return false, "", nil, err
		}
		if err := s.requireSAR(ctx, caller, ns, "discovery.k8s.io", "endpointslices", "list", ""); err != nil {
			return false, "", nil, err
		}

		_, err := s.kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("service get failed: %v", err), nil, nil
		}

		readyAddrs, err := countReadyAddressesForService(ctx, s.kube, ns, name)
		if err != nil {
			return false, fmt.Sprintf("endpointslices failed: %v", err), nil, nil
		}
		if readyAddrs >= minReady {
			return true, fmt.Sprintf("ready addresses %d >= %d", readyAddrs, minReady), nil, nil
		}
		return false, fmt.Sprintf("ready addresses %d < %d", readyAddrs, minReady), nil, nil
	}

	if block, ok := check["podLabelReady"].(map[string]any); ok {
		selector := strOr(block["selector"], "")
		if selector == "" {
			return false, "", errors.New("podLabelReady.selector required"), nil
		}
		minReady := int64(numOr(block["minReadyPods"], 1))

		if err := s.requireSAR(ctx, caller, ns, "", "pods", "list", ""); err != nil {
			return false, "", nil, err
		}

		pods, err := s.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, fmt.Sprintf("pods list failed: %v", err), nil, nil
		}
		readyCount := int64(0)
		for _, p := range pods.Items {
			if podReady(&p) {
				readyCount++
			}
		}
		if readyCount >= minReady {
			return true, fmt.Sprintf("ready pods %d >= %d", readyCount, minReady), nil, nil
		}
		return false, fmt.Sprintf("ready pods %d < %d", readyCount, minReady), nil, nil
	}

	// NOTE: argoApplicationHealthy exists in the CRD but is not implemented in this file.
	// Add it when you’re ready; until then, leave it out of Gate specs.
	if _, ok := check["argoApplicationHealthy"]; ok {
		return false, "", errors.New("argoApplicationHealthy not implemented"), nil
	}

	return false, "", errors.New("no recognized check type set"), nil
}

// --- readiness helpers

func deploymentReady(d *appsv1.Deployment, minAvail int64) (bool, string, error, error) {
	avail := int64(d.Status.AvailableReplicas)
	if avail < minAvail {
		return false, fmt.Sprintf("availableReplicas %d < %d", avail, minAvail), nil, nil
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true, "deployment Available=True", nil, nil
		}
	}
	return false, "deployment Available condition not True", nil, nil
}

func statefulSetReady(sts *appsv1.StatefulSet, minReady int64, requireUpdated bool) (bool, string, error, error) {
	ready := int64(sts.Status.ReadyReplicas)
	if ready < minReady {
		return false, fmt.Sprintf("readyReplicas %d < %d", ready, minReady), nil, nil
	}
	if requireUpdated {
		if sts.Status.UpdateRevision != "" && sts.Status.CurrentRevision != "" &&
			sts.Status.UpdateRevision != sts.Status.CurrentRevision {
			return false, fmt.Sprintf("revision not fully updated (current=%s update=%s)", sts.Status.CurrentRevision, sts.Status.UpdateRevision), nil, nil
		}
	}
	return true, "statefulset ready", nil, nil
}

func jobComplete(j *batchv1.Job) (bool, string, error, error) {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true, "job Complete=True", nil, nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return false, "job Failed=True: " + c.Message, nil, nil
			}
			return false, "job Failed=True", nil, nil
		}
	}
	return false, "job not complete yet", nil, nil
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
	for _, es := range slices.Items {
		ready += readyAddresses(&es)
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

// --- authz (delegated) helpers

func (s *Server) requireSAR(ctx context.Context, caller *Caller, namespace, apiGroup, resource, verb, name string) error {
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   caller.User,
			Groups: caller.Groups,
			Extra:  convertExtra(caller.Extra),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      verb,
				Group:     apiGroup,
				Resource:  resource,
				Name:      name,
			},
		},
	}
	out, err := s.kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("subjectaccessreview failed: %w", err)
	}
	if !out.Status.Allowed {
		reason := out.Status.Reason
		if reason == "" {
			reason = "not allowed"
		}
		group := apiGroup
		if group == "" {
			group = "core"
		}
		return fmt.Errorf("forbidden: %s %s (group=%s) name=%q ns=%q: %s", verb, resource, group, name, namespace, reason)
	}
	return nil
}

func convertExtra(extra map[string]authv1.ExtraValue) map[string]authzv1.ExtraValue {
	if extra == nil {
		return nil
	}
	out := make(map[string]authzv1.ExtraValue, len(extra))
	for k, v := range extra {
		out[k] = authzv1.ExtraValue(v)
	}
	return out
}

// --- Gate status update

func (s *Server) updateGateStatus(ctx context.Context, ns string, gate *unstructured.Unstructured, ready bool, results []EvalResult) error {
	now := time.Now().UTC().Format(time.RFC3339)

	status := map[string]any{
		"observedGeneration": gate.GetGeneration(),
		"ready":              ready,
		"lastEvaluatedTime":  now,
		"results":            resultsToUnstructured(results),
	}

	patch := map[string]any{"status": status}
	b, _ := json.Marshal(patch)

	_, err := s.dyn.Resource(s.gateGVR).Namespace(ns).Patch(
		ctx,
		gate.GetName(),
		types.MergePatchType,
		b,
		metav1.PatchOptions{},
		"status",
	)
	return err
}

func resultsToUnstructured(in []EvalResult) []any {
	out := make([]any, 0, len(in))
	for _, r := range in {
		out = append(out, map[string]any{
			"id":      r.ID,
			"ready":   r.Ready,
			"message": r.Message,
		})
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
