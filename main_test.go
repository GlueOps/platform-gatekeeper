package main

import (
	"context"
	"errors"
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
		platformAllowedNS:       map[string]bool{"glueops-core": true, "nonprod": true},
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
	s := &Server{
		platformAllowedNS:       parseCSVSet(""),
		platformAllowedPrefixes: parseCSVList(""),
	}
	for _, ns := range []string{"glueops-core", "nonprod", "glueops-core-gatekeeper", "anything"} {
		if s.platformNSAllowed(ns) {
			t.Errorf("platformNSAllowed(%q) = true with an unset allowlist, want false", ns)
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
			name: "failed is terminal",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "OOM"},
					},
				},
			},
			wantReady:    false,
			wantMsg:      "job Failed=True: OOM",
			wantTerminal: true,
		},
		{
			name: "failed no message is terminal",
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
		// Degraded will not recover without a change, so waiting is pointless.
		{"degraded is terminal", makeApp("Degraded", "Synced"), true, true, false, true, false},
		{"missing is terminal", makeApp("Missing", "Synced"), true, true, false, true, false},
		// Progressing is a transient state, so it must not be terminal.
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
	cond := func(t, s, m string) any {
		return map[string]any{"type": t, "status": s, "message": m}
	}
	ready := []any{cond("Ready", "True", "Helm install succeeded")}
	notReady := []any{cond("Ready", "False", "install retries exhausted")}
	stalled := []any{
		cond("Ready", "False", "install retries exhausted"),
		cond("Stalled", "True", "exhausted 3 retries"),
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
		{"conditions present but no Ready", makeHR(1, 1, []any{cond("Released", "True", "")}), true, false, false, false},
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
	t.Run("stalled message names the condition", func(t *testing.T) {
		_, msg, _, _ := fluxHelmReleaseReady(makeHR(1, 1, stalled), true)
		if !strings.Contains(msg, "Stalled=True") {
			t.Errorf("stalled message = %q, want it to mention Stalled=True", msg)
		}
		if !strings.Contains(msg, "exhausted 3 retries") {
			t.Errorf("stalled message = %q, want the controller message included", msg)
		}
	})

	t.Run("not-ready message carries the controller message", func(t *testing.T) {
		_, msg, _, _ := fluxHelmReleaseReady(makeHR(1, 1, notReady), true)
		if !strings.Contains(msg, "install retries exhausted") {
			t.Errorf("not-ready message = %q, want the controller message included", msg)
		}
		if strings.Contains(msg, "Stalled") {
			t.Errorf("not-ready message = %q, must not claim Stalled", msg)
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
		hr := makeHR(1, 1, []any{"garbage", cond("Ready", "True", "")})
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

func TestReadyAddresses(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name string
		es   *discoveryv1.EndpointSlice
		want int64
	}{
		{
			name: "two ready endpoints with addresses",
			es: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.1", "10.0.0.2"}},
					{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, Addresses: []string{"10.0.0.3"}},
				},
			},
			want: 3,
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
			name: "nil ready",
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
			if got := readyAddresses(tt.es); got != tt.want {
				t.Errorf("readyAddresses() = %d, want %d", got, tt.want)
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

func TestParseCSVSet(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]bool
	}{
		{"a,b,c", map[string]bool{"a": true, "b": true, "c": true}},
		{" a , b ", map[string]bool{"a": true, "b": true}},
		{"single", map[string]bool{"single": true}},
		{"", map[string]bool{}},
		{",,,", map[string]bool{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseCSVSet(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCSVSet(%q) len = %d, want %d", tt.input, len(got), len(tt.want))
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("parseCSVSet(%q) missing key %q", tt.input, k)
				}
			}
		})
	}
}

func TestParseCSVList(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ", []string{"a", "b"}},
		{"", nil},
		{",,,", nil},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseCSVList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCSVList(%q) len = %d, want %d", tt.input, len(got), len(tt.want))
			}
			for i, v := range tt.want {
				if got[i] != v {
					t.Errorf("parseCSVList(%q)[%d] = %q, want %q", tt.input, i, got[i], v)
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
	Version:  "v1alpha2",
	Resource: "gates",
}

func testGate(ns, name string, generation int64, checks ...any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "platform.glueops.dev/v1alpha2",
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

// newTestServer wires a Server onto fake clients. mode is the value of the
// namespace mode label on "nonprod"; an empty string leaves it unlabelled, which
// means customer mode.
func newTestServer(t *testing.T, mode string, kubeObjs []runtime.Object, gates ...runtime.Object) *Server {
	t.Helper()

	nonprod := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "nonprod"}}
	if mode != "" {
		nonprod.Labels = map[string]string{nsModeLabel: mode}
	}
	kube := kubefake.NewSimpleClientset(append([]runtime.Object{nonprod}, kubeObjs...)...)

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{testGateGVR: "GateList"},
		gates...,
	)

	return &Server{
		kube:                    kube,
		dyn:                     dyn,
		gateGVR:                 testGateGVR,
		platformAllowedNS:       map[string]bool{"glueops-core": true},
		platformAllowedPrefixes: []string{"glueops-core-"},
		reconcileInterval:       time.Second,
	}
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
			s := newTestServer(t, tt.mode, nil)
			results := s.evaluateGate(context.Background(), modeOrDefault(tt.mode), "nonprod",
				testGate("nonprod", "app", 1, tt.check))
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
			if got := resultReason(results[0]); got != tt.wantReason {
				t.Errorf("reason = %q, want %q (message: %s)", got, tt.wantReason, results[0].Message)
			}
		})
	}
}

func modeOrDefault(mode string) string {
	if mode == modePlatform {
		return modePlatform
	}
	return modeCustomer
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
			if resultReason(results[0]) != reasonInvalidSpec {
				t.Errorf("reason = %q, want %q", resultReason(results[0]), reasonInvalidSpec)
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
