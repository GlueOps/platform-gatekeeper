package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// --- platformNSAllowed ---

func TestPlatformNSAllowed(t *testing.T) {
	s := &Server{
		platformAllowedNS:       []string{"glueops-core", "nonprod"},
		platformAllowedPrefixes: []string{"glueops-core-"},
	}

	tests := []struct {
		ns   string
		want bool
	}{
		{"glueops-core", true},
		{"nonprod", true},
		{"glueops-core-gatekeeper", true},
		{"glueops-core-logging", true},
		{"production", false},
		{"customer-ns", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ns, func(t *testing.T) {
			if got := s.platformNSAllowed(tt.ns); got != tt.want {
				t.Errorf("platformNSAllowed(%q) = %v, want %v", tt.ns, got, tt.want)
			}
		})
	}
}

// An operator who forgets the allowlist env vars must get no cross-namespace
// access, rather than the previously hardcoded defaults.
func TestPlatformNSAllowedEmptyByDefault(t *testing.T) {
	// A trailing comma or a whitespace-only value must not yield an empty prefix:
	// strings.HasPrefix(ns, "") matches every namespace.
	for _, raw := range []string{"", " ", ",", ",,", " , ", "\t"} {
		s := &Server{
			platformAllowedNS:       parseCSV(raw),
			platformAllowedPrefixes: parseCSV(raw),
		}
		for _, ns := range []string{"glueops-core", "nonprod", "glueops-core-gatekeeper", "anything"} {
			if s.platformNSAllowed(ns) {
				t.Errorf("parseCSV(%q): platformNSAllowed(%q) = true, want false", raw, ns)
			}
		}
	}
}

// --- deploymentReady ---

func TestDeploymentReady(t *testing.T) {
	tests := []struct {
		name      string
		deploy    *appsv1.Deployment
		minAvail  int64
		wantReady bool
	}{
		{
			name: "available and condition true",
			deploy: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 2,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
					},
				},
			},
			minAvail:  1,
			wantReady: true,
		},
		{
			name: "not enough replicas",
			deploy: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 0,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
					},
				},
			},
			minAvail:  1,
			wantReady: false,
		},
		{
			name: "enough replicas but condition not true",
			deploy: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 2,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse},
					},
				},
			},
			minAvail:  1,
			wantReady: false,
		},
		{
			name: "no conditions",
			deploy: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 2,
				},
			},
			minAvail:  1,
			wantReady: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, _ := deploymentReady(tt.deploy, tt.minAvail)
			if ready != tt.wantReady {
				t.Errorf("deploymentReady() ready = %v, want %v", ready, tt.wantReady)
			}
		})
	}
}

// --- statefulSetReady ---

func TestStatefulSetReady(t *testing.T) {
	tests := []struct {
		name           string
		sts            *appsv1.StatefulSet
		minReady       int64
		requireUpdated bool
		wantReady      bool
	}{
		{
			name: "ready and updated",
			sts: &appsv1.StatefulSet{
				Status: appsv1.StatefulSetStatus{
					ReadyReplicas:   3,
					CurrentRevision: "rev-1",
					UpdateRevision:  "rev-1",
				},
			},
			minReady:       1,
			requireUpdated: true,
			wantReady:      true,
		},
		{
			name: "not enough ready",
			sts: &appsv1.StatefulSet{
				Status: appsv1.StatefulSetStatus{
					ReadyReplicas: 0,
				},
			},
			minReady:       1,
			requireUpdated: false,
			wantReady:      false,
		},
		{
			name: "ready but revision mismatch",
			sts: &appsv1.StatefulSet{
				Status: appsv1.StatefulSetStatus{
					ReadyReplicas:   3,
					CurrentRevision: "rev-1",
					UpdateRevision:  "rev-2",
				},
			},
			minReady:       1,
			requireUpdated: true,
			wantReady:      false,
		},
		{
			name: "ready and revision mismatch but not required",
			sts: &appsv1.StatefulSet{
				Status: appsv1.StatefulSetStatus{
					ReadyReplicas:   3,
					CurrentRevision: "rev-1",
					UpdateRevision:  "rev-2",
				},
			},
			minReady:       1,
			requireUpdated: false,
			wantReady:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, _ := statefulSetReady(tt.sts, tt.minReady, tt.requireUpdated)
			if ready != tt.wantReady {
				t.Errorf("statefulSetReady() ready = %v, want %v", ready, tt.wantReady)
			}
		})
	}
}

// --- jobComplete ---

func TestJobComplete(t *testing.T) {
	tests := []struct {
		name         string
		job          *batchv1.Job
		wantReady    bool
		wantMsg      string
		wantTerminal bool
	}{
		{
			name: "completed",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			wantReady: true,
			wantMsg:   "job Complete=True",
		},
		{
			// A failed Job will never complete on its own, so this must be
			// terminal: a poller that treats it as "not yet" waits out its whole
			// timeout for something that cannot happen.
			name: "failed is terminal, and echoes the reason not the message",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
							Reason: "BackoffLimitExceeded", Message: "pod failed: SECRET-VALUE"},
					},
				},
			},
			wantReady:    false,
			wantMsg:      "job Failed=True (BackoffLimitExceeded)",
			wantTerminal: true,
		},
		{
			name: "failed without a reason is still terminal",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
			},
			wantReady:    false,
			wantMsg:      "job Failed=True",
			wantTerminal: true,
		},
		{
			name:      "still running is not terminal",
			job:       &batchv1.Job{},
			wantReady: false,
			wantMsg:   "job not complete yet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, msg, terminal := jobComplete(tt.job)
			if ready != tt.wantReady {
				t.Errorf("jobComplete() ready = %v, want %v", ready, tt.wantReady)
			}
			if msg != tt.wantMsg {
				t.Errorf("jobComplete() msg = %q, want %q", msg, tt.wantMsg)
			}
			if terminal != tt.wantTerminal {
				t.Errorf("jobComplete() terminal = %v, want %v", terminal, tt.wantTerminal)
			}
		})
	}
}

// --- argoAppReady ---

func TestArgoAppReady(t *testing.T) {
	makeApp := func(health, sync string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{Object: map[string]any{}}
		if health != "" || sync != "" {
			status := map[string]any{}
			if health != "" {
				status["health"] = map[string]any{"status": health}
			}
			if sync != "" {
				status["sync"] = map[string]any{"status": sync}
			}
			obj.Object["status"] = status
		}
		return obj
	}

	tests := []struct {
		name           string
		app            *unstructured.Unstructured
		requireSynced  bool
		requireHealthy bool
		wantReady      bool
		wantTerminal   bool
		wantErr        bool
	}{
		{"healthy and synced", makeApp("Healthy", "Synced"), true, true, true, false, false},
		{"healthy not synced", makeApp("Healthy", "OutOfSync"), true, true, false, false, false},
		// Argo CD health is a rollup over an Application's resources, not a
		// statement that its controller gave up, so none of these is terminal.
		// Missing in particular is the normal state of an Application whose
		// resources are not live yet -- exactly what a Gate should wait for.
		{"degraded is not terminal", makeApp("Degraded", "Synced"), true, true, false, false, false},
		{"missing is not terminal", makeApp("Missing", "Synced"), true, true, false, false, false},
		{"progressing is not terminal", makeApp("Progressing", "Synced"), true, true, false, false, false},
		{"healthy only required", makeApp("Healthy", "OutOfSync"), false, true, true, false, false},
		{"health not checked when not required", makeApp("Degraded", "Synced"), true, false, true, false, false},
		{"neither required", makeApp("Degraded", "OutOfSync"), false, false, true, false, false},
		{"no status at all", makeApp("", ""), true, true, false, false, false},
		{"health missing sync present", makeApp("", "Synced"), true, true, false, false, false},
		{"health present sync missing", makeApp("Healthy", ""), true, true, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, _, terminal, err := argoAppReady(tt.app, tt.requireSynced, tt.requireHealthy)
			if ready != tt.wantReady {
				t.Errorf("argoAppReady() ready = %v, want %v", ready, tt.wantReady)
			}
			if terminal != tt.wantTerminal {
				t.Errorf("argoAppReady() terminal = %v, want %v", terminal, tt.wantTerminal)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("argoAppReady() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	// A malformed Application status is the upstream object's fault, not the Gate
	// author's, so it surfaces as an error rather than an InvalidSpec result.
	t.Run("health status wrong type", func(t *testing.T) {
		app := &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{
				"health": map[string]any{"status": 123},
				"sync":   map[string]any{"status": "Synced"},
			},
		}}
		if _, _, _, err := argoAppReady(app, true, true); err == nil {
			t.Error("expected an error for non-string health status")
		}
	})

	t.Run("sync status wrong type", func(t *testing.T) {
		app := &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{
				"health": map[string]any{"status": "Healthy"},
				"sync":   map[string]any{"status": []string{"Synced"}},
			},
		}}
		if _, _, _, err := argoAppReady(app, true, false); err == nil {
			t.Error("expected an error for non-string sync status")
		}
	})
}

// --- fluxHelmReleaseReady ---

func TestFluxHelmReleaseReady(t *testing.T) {
	// generation/observedGeneration are omitted when < 0 so the "never reconciled"
	// path can be exercised; conds of nil means status.conditions is absent entirely.
	makeHR := func(generation, observed int64, conds []any) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"generation": generation},
		}}
		status := map[string]any{}
		if observed >= 0 {
			status["observedGeneration"] = observed
		}
		if conds != nil {
			status["conditions"] = conds
		}
		if len(status) > 0 {
			obj.Object["status"] = status
		}
		return obj
	}
	cond := func(t, s, reason, m string) any {
		return map[string]any{"type": t, "status": s, "reason": reason, "message": m}
	}
	// The messages carry a marker so the tests below can prove upstream text is
	// never republished: a real one wraps a Helm error, which may wrap an admission
	// webhook quoting the rejected object's field values.
	ready := []any{cond("Ready", "True", "InstallSucceeded", "Helm install succeeded")}
	notReady := []any{cond("Ready", "False", "InstallFailed", "LEAKED-UPSTREAM-TEXT")}
	stalled := []any{
		cond("Ready", "False", "InstallFailed", "LEAKED-UPSTREAM-TEXT"),
		cond("Stalled", "True", "RetriesExceeded", "LEAKED-UPSTREAM-TEXT"),
	}

	tests := []struct {
		name         string
		hr           *unstructured.Unstructured
		requireGen   bool
		wantReady    bool
		wantTerminal bool
		wantErr      bool
	}{
		{"ready and current", makeHR(3, 3, ready), true, true, false, false},
		{"ready, generation lagging", makeHR(4, 3, ready), true, false, false, false},
		{"ready, lagging but currency not required", makeHR(4, 3, ready), false, true, false, false},
		{"observedGeneration ahead (controller raced)", makeHR(3, 4, ready), true, true, false, false},
		{"never reconciled", makeHR(1, -1, nil), true, false, false, false},
		{"never reconciled, currency not required", makeHR(1, -1, nil), false, false, false, false},
		{"reconciled but no conditions yet", makeHR(1, 1, nil), true, false, false, false},
		{"empty conditions slice", makeHR(1, 1, []any{}), true, false, false, false},
		{"not ready is not terminal", makeHR(1, 1, notReady), true, false, false, false},
		// Stalled means helm-controller has given up until the spec changes, so
		// unlike a plain not-ready it must not be waited out.
		{"stalled is terminal", makeHR(1, 1, stalled), true, false, true, false},
		{"conditions present but no Ready", makeHR(1, 1, []any{cond("Released", "True", "", "")}), true, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, terminal, err := fluxHelmReleaseReady(tt.hr, tt.requireGen)
			if got != tt.wantReady {
				t.Errorf("fluxHelmReleaseReady() ready = %v, want %v", got, tt.wantReady)
			}
			if terminal != tt.wantTerminal {
				t.Errorf("fluxHelmReleaseReady() terminal = %v, want %v", terminal, tt.wantTerminal)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("fluxHelmReleaseReady() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	// Stalled must be distinguishable from a plain not-Ready: a poller that cannot
	// tell them apart burns its whole timeout on a release that will never recover.
	t.Run("stalled names the condition and its reason", func(t *testing.T) {
		_, msg, _, _ := fluxHelmReleaseReady(makeHR(1, 1, stalled), true)
		if !strings.Contains(msg, "Stalled=True") {
			t.Errorf("stalled message = %q, want it to mention Stalled=True", msg)
		}
		if !strings.Contains(msg, "RetriesExceeded") {
			t.Errorf("stalled message = %q, want the condition reason included", msg)
		}
	})

	t.Run("not-ready carries the reason but never the upstream message", func(t *testing.T) {
		_, msg, _, _ := fluxHelmReleaseReady(makeHR(1, 1, notReady), true)
		if !strings.Contains(msg, "InstallFailed") {
			t.Errorf("not-ready message = %q, want the condition reason included", msg)
		}
		if strings.Contains(msg, "Stalled") {
			t.Errorf("not-ready message = %q, must not claim Stalled", msg)
		}
	})

	// A Gate's status is readable by anyone who can read its namespace, and the
	// Argo CD health check republishes it into the UI, so upstream condition
	// messages must not travel.
	t.Run("upstream condition messages are never republished", func(t *testing.T) {
		for name, conds := range map[string][]any{"not ready": notReady, "stalled": stalled} {
			_, msg, _, _ := fluxHelmReleaseReady(makeHR(1, 1, conds), true)
			if strings.Contains(msg, "LEAKED-UPSTREAM-TEXT") {
				t.Errorf("%s: message = %q, must not echo the upstream condition message", name, msg)
			}
		}
	})

	t.Run("lagging generation is reported as in-progress, not failed", func(t *testing.T) {
		_, msg, _, _ := fluxHelmReleaseReady(makeHR(4, 3, ready), true)
		if !strings.Contains(msg, "reconcile in progress") {
			t.Errorf("lagging message = %q, want it to say reconcile in progress", msg)
		}
	})

	t.Run("observedGeneration wrong type", func(t *testing.T) {
		hr := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"generation": int64(1)},
			"status":   map[string]any{"observedGeneration": "nope"},
		}}
		if _, _, _, err := fluxHelmReleaseReady(hr, true); err == nil {
			t.Error("expected an error for non-integer observedGeneration")
		}
	})

	t.Run("conditions wrong type", func(t *testing.T) {
		hr := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"generation": int64(1)},
			"status": map[string]any{
				"observedGeneration": int64(1),
				"conditions":         "not-a-slice",
			},
		}}
		if _, _, _, err := fluxHelmReleaseReady(hr, true); err == nil {
			t.Error("expected an error for non-slice conditions")
		}
	})

	t.Run("malformed condition entries are skipped, not fatal", func(t *testing.T) {
		hr := makeHR(1, 1, []any{"garbage", cond("Ready", "True", "", "")})
		got, _, _, err := fluxHelmReleaseReady(hr, true)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if !got {
			t.Error("expected ready despite a malformed sibling condition entry")
		}
	})
}

// --- podReady ---

func TestPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "not ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "no conditions",
			pod:  &corev1.Pod{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podReady(tt.pod); got != tt.want {
				t.Errorf("podReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- readyAddresses ---

func TestReadyEndpoints(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name string
		es   *discoveryv1.EndpointSlice
		want int64
	}{
		{
			// Endpoints[].Addresses holds alternate addresses for one endpoint, so
			// two ready pods are two backends however many addresses they carry.
			name: "counts endpoints, not addresses",
			es: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.1", "10.0.0.2"}},
					{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.3"}},
				},
			},
			want: 2,
		},
		{
			name: "mixed ready and not ready",
			es: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.1"}},
					{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)}, Addresses: []string{"10.0.0.2"}},
				},
			},
			want: 1,
		},
		{
			name: "nil ready fails closed",
			es: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{Conditions: discoveryv1.EndpointConditions{Ready: nil}, Addresses: []string{"10.0.0.1"}},
				},
			},
			want: 0,
		},
		{
			name: "empty",
			es:   &discoveryv1.EndpointSlice{},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readyEndpoints(tt.es); got != tt.want {
				t.Errorf("readyEndpoints() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A dual-stack Service has one slice per address family describing the same pods,
// so the count must not add them together.
func TestCountReadyAddressesIsNotInflatedByDualStack(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	readySlice := func(name string, at discoveryv1.AddressType, addrs ...string) *discoveryv1.EndpointSlice {
		eps := make([]discoveryv1.Endpoint, 0, len(addrs))
		for _, a := range addrs {
			eps = append(eps, discoveryv1.Endpoint{
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
				Addresses:  []string{a},
			})
		}
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "nonprod", Labels: map[string]string{"kubernetes.io/service-name": "redis"}},
			AddressType: at,
			Endpoints:   eps,
		}
	}

	tests := []struct {
		name   string
		slices []runtime.Object
		want   int64
	}{
		{
			name:   "single stack",
			slices: []runtime.Object{readySlice("redis-v4", discoveryv1.AddressTypeIPv4, "10.0.0.1", "10.0.0.2")},
			want:   2,
		},
		{
			// Two real pods, described twice. Summing would report 4.
			name: "dual stack counts each backend once",
			slices: []runtime.Object{
				readySlice("redis-v4", discoveryv1.AddressTypeIPv4, "10.0.0.1", "10.0.0.2"),
				readySlice("redis-v6", discoveryv1.AddressTypeIPv6, "fd00::1", "fd00::2"),
			},
			want: 2,
		},
		{
			name: "an FQDN slice is ignored",
			slices: []runtime.Object{
				readySlice("redis-v4", discoveryv1.AddressTypeIPv4, "10.0.0.1"),
				readySlice("redis-fqdn", discoveryv1.AddressTypeFQDN, "a.example", "b.example", "c.example"),
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kube := kubefake.NewSimpleClientset(tt.slices...)
			got, err := countReadyAddressesForService(context.Background(), kube, "nonprod", "redis")
			if err != nil {
				t.Fatalf("countReadyAddressesForService: %v", err)
			}
			if got != tt.want {
				t.Errorf("ready addresses = %d, want %d", got, tt.want)
			}
		})
	}
}

// --- countCheckTypes ---

func TestCountCheckTypes(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]any
		want int
	}{
		{"one type", map[string]any{"deploymentAvailable": map[string]any{}}, 1},
		{"two types", map[string]any{"deploymentAvailable": map[string]any{}, "jobComplete": map[string]any{}}, 2},
		{"zero types", map[string]any{"id": "test"}, 0},
		{"all types", map[string]any{
			"deploymentAvailable":    map[string]any{},
			"statefulSetReady":       map[string]any{},
			"jobComplete":            map[string]any{},
			"serviceReadyEndpoints":  map[string]any{},
			"podLabelReady":          map[string]any{},
			"argoApplicationHealthy": map[string]any{},
			"fluxHelmReleaseReady":   map[string]any{},
		}, 7},
		{"unknown key ignored", map[string]any{"unknownCheck": map[string]any{}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countCheckTypes(tt.m); got != tt.want {
				t.Errorf("countCheckTypes() = %d, want %d", got, tt.want)
			}
		})
	}
}

// --- small helpers ---

func TestParseCSV(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ", []string{"a", "b"}},
		{"single", []string{"single"}},
		{"", nil},
		{",,,", nil},
		{" , ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseCSV(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCSV(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i, v := range tt.want {
				if got[i] != v {
					t.Errorf("parseCSV(%q)[%d] = %q, want %q", tt.input, i, got[i], v)
				}
			}
		})
	}
}

func TestStrOr(t *testing.T) {
	tests := []struct {
		name string
		v    any
		def  string
		want string
	}{
		{"nil", nil, "default", "default"},
		{"empty string", "", "default", "default"},
		{"whitespace", "   ", "default", "default"},
		{"value", "hello", "default", "hello"},
		{"number", 42, "default", "42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strOr(tt.v, tt.def); got != tt.want {
				t.Errorf("strOr(%v, %q) = %q, want %q", tt.v, tt.def, got, tt.want)
			}
		})
	}
}

func TestNumOr(t *testing.T) {
	tests := []struct {
		name string
		v    any
		def  int
		want int
	}{
		{"nil", nil, 5, 5},
		{"int", 10, 5, 10},
		{"int64", int64(20), 5, 20},
		{"float64", float64(30), 5, 30},
		{"string fallback", "not a number", 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := numOr(tt.v, tt.def); got != tt.want {
				t.Errorf("numOr(%v, %d) = %d, want %d", tt.v, tt.def, got, tt.want)
			}
		})
	}
}

func TestBoolOr(t *testing.T) {
	tests := []struct {
		name string
		v    any
		def  bool
		want bool
	}{
		{"nil", nil, true, true},
		{"true", true, false, true},
		{"false", false, true, false},
		{"non-bool fallback", "yes", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boolOr(tt.v, tt.def); got != tt.want {
				t.Errorf("boolOr(%v, %v) = %v, want %v", tt.v, tt.def, got, tt.want)
			}
		})
	}
}

// --- reconcile-level tests ---

var testGateGVR = schema.GroupVersionResource{
	Group:    "platform.glueops.dev",
	Version:  "v1alpha1",
	Resource: "gates",
}

func testGate(ns, name string, generation int64, checks ...any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "platform.glueops.dev/v1alpha1",
		"kind":       "Gate",
		"metadata": map[string]any{
			"name":       name,
			"namespace":  ns,
			"generation": generation,
		},
		"spec": map[string]any{"checks": checks},
	}}
}

func availableDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
}

var (
	testArgoGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	testFluxGVR = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
)

// newTestServer wires a Server onto fake clients. mode is the value of the
// namespace mode label on "nonprod"; an empty string leaves it unlabelled, which
// means customer mode. dynObjs holds Gates and any custom resources checks point
// at (Argo CD Applications, Flux HelmReleases).
func newTestServer(t *testing.T, mode string, kubeObjs []runtime.Object, dynObjs ...runtime.Object) *Server {
	t.Helper()

	nonprod := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "nonprod"}}
	if mode != "" {
		nonprod.Labels = map[string]string{nsModeLabel: mode}
	}
	kube := kubefake.NewSimpleClientset(append([]runtime.Object{nonprod}, kubeObjs...)...)

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			testGateGVR: "GateList",
			testArgoGVR: "ApplicationList",
			testFluxGVR: "HelmReleaseList",
		},
		dynObjs...,
	)

	return &Server{
		kube:                    kube,
		dyn:                     dyn,
		gateGVR:                 testGateGVR,
		platformAllowedNS:       []string{"glueops-core"},
		platformAllowedPrefixes: []string{"glueops-core-"},
		reconcileInterval:       time.Second,
	}
}

func unstructuredObj(apiVersion, kind, ns, name string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": ns, "generation": int64(1)},
		"status":     status,
	}}
}

func readyConditionFields(t *testing.T, s *Server, ns, name string) (status, reason, message, transition string) {
	t.Helper()
	got, err := s.dyn.Resource(s.gateGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}
	conds, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found {
		t.Fatalf("gate %s/%s has no status.conditions (err=%v)", ns, name, err)
	}
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok || strOr(c["type"], "") != conditionReady {
			continue
		}
		return strOr(c["status"], ""), strOr(c["reason"], ""), strOr(c["message"], ""), strOr(c["lastTransitionTime"], "")
	}
	t.Fatalf("gate %s/%s has no Ready condition", ns, name)
	return "", "", "", ""
}

func TestReconcileOnceWritesReadyCondition(t *testing.T) {
	gate := testGate("nonprod", "app", 3, map[string]any{
		"id":                  "db",
		"deploymentAvailable": map[string]any{"name": "postgres"},
	})
	s := newTestServer(t, "", []runtime.Object{availableDeployment("nonprod", "postgres")}, gate)

	s.reconcileOnce(context.Background())

	status, reason, _, _ := readyConditionFields(t, s, "nonprod", "app")
	if status != string(metav1.ConditionTrue) {
		t.Errorf("Ready status = %q, want True", status)
	}
	if reason != reasonReady {
		t.Errorf("Ready reason = %q, want %q", reason, reasonReady)
	}

	got, _ := s.dyn.Resource(testGateGVR).Namespace("nonprod").Get(context.Background(), "app", metav1.GetOptions{})
	if gen, _, _ := unstructured.NestedInt64(got.Object, "status", "observedGeneration"); gen != 3 {
		t.Errorf("status.observedGeneration = %d, want 3", gen)
	}
	results, found, _ := unstructured.NestedSlice(got.Object, "status", "results")
	if !found || len(results) != 1 {
		t.Errorf("status.results = %v, want 1 entry", results)
	}
	if s.lastSweep.Load() == 0 {
		t.Error("a successful sweep should stamp lastSweep")
	}
}

// lastTransitionTime marks when the status last changed, not when it was last
// observed, so a steady Gate must keep the same timestamp across sweeps.
func TestLastTransitionTimeInheritedWhenStatusUnchanged(t *testing.T) {
	gate := testGate("nonprod", "app", 1, map[string]any{
		"id":                  "db",
		"deploymentAvailable": map[string]any{"name": "postgres"},
	})
	s := newTestServer(t, "", []runtime.Object{availableDeployment("nonprod", "postgres")}, gate)

	s.reconcileOnce(context.Background())
	_, _, _, first := readyConditionFields(t, s, "nonprod", "app")

	s.reconcileOnce(context.Background())
	status, _, _, second := readyConditionFields(t, s, "nonprod", "app")

	if status != string(metav1.ConditionTrue) {
		t.Fatalf("Ready status = %q, want True", status)
	}
	if first == "" || first != second {
		t.Errorf("lastTransitionTime moved on an unchanged status: %q then %q", first, second)
	}
}

// A single API error must not report the dependency as unready: that would flip a
// healthy Gate to False and, with the Argo CD health check installed, interrupt a
// wave that was about to proceed.
func TestTransientErrorYieldsUnknownNotFalse(t *testing.T) {
	gate := testGate("nonprod", "app", 1, map[string]any{
		"id":                  "db",
		"deploymentAvailable": map[string]any{"name": "postgres"},
	})
	s := newTestServer(t, "", []runtime.Object{availableDeployment("nonprod", "postgres")}, gate)

	kube := s.kube.(*kubefake.Clientset)
	kube.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver had a moment"))
	})

	s.reconcileOnce(context.Background())

	status, reason, _, _ := readyConditionFields(t, s, "nonprod", "app")
	if status != string(metav1.ConditionUnknown) {
		t.Errorf("Ready status = %q, want Unknown", status)
	}
	if reason != reasonEvaluationFailed {
		t.Errorf("Ready reason = %q, want %q", reason, reasonEvaluationFailed)
	}
}

func TestForbiddenReadIsDistinctFromNotReady(t *testing.T) {
	gate := testGate("nonprod", "app", 1, map[string]any{
		"id":                  "db",
		"deploymentAvailable": map[string]any{"name": "postgres"},
	})
	s := newTestServer(t, "", nil, gate)

	kube := s.kube.(*kubefake.Clientset)
	kube.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "postgres", errors.New("nope"))
	})

	s.reconcileOnce(context.Background())

	status, reason, message, _ := readyConditionFields(t, s, "nonprod", "app")
	if status != string(metav1.ConditionFalse) {
		t.Errorf("Ready status = %q, want False", status)
	}
	if reason != reasonReadForbidden {
		t.Errorf("Ready reason = %q, want %q", reason, reasonReadForbidden)
	}
	if !strings.Contains(message, "ClusterRole") {
		t.Errorf("message = %q, want it to point the operator at gatekeeper's ClusterRole", message)
	}
}

// A dependency that does not exist yet is a normal not-ready, not an error.
func TestMissingDependencyIsNotReady(t *testing.T) {
	gate := testGate("nonprod", "app", 1, map[string]any{
		"id":                  "db",
		"deploymentAvailable": map[string]any{"name": "postgres"},
	})
	s := newTestServer(t, "", nil, gate)

	s.reconcileOnce(context.Background())

	status, reason, _, _ := readyConditionFields(t, s, "nonprod", "app")
	if status != string(metav1.ConditionFalse) {
		t.Errorf("Ready status = %q, want False", status)
	}
	if reason != reasonDependenciesNotReady {
		t.Errorf("Ready reason = %q, want %q", reason, reasonDependenciesNotReady)
	}
}

// The namespace mode label plus the allowlist is the only thing bounding what a
// Gate may read, so cover it directly.
func TestModeEnforcement(t *testing.T) {
	crossNS := map[string]any{
		"id":                  "dep",
		"namespace":           "glueops-core",
		"deploymentAvailable": map[string]any{"name": "whatever"},
	}
	notAllowed := map[string]any{
		"id":                  "dep",
		"namespace":           "someone-elses-ns",
		"deploymentAvailable": map[string]any{"name": "whatever"},
	}

	tests := []struct {
		name       string
		mode       string
		check      map[string]any
		wantReason string
	}{
		{"customer mode blocks cross-namespace", modeCustomer, crossNS, reasonPolicyViolation},
		{"unlabelled namespace defaults to customer", "", crossNS, reasonPolicyViolation},
		{"platform mode allows an allowlisted namespace", modePlatform, crossNS, reasonDependenciesNotReady},
		{"platform mode blocks a namespace off the allowlist", modePlatform, notAllowed, reasonPolicyViolation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Deliberately driven through reconcileOnce rather than calling
			// evaluateGate with a hand-computed mode: the namespace label is the
			// policy boundary, so namespaceMode has to be the thing under test.
			gate := testGate("nonprod", "app", 1, tt.check)
			s := newTestServer(t, tt.mode, nil, gate)

			s.reconcileOnce(context.Background())

			_, reason, message, _ := readyConditionFields(t, s, "nonprod", "app")
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q (message: %s)", reason, tt.wantReason, message)
			}
		})
	}
}

// The mode label is read with ToLower/TrimSpace, so confirm the normalisation
// actually grants platform mode rather than silently falling back to customer.
func TestNamespaceModeNormalisation(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"platform", modePlatform},
		{"Platform", modePlatform},
		{"  PLATFORM  ", modePlatform},
		{"customer", modeCustomer},
		{"", modeCustomer},
		{"nonsense", modeCustomer},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			s := newTestServer(t, tt.label, nil)
			got, err := s.namespaceMode(context.Background(), "nonprod")
			if err != nil {
				t.Fatalf("namespaceMode: %v", err)
			}
			if got != tt.want {
				t.Errorf("namespaceMode(label=%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}

// If the namespace mode can't be resolved, gatekeeper cannot know which policy
// applies, so it must make no claim rather than guess.
func TestNamespaceModeLookupFailureIsUnknown(t *testing.T) {
	gate := testGate("ghost-ns", "app", 1, map[string]any{
		"id":                  "db",
		"deploymentAvailable": map[string]any{"name": "postgres"},
	})
	s := newTestServer(t, "", nil, gate)

	s.reconcileOnce(context.Background())

	status, reason, _, _ := readyConditionFields(t, s, "ghost-ns", "app")
	if status != string(metav1.ConditionUnknown) {
		t.Errorf("Ready status = %q, want Unknown", status)
	}
	if reason != reasonEvaluationFailed {
		t.Errorf("Ready reason = %q, want %q", reason, reasonEvaluationFailed)
	}
}

func TestSummarize(t *testing.T) {
	ready := EvalResult{ID: "a", Ready: true, Reason: reasonReady}

	tests := []struct {
		name       string
		results    []EvalResult
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"no checks", nil, metav1.ConditionFalse, reasonInvalidSpec},
		{"all ready", []EvalResult{ready, {ID: "b", Ready: true}}, metav1.ConditionTrue, reasonReady},
		{
			"not ready",
			[]EvalResult{ready, {ID: "b", Reason: reasonDependenciesNotReady}},
			metav1.ConditionFalse, reasonDependenciesNotReady,
		},
		{
			"terminal outranks not-ready",
			[]EvalResult{{ID: "b", Reason: reasonDependenciesNotReady}, {ID: "c", Reason: reasonTerminal, Terminal: true}},
			metav1.ConditionFalse, reasonTerminal,
		},
		{
			"evaluation failure is Unknown",
			[]EvalResult{ready, {ID: "b", Reason: reasonEvaluationFailed}},
			metav1.ConditionUnknown, reasonEvaluationFailed,
		},
		{
			"forbidden outranks terminal",
			[]EvalResult{{ID: "b", Reason: reasonTerminal, Terminal: true}, {ID: "c", Reason: reasonReadForbidden}},
			metav1.ConditionFalse, reasonReadForbidden,
		},
		{
			"policy violation outranks forbidden",
			[]EvalResult{{ID: "b", Reason: reasonReadForbidden}, {ID: "c", Reason: reasonPolicyViolation}},
			metav1.ConditionFalse, reasonPolicyViolation,
		},
		{
			"invalid spec outranks everything",
			[]EvalResult{{ID: "b", Reason: reasonPolicyViolation}, {ID: "c", Reason: reasonInvalidSpec}},
			metav1.ConditionFalse, reasonInvalidSpec,
		},
		{
			// A terminal check must beat an unevaluable one, or a broken
			// dependency would read as Unknown and keep the wave waiting.
			"terminal outranks evaluation failure",
			[]EvalResult{{ID: "b", Reason: reasonEvaluationFailed}, {ID: "c", Reason: reasonTerminal, Terminal: true}},
			metav1.ConditionFalse, reasonTerminal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, msg := summarize(tt.results)
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
			if msg == "" {
				t.Error("summarize returned an empty message")
			}
		})
	}
}

func TestInvalidCheckSpec(t *testing.T) {
	tests := []struct {
		name  string
		check map[string]any
	}{
		{"zero check types", map[string]any{"id": "x"}},
		{"two check types", map[string]any{
			"id":                  "x",
			"deploymentAvailable": map[string]any{"name": "a"},
			"jobComplete":         map[string]any{"name": "b"},
		}},
		{"missing required name", map[string]any{
			"id":                  "x",
			"deploymentAvailable": map[string]any{},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, "", nil)
			results := s.evaluateGate(context.Background(), modeCustomer, "nonprod",
				testGate("nonprod", "app", 1, tt.check))
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
			if results[0].Reason != reasonInvalidSpec {
				t.Errorf("reason = %q, want %q", results[0].Reason, reasonInvalidSpec)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", maxMessageLen+100)
	got := truncate(long)
	if len(got) != maxMessageLen {
		t.Errorf("truncate() length = %d, want %d", len(got), maxMessageLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated message should end in an ellipsis, got %q", got[len(got)-10:])
	}
}

// Every check type must actually dispatch. The option names and the two custom
// GVRs in evalOne are string literals hand-matched against crd.yaml, and a typo in
// any of them fails silently — the key lookup misses, numOr/boolOr return the
// default, and the predicate unit tests still pass because they take the value as
// an argument. This is the test that catches that class.
func TestEveryCheckTypeDispatches(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	readyPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nonprod", Labels: map[string]string{"app": "worker"}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}},
		}
	}

	tests := []struct {
		name     string
		check    map[string]any
		kubeObjs []runtime.Object
		dynObjs  []runtime.Object
		wantPass bool
	}{
		{
			name:     "deploymentAvailable satisfied",
			check:    map[string]any{"id": "c", "deploymentAvailable": map[string]any{"name": "d", "minAvailableReplicas": int64(2)}},
			kubeObjs: []runtime.Object{deploymentWithAvailable("nonprod", "d", 2)},
			wantPass: true,
		},
		{
			// Proves minAvailableReplicas is read rather than silently defaulted to 1.
			name:     "deploymentAvailable respects minAvailableReplicas",
			check:    map[string]any{"id": "c", "deploymentAvailable": map[string]any{"name": "d", "minAvailableReplicas": int64(3)}},
			kubeObjs: []runtime.Object{deploymentWithAvailable("nonprod", "d", 2)},
			wantPass: false,
		},
		{
			name:  "statefulSetReady satisfied",
			check: map[string]any{"id": "c", "statefulSetReady": map[string]any{"name": "s", "minReadyReplicas": int64(2)}},
			kubeObjs: []runtime.Object{&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "nonprod"},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 2, CurrentRevision: "r1", UpdateRevision: "r1"},
			}},
			wantPass: true,
		},
		{
			// Proves requireUpdatedRevision is read; with it defaulted true this
			// mid-rollout StatefulSet must not pass.
			name:  "statefulSetReady honours requireUpdatedRevision",
			check: map[string]any{"id": "c", "statefulSetReady": map[string]any{"name": "s", "requireUpdatedRevision": true}},
			kubeObjs: []runtime.Object{&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "nonprod"},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 2, CurrentRevision: "r1", UpdateRevision: "r2"},
			}},
			wantPass: false,
		},
		{
			name:  "jobComplete satisfied",
			check: map[string]any{"id": "c", "jobComplete": map[string]any{"name": "j"}},
			kubeObjs: []runtime.Object{&batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "nonprod"},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				}},
			}},
			wantPass: true,
		},
		{
			name:  "serviceReadyEndpoints satisfied",
			check: map[string]any{"id": "c", "serviceReadyEndpoints": map[string]any{"name": "svc", "minReadyAddresses": int64(2)}},
			kubeObjs: []runtime.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "nonprod"}},
				&discoveryv1.EndpointSlice{
					ObjectMeta:  metav1.ObjectMeta{Name: "svc-1", Namespace: "nonprod", Labels: map[string]string{"kubernetes.io/service-name": "svc"}},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.1"}},
						{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.2"}},
					},
				},
			},
			wantPass: true,
		},
		{
			// Proves minReadyAddresses is read; one backend must not satisfy two.
			name:  "serviceReadyEndpoints respects minReadyAddresses",
			check: map[string]any{"id": "c", "serviceReadyEndpoints": map[string]any{"name": "svc", "minReadyAddresses": int64(2)}},
			kubeObjs: []runtime.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "nonprod"}},
				&discoveryv1.EndpointSlice{
					ObjectMeta:  metav1.ObjectMeta{Name: "svc-1", Namespace: "nonprod", Labels: map[string]string{"kubernetes.io/service-name": "svc"}},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.1", "10.0.0.9"}},
					},
				},
			},
			wantPass: false,
		},
		{
			name:     "podLabelReady satisfied",
			check:    map[string]any{"id": "c", "podLabelReady": map[string]any{"selector": "app=worker", "minReadyPods": int64(2)}},
			kubeObjs: []runtime.Object{readyPod("w1"), readyPod("w2")},
			wantPass: true,
		},
		{
			// Proves minReadyPods is read rather than defaulted to 1.
			name:     "podLabelReady respects minReadyPods",
			check:    map[string]any{"id": "c", "podLabelReady": map[string]any{"selector": "app=worker", "minReadyPods": int64(3)}},
			kubeObjs: []runtime.Object{readyPod("w1"), readyPod("w2")},
			wantPass: false,
		},
		{
			name:  "argoApplicationHealthy satisfied",
			check: map[string]any{"id": "c", "argoApplicationHealthy": map[string]any{"name": "app"}},
			dynObjs: []runtime.Object{unstructuredObj("argoproj.io/v1alpha1", "Application", "nonprod", "app", map[string]any{
				"health": map[string]any{"status": "Healthy"},
				"sync":   map[string]any{"status": "Synced"},
			})},
			wantPass: true,
		},
		{
			// Proves requireSynced is read: OutOfSync must block by default.
			name:  "argoApplicationHealthy honours requireSynced",
			check: map[string]any{"id": "c", "argoApplicationHealthy": map[string]any{"name": "app"}},
			dynObjs: []runtime.Object{unstructuredObj("argoproj.io/v1alpha1", "Application", "nonprod", "app", map[string]any{
				"health": map[string]any{"status": "Healthy"},
				"sync":   map[string]any{"status": "OutOfSync"},
			})},
			wantPass: false,
		},
		{
			name:  "fluxHelmReleaseReady satisfied",
			check: map[string]any{"id": "c", "fluxHelmReleaseReady": map[string]any{"name": "hr"}},
			dynObjs: []runtime.Object{unstructuredObj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "nonprod", "hr", map[string]any{
				"observedGeneration": int64(1),
				"conditions": []any{map[string]any{
					"type": "Ready", "status": "True", "reason": "InstallSucceeded",
				}},
			})},
			wantPass: true,
		},
		{
			// Proves requireCurrentGeneration is read: a stale observedGeneration
			// must block even though Ready=True.
			name:  "fluxHelmReleaseReady honours requireCurrentGeneration",
			check: map[string]any{"id": "c", "fluxHelmReleaseReady": map[string]any{"name": "hr"}},
			dynObjs: []runtime.Object{func() runtime.Object {
				o := unstructuredObj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "nonprod", "hr", map[string]any{
					"observedGeneration": int64(1),
					"conditions": []any{map[string]any{
						"type": "Ready", "status": "True", "reason": "InstallSucceeded",
					}},
				})
				_ = unstructured.SetNestedField(o.Object, int64(5), "metadata", "generation")
				return o
			}()},
			wantPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := testGate("nonprod", "g", 1, tt.check)
			dyn := append([]runtime.Object{gate}, tt.dynObjs...)
			s := newTestServer(t, "", tt.kubeObjs, dyn...)

			s.reconcileOnce(context.Background())

			status, reason, message, _ := readyConditionFields(t, s, "nonprod", "g")
			if tt.wantPass {
				if status != string(metav1.ConditionTrue) {
					t.Errorf("status = %q reason = %q message = %q, want True", status, reason, message)
				}
				return
			}
			if status == string(metav1.ConditionTrue) {
				t.Errorf("status = True, want the check to block (message: %s)", message)
			}
			// A blocked check must be a verdict, not a failure to look.
			if reason != reasonDependenciesNotReady {
				t.Errorf("reason = %q, want %q (message: %s)", reason, reasonDependenciesNotReady, message)
			}
		})
	}
}

func deploymentWithAvailable(ns, name string, available int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: available,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
}

// Terminal is the signal that makes Argo CD fail a gated sync promptly instead of
// waiting out --sync-timeout, so the plumbing that produces it needs covering end
// to end, not just summarize's precedence.
func TestTerminalReachesConditionAndResults(t *testing.T) {
	gate := testGate("nonprod", "g", 1, map[string]any{
		"id":          "migration",
		"jobComplete": map[string]any{"name": "j"},
	})
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "nonprod"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}},
	}
	s := newTestServer(t, "", []runtime.Object{failedJob}, gate)

	s.reconcileOnce(context.Background())

	status, reason, _, _ := readyConditionFields(t, s, "nonprod", "g")
	if status != string(metav1.ConditionFalse) {
		t.Errorf("status = %q, want False", status)
	}
	if reason != reasonTerminal {
		t.Errorf("reason = %q, want %q", reason, reasonTerminal)
	}

	got, err := s.dyn.Resource(testGateGVR).Namespace("nonprod").Get(context.Background(), "g", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}
	results, _, _ := unstructured.NestedSlice(got.Object, "status", "results")
	if len(results) != 1 {
		t.Fatalf("status.results = %v, want 1 entry", results)
	}
	entry, _ := results[0].(map[string]any)
	if terminal, _ := entry["terminal"].(bool); !terminal {
		t.Errorf("status.results[0].terminal = %v, want true", entry["terminal"])
	}
}

// lastSweep is the only input to the staleness alert, so the cases where it must
// NOT advance matter as much as the happy path.
func TestSweepSuccessAccounting(t *testing.T) {
	newGate := func() *unstructured.Unstructured {
		return testGate("nonprod", "g", 1, map[string]any{
			"id":                  "db",
			"deploymentAvailable": map[string]any{"name": "d"},
		})
	}

	t.Run("a write failure withholds the stamp", func(t *testing.T) {
		s := newTestServer(t, "", []runtime.Object{deploymentWithAvailable("nonprod", "d", 1)}, newGate())
		dyn := s.dyn.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("patch", "gates", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("nope"))
		})

		s.reconcileOnce(context.Background())

		if s.lastSweep.Load() != 0 {
			t.Error("lastSweep was stamped despite every status write failing")
		}
	})

	// A CRD installed without its status subresource 404s every patch. Treating
	// that as "the Gate was deleted" would stamp a successful sweep forever while
	// no Gate ever carries a status, and the alert would never fire.
	t.Run("an all-404 sweep withholds the stamp", func(t *testing.T) {
		s := newTestServer(t, "", []runtime.Object{deploymentWithAvailable("nonprod", "d", 1)}, newGate())
		dyn := s.dyn.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("patch", "gates", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "gates"}, "g")
		})

		s.reconcileOnce(context.Background())

		if s.lastSweep.Load() != 0 {
			t.Error("lastSweep was stamped on a sweep where every patch 404'd")
		}
	})

	t.Run("a list error withholds the stamp", func(t *testing.T) {
		s := newTestServer(t, "", nil, newGate())
		dyn := s.dyn.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("list", "gates", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("nope"))
		})

		s.reconcileOnce(context.Background())

		if s.lastSweep.Load() != 0 {
			t.Error("lastSweep was stamped despite the list failing")
		}
	})

	t.Run("an empty cluster is a successful sweep", func(t *testing.T) {
		s := newTestServer(t, "", nil)
		s.reconcileOnce(context.Background())
		if s.lastSweep.Load() == 0 {
			t.Error("a cluster with no Gates should still count as swept")
		}
	})

	t.Run("unevaluated gates are counted but do not withhold the stamp", func(t *testing.T) {
		// No Deployment exists and namespace reads fail, so the Gate ends Unknown.
		s := newTestServer(t, "", nil, newGate())
		kube := s.kube.(*kubefake.Clientset)
		kube.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("nope"))
		})

		s.reconcileOnce(context.Background())

		if s.lastSweep.Load() == 0 {
			t.Error("the sweep wrote a status, so it should be stamped")
		}
		if got := s.gatesUnknown.Load(); got != 1 {
			t.Errorf("gatesUnknown = %d, want 1", got)
		}
	})
}

// The metric names here are a contract with prometheusrule.yaml; renaming either
// side silently disables the only staleness alert.
func TestHandleMetricsExposesTheAlertedSeries(t *testing.T) {
	s := newTestServer(t, "", nil)
	s.lastSweep.Store(1700000000)
	s.gatesUnknown.Store(2)
	s.sweepDuration.Store(1500)

	rec := httptest.NewRecorder()
	s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		"gatekeeper_last_successful_sweep_timestamp_seconds 1700000000",
		"gatekeeper_gates_unevaluated 2",
		"gatekeeper_sweep_duration_seconds 1.500",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q\ngot:\n%s", want, body)
		}
	}
}

func TestReconcileIntervalFloor(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"", defaultReconcileInterval},
		{"garbage", defaultReconcileInterval},
		{"0", defaultReconcileInterval},
		{"-5s", defaultReconcileInterval},
		// Sub-second values would hammer the API server rather than reconcile.
		{"1ns", defaultReconcileInterval},
		{"500ms", defaultReconcileInterval},
		{"1s", time.Second},
		{"45s", 45 * time.Second},
		{"5m", 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv("GATEKEEPER_RECONCILE_INTERVAL", tt.raw)
			if got := reconcileInterval(); got != tt.want {
				t.Errorf("reconcileInterval() with %q = %s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}
