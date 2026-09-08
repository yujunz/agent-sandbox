// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portal

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	clientfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extensionsfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestTerminalResolver(t *testing.T) {
	now := time.Unix(500, 0)
	forbiddenDetail := errors.New("raw API response credential=secret")
	tests := []struct {
		name             string
		namespace        string
		sandboxName      string
		container        string
		scope            TerminalScope
		mutate           func(*terminalFixture)
		sandboxError     error
		claimError       error
		podError         error
		want             TerminalTarget
		wantStatus       int
		wantMessage      string
		wantNoAPICalls   bool
		wantNoPodRead    bool
		wantWrappedError bool
	}{
		{
			name:        "ready Sandbox resolves its annotated Pod and named container",
			namespace:   "team-a",
			sandboxName: "box-a",
			container:   "workspace",
			scope:       TerminalScope{Namespace: "team-a"},
			want: TerminalTarget{
				Namespace:   "team-a",
				SandboxName: "box-a",
				PodName:     "adopted-pod",
				Container:   "workspace",
			},
		},
		{
			name:        "all-namespaces scope resolves a Sandbox in another namespace",
			namespace:   "team-b",
			sandboxName: "box-a",
			container:   "workspace",
			scope:       TerminalScope{Namespace: "team-a", AllNamespaces: true},
			mutate: func(f *terminalFixture) {
				f.sandbox.Namespace = "team-b"
				f.pods[0].Namespace = "team-b"
			},
			want: TerminalTarget{
				Namespace:   "team-b",
				SandboxName: "box-a",
				PodName:     "adopted-pod",
				Container:   "workspace",
			},
		},
		{
			name:           "invalid namespace",
			namespace:      "Team_A",
			sandboxName:    "box-a",
			scope:          TerminalScope{AllNamespaces: true},
			wantStatus:     http.StatusBadRequest,
			wantMessage:    "invalid namespace",
			wantNoAPICalls: true,
		},
		{
			name:           "invalid Sandbox name",
			namespace:      "team-a",
			sandboxName:    "Box_A",
			scope:          TerminalScope{AllNamespaces: true},
			wantStatus:     http.StatusBadRequest,
			wantMessage:    "invalid Sandbox name",
			wantNoAPICalls: true,
		},
		{
			name:           "namespace outside single namespace scope",
			namespace:      "team-b",
			sandboxName:    "box-a",
			scope:          TerminalScope{Namespace: "team-a"},
			wantStatus:     http.StatusNotFound,
			wantMessage:    "Sandbox not found",
			wantNoAPICalls: true,
		},
		{
			name:           "empty single namespace scope fails closed",
			namespace:      "team-a",
			sandboxName:    "box-a",
			scope:          TerminalScope{},
			wantStatus:     http.StatusNotFound,
			wantMessage:    "Sandbox not found",
			wantNoAPICalls: true,
		},
		{
			name:        "Sandbox missing",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.sandbox = nil
			},
			wantStatus:  http.StatusNotFound,
			wantMessage: "Sandbox not found",
		},
		{
			name:             "Sandbox list/get forbidden",
			namespace:        "team-a",
			sandboxName:      "box-a",
			scope:            TerminalScope{Namespace: "team-a"},
			sandboxError:     apierrors.NewForbidden(schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"}, "box-a", forbiddenDetail),
			wantStatus:       http.StatusForbidden,
			wantMessage:      "Sandbox access forbidden",
			wantWrappedError: true,
		},
		{
			name:             "Sandbox API unavailable",
			namespace:        "team-a",
			sandboxName:      "box-a",
			scope:            TerminalScope{Namespace: "team-a"},
			sandboxError:     errors.New("raw API response credential=secret"),
			wantStatus:       http.StatusServiceUnavailable,
			wantMessage:      "Sandbox is temporarily unavailable",
			wantWrappedError: true,
		},
		{
			name:        "Sandbox deleting",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				deletionTime := metav1.NewTime(now)
				f.sandbox.DeletionTimestamp = &deletionTime
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "Sandbox is not Ready",
		},
		{
			name:        "Ready missing",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.sandbox.Status.Conditions = nil
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "Sandbox is not Ready",
		},
		{
			name:        "Ready False",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.sandbox.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "Sandbox is not Ready",
		},
		{
			name:        "operatingMode Suspended",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "Sandbox is not Ready",
		},
		{
			name:        "direct Sandbox shutdown elapsed",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				shutdown := metav1.NewTime(now)
				f.sandbox.Spec.ShutdownTime = &shutdown
			},
			wantStatus:  http.StatusGone,
			wantMessage: "Sandbox has expired",
		},
		{
			name:        "owner-referenced claim shutdown elapsed",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				claim := f.addClaim()
				shutdown := metav1.NewTime(now)
				claim.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{ShutdownTime: &shutdown}
			},
			wantStatus:  http.StatusGone,
			wantMessage: "Sandbox has expired",
		},
		{
			name:        "owner-referenced claim finished TTL elapsed",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				claim := f.addClaim()
				ttl := int32(60)
				claim.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{TTLSecondsAfterFinished: &ttl}
				claim.Status.Conditions = []metav1.Condition{{
					Type:               string(sandboxv1beta1.SandboxConditionFinished),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Unix(440, 0)),
				}}
			},
			wantStatus:  http.StatusGone,
			wantMessage: "Sandbox has expired",
		},
		{
			name:        "owner-referenced claim with nil lifecycle is allowed",
			namespace:   "team-a",
			sandboxName: "box-a",
			container:   "workspace",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.addClaim()
			},
			want: TerminalTarget{Namespace: "team-a", SandboxName: "box-a", PodName: "adopted-pod", Container: "workspace"},
		},
		{
			name:        "owner-referenced claim missing",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				claim := terminalClaim()
				setClaimOwner(f.sandbox, claim)
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "SandboxClaim is unavailable",
		},
		{
			name:        "owner-referenced claim forbidden",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.addClaim()
			},
			claimError:       apierrors.NewForbidden(schema.GroupResource{Group: extensionsv1beta1.GroupVersion.Group, Resource: "sandboxclaims"}, "claim-a", forbiddenDetail),
			wantStatus:       http.StatusForbidden,
			wantMessage:      "SandboxClaim access forbidden",
			wantWrappedError: true,
		},
		{
			name:        "owner-referenced claim owner UID empty",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.addClaim()
				f.sandbox.OwnerReferences[0].UID = ""
			},
			wantStatus:    http.StatusConflict,
			wantMessage:   "SandboxClaim is unavailable",
			wantNoPodRead: true,
		},
		{
			name:        "owner-referenced claim fetched UID empty",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				claim := f.addClaim()
				claim.UID = ""
			},
			wantStatus:    http.StatusConflict,
			wantMessage:   "SandboxClaim is unavailable",
			wantNoPodRead: true,
		},
		{
			name:        "owner-referenced claim UID mismatch",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				claim := f.addClaim()
				claim.UID = types.UID("replacement-claim-uid")
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "SandboxClaim is unavailable",
		},
		{
			name:        "Pod missing",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods = nil
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:             "Pod forbidden",
			namespace:        "team-a",
			sandboxName:      "box-a",
			scope:            TerminalScope{Namespace: "team-a"},
			podError:         apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "adopted-pod", forbiddenDetail),
			wantStatus:       http.StatusForbidden,
			wantMessage:      "backing Pod access forbidden",
			wantWrappedError: true,
		},
		{
			name:        "Pod controlled by another UID",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].OwnerReferences[0].UID = types.UID("other-sandbox-uid")
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "Sandbox without UID cannot own Pod",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.sandbox.UID = ""
				f.pods[0].OwnerReferences[0].UID = ""
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "Pod Pending",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].Status.Phase = corev1.PodPending
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "Pod Succeeded",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].Status.Phase = corev1.PodSucceeded
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "Pod Failed",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].Status.Phase = corev1.PodFailed
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "Running Pod without PodReady=True",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "named container absent",
			namespace:   "team-a",
			sandboxName: "box-a",
			container:   "missing",
			scope:       TerminalScope{Namespace: "team-a"},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "container not found",
		},
		{
			name:        "empty container query selects the first regular live Pod container",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].Spec.Containers = append(f.pods[0].Spec.Containers, corev1.Container{Name: "sidecar"})
			},
			want: TerminalTarget{Namespace: "team-a", SandboxName: "box-a", PodName: "adopted-pod", Container: "workspace"},
		},
		{
			name:        "no regular containers",
			namespace:   "team-a",
			sandboxName: "box-a",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.pods[0].Spec.InitContainers = []corev1.Container{{Name: "setup"}}
				f.pods[0].Spec.Containers = nil
			},
			wantStatus:  http.StatusConflict,
			wantMessage: "backing Pod is unavailable",
		},
		{
			name:        "non-empty legacy Pod annotation wins",
			namespace:   "team-a",
			sandboxName: "box-a",
			container:   "workspace",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				fallback := terminalPod("box-a", f.sandbox)
				f.pods = append(f.pods, fallback)
			},
			want: TerminalTarget{Namespace: "team-a", SandboxName: "box-a", PodName: "adopted-pod", Container: "workspace"},
		},
		{
			name:        "empty annotation falls back to the Sandbox name",
			namespace:   "team-a",
			sandboxName: "box-a",
			container:   "workspace",
			scope:       TerminalScope{Namespace: "team-a"},
			mutate: func(f *terminalFixture) {
				f.sandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation] = ""
				f.pods[0].Name = "box-a"
			},
			want: TerminalTarget{Namespace: "team-a", SandboxName: "box-a", PodName: "box-a", Container: "workspace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTerminalFixture()
			if tt.mutate != nil {
				tt.mutate(fixture)
			}
			resolver, sandboxClient, claimClient, podClient := fixture.resolver(t, tt.scope, now)
			if tt.sandboxError != nil {
				sandboxClient.PrependReactor("get", "sandboxes", terminalErrorReactor(tt.sandboxError))
			}
			if tt.claimError != nil {
				claimClient.PrependReactor("get", "sandboxclaims", terminalErrorReactor(tt.claimError))
			}
			if tt.podError != nil {
				podClient.PrependReactor("get", "pods", terminalErrorReactor(tt.podError))
			}

			got, err := resolver.Resolve(t.Context(), tt.namespace, tt.sandboxName, tt.container)
			if tt.wantStatus == 0 {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
				return
			}

			assert.Equal(t, TerminalTarget{}, got)
			var terminalErr *TerminalError
			require.ErrorAs(t, err, &terminalErr)
			assert.Equal(t, tt.wantStatus, terminalErr.Status)
			assert.Equal(t, tt.wantMessage, terminalErr.Message)
			assert.Equal(t, tt.wantMessage, terminalErr.Error())
			assert.NotContains(t, terminalErr.Error(), "credential")
			if tt.wantWrappedError {
				require.Error(t, terminalErr.Err)
				assert.Contains(t, terminalErr.Err.Error(), "credential")
			}
			if tt.wantNoAPICalls {
				assert.Empty(t, sandboxClient.Actions())
				assert.Empty(t, claimClient.Actions())
				assert.Empty(t, podClient.Actions())
			}
			if tt.wantNoPodRead {
				assert.Empty(t, podClient.Actions())
			}
		})
	}
}

func TestTerminalResolverUsesFreshReadsForEveryResolution(t *testing.T) {
	fixture := newTerminalFixture()
	fixture.addClaim()
	resolver, sandboxClient, claimClient, podClient := fixture.resolver(t, TerminalScope{Namespace: "team-a"}, time.Unix(500, 0))

	for range 2 {
		got, err := resolver.Resolve(t.Context(), "team-a", "box-a", "workspace")
		require.NoError(t, err)
		assert.Equal(t, "adopted-pod", got.PodName)
	}

	assert.Len(t, sandboxClient.Actions(), 2)
	assert.Len(t, claimClient.Actions(), 2)
	assert.Len(t, podClient.Actions(), 2)
	for _, actions := range [][]clienttesting.Action{sandboxClient.Actions(), claimClient.Actions(), podClient.Actions()} {
		for _, action := range actions {
			assert.Equal(t, "get", action.GetVerb())
			assert.Equal(t, "team-a", action.GetNamespace())
		}
	}
}

type terminalFixture struct {
	sandbox *sandboxv1beta1.Sandbox
	claim   *extensionsv1beta1.SandboxClaim
	pods    []*corev1.Pod
}

func newTerminalFixture() *terminalFixture {
	sandbox := readySandbox("box-a", "team-a")
	sandbox.UID = types.UID("sandbox-uid")
	sandbox.Annotations = map[string]string{sandboxv1beta1.SandboxPodNameAnnotation: "adopted-pod"}
	return &terminalFixture{
		sandbox: sandbox,
		pods:    []*corev1.Pod{terminalPod("adopted-pod", sandbox)},
	}
}

func (f *terminalFixture) addClaim() *extensionsv1beta1.SandboxClaim {
	f.claim = terminalClaim()
	setClaimOwner(f.sandbox, f.claim)
	return f.claim
}

func (f *terminalFixture) resolver(
	t *testing.T,
	scope TerminalScope,
	now time.Time,
) (*TerminalResolver, *clientfake.Clientset, *extensionsfake.Clientset, *kubernetesfake.Clientset) {
	t.Helper()
	sandboxClient := newSandboxClientset(t)
	if f.sandbox != nil {
		resource := sandboxv1beta1.GroupVersion.WithResource("sandboxes")
		require.NoError(t, sandboxClient.Tracker().Create(resource, f.sandbox, f.sandbox.Namespace))
	}
	claimObjects := make([]runtime.Object, 0, 1)
	if f.claim != nil {
		claimObjects = append(claimObjects, f.claim)
	}
	claimClient := extensionsfake.NewSimpleClientset(claimObjects...)
	podObjects := make([]runtime.Object, 0, len(f.pods))
	for _, pod := range f.pods {
		podObjects = append(podObjects, pod)
	}
	podClient := kubernetesfake.NewSimpleClientset(podObjects...)
	resolver := NewTerminalResolver(
		sandboxClient.AgentsV1beta1(),
		claimClient.ExtensionsV1beta1(),
		podClient.CoreV1(),
		scope,
		func() time.Time { return now },
	)
	return resolver, sandboxClient, claimClient, podClient
}

func terminalClaim() *extensionsv1beta1.SandboxClaim {
	return claimForSandbox("claim-a", "team-a", "box-a", types.UID("claim-uid"))
}

func terminalPod(name string, sandbox *sandboxv1beta1.Sandbox) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandbox.Namespace,
			UID:       types.UID("pod-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1beta1.GroupVersion.String(),
				Kind:       "Sandbox",
				Name:       sandbox.Name,
				UID:        sandbox.UID,
				Controller: new(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func terminalErrorReactor(err error) clienttesting.ReactionFunc {
	return func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	}
}
