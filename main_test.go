package main

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- namespaceFromSAUser ---

func TestNamespaceFromSAUser(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantNS   string
		wantErr  bool
	}{
		{"valid sa", "system:serviceaccount:nonprod:my-sa", "nonprod", false},
		{"valid sa with dashes", "system:serviceaccount:glueops-core:gatekeeper", "glueops-core", false},
		{"not a sa", "admin", "", true},
		{"missing namespace", "system:serviceaccount:", "", true},
		{"missing sa name", "system:serviceaccount:ns:", "", true},
		{"too many colons", "system:serviceaccount:ns:sa:extra", "", true},
		{"empty string", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, err := namespaceFromSAUser(tt.username)
			if (err != nil) != tt.wantErr {
				t.Fatalf("namespaceFromSAUser(%q) err = %v, wantErr %v", tt.username, err, tt.wantErr)
			}
			if ns != tt.wantNS {
				t.Errorf("namespaceFromSAUser(%q) = %q, want %q", tt.username, ns, tt.wantNS)
			}
		})
	}
}

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
			ready, _, _, _ := deploymentReady(tt.deploy, tt.minAvail)
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
			ready, _, _, _ := statefulSetReady(tt.sts, tt.minReady, tt.requireUpdated)
			if ready != tt.wantReady {
				t.Errorf("statefulSetReady() ready = %v, want %v", ready, tt.wantReady)
			}
		})
	}
}

// --- jobComplete ---

func TestJobComplete(t *testing.T) {
	tests := []struct {
		name      string
		job       *batchv1.Job
		wantReady bool
		wantMsg   string
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
			name: "failed",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "OOM"},
					},
				},
			},
			wantReady: false,
			wantMsg:   "job Failed=True: OOM",
		},
		{
			name: "failed no message",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
			},
			wantReady: false,
			wantMsg:   "job Failed=True",
		},
		{
			name:      "still running",
			job:       &batchv1.Job{},
			wantReady: false,
			wantMsg:   "job not complete yet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, msg, _, _ := jobComplete(tt.job)
			if ready != tt.wantReady {
				t.Errorf("jobComplete() ready = %v, want %v", ready, tt.wantReady)
			}
			if msg != tt.wantMsg {
				t.Errorf("jobComplete() msg = %q, want %q", msg, tt.wantMsg)
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
	}{
		{"healthy and synced", makeApp("Healthy", "Synced"), true, true, true},
		{"healthy not synced", makeApp("Healthy", "OutOfSync"), true, true, false},
		{"not healthy but synced", makeApp("Degraded", "Synced"), true, true, false},
		{"healthy only required", makeApp("Healthy", "OutOfSync"), false, true, true},
		{"synced only required", makeApp("Degraded", "Synced"), true, false, true},
		{"neither required", makeApp("Degraded", "OutOfSync"), false, false, true},
		{"no status at all", makeApp("", ""), true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, _, _, _ := argoAppReady(tt.app, tt.requireSynced, tt.requireHealthy)
			if ready != tt.wantReady {
				t.Errorf("argoAppReady() ready = %v, want %v", ready, tt.wantReady)
			}
		})
	}
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
		}, 6},
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
