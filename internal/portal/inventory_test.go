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
	"context"
	"errors"
	"fmt"
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
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	clientfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extensionsfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	testUserLabel  = "sandbox.users.io/user"
	testAgentLabel = "sandbox.users.io/agent"
)

func TestInventoryProjectsCoreSandboxFields(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	createdAt := metav1.NewTime(time.Unix(100, 0).UTC())
	transitionTime := metav1.NewTime(time.Unix(150, 0).UTC())
	shutdownTime := metav1.NewTime(time.Unix(300, 0).UTC())
	runtimeClass := "gvisor"
	sb := &sandboxv1beta1.Sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "box-b",
			Namespace:         "team-a",
			CreationTimestamp: createdAt,
			Labels:            map[string]string{testUserLabel: "fallback"},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{
					testUserLabel: "alice", testAgentLabel: "planner",
				}},
				Spec: corev1.PodSpec{RuntimeClassName: &runtimeClass, Containers: []corev1.Container{
					{Name: "workspace", Image: "registry.test/workspace:v1", Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					}},
					{Name: "sidecar", Image: "registry.test/sidecar:v2", Ports: []corev1.ContainerPort{
						{ContainerPort: 5353, Protocol: corev1.ProtocolUDP},
					}},
				}},
			}},
			Lifecycle:     sandboxv1beta1.Lifecycle{ShutdownTime: &shutdownTime},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs:      []string{"10.0.0.8", "2001:db8::8"},
			ServiceFQDN: "box-b.team-a.svc.cluster.local",
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
				Message:            "ready",
				LastTransitionTime: transitionTime,
			}},
		},
	}

	got := listInventory(t, "team-a", false, now, sb)
	require.Len(t, got.Sandboxes, 1)
	record := got.Sandboxes[0]
	assert.Equal(t, now, got.GeneratedAt)
	assert.Equal(t, "test-context", got.Context)
	assert.Equal(t, "team-a", got.Namespace)
	assert.False(t, got.AllNamespaces)
	assert.Empty(t, got.Warnings)
	assert.Equal(t, "team-a", record.Namespace)
	assert.Equal(t, "box-b", record.Name)
	assert.Equal(t, createdAt.Time, record.CreatedAt)
	assert.Equal(t, "Running", record.OperatingMode)
	assert.Equal(t, ConditionSummary{
		Status:         "True",
		Reason:         sandboxv1beta1.SandboxReasonDependenciesReady,
		Message:        "ready",
		TransitionTime: new(transitionTime.Time),
	}, record.Ready)
	assert.Equal(t, "alice", record.User)
	assert.Equal(t, "planner", record.Agent)
	assert.Equal(t, "gvisor", record.RuntimeClass)
	assert.Equal(t, []ContainerRecord{
		{Name: "workspace", Image: "registry.test/workspace:v1", Ports: []PortRecord{{Name: "http", Protocol: "TCP", Port: 8080}}},
		{Name: "sidecar", Image: "registry.test/sidecar:v2", Ports: []PortRecord{{Protocol: "UDP", Port: 5353}}},
	}, record.Containers)
	assert.Equal(t, LifecycleSummary{Source: "Sandbox", ExpiresAt: new(shutdownTime.Time)}, record.Lifecycle)
	assert.Equal(t, []string{"10.0.0.8", "2001:db8::8"}, record.PodIPs)
	assert.Equal(t, "box-b.team-a.svc.cluster.local", record.ServiceFQDN)
	assert.Equal(t, []ConnectionRecord{
		{Kind: "podIP", Label: "Pod IP", Value: "10.0.0.8"},
		{Kind: "podIP", Label: "Pod IP", Value: "2001:db8::8"},
		{Kind: "serviceFQDN", Label: "Service FQDN", Value: "box-b.team-a.svc.cluster.local"},
	}, record.Connections)
	assert.True(t, record.TerminalEligible)

	record.PodIPs[0] = "modified"
	record.Containers[0].Ports[0].Port = 1
	assert.Equal(t, []string{"10.0.0.8", "2001:db8::8"}, sb.Status.PodIPs)
	assert.EqualValues(t, 8080, sb.Spec.PodTemplate.Spec.Containers[0].Ports[0].ContainerPort)
}

func TestInventoryClaimLifecycleTakesPrecedence(t *testing.T) {
	claimShutdown := metav1.NewTime(time.Unix(500, 0))
	sandboxShutdown := metav1.NewTime(time.Unix(900, 0))
	claim := claimForSandbox("claim-a", "team-a", "box-a", types.UID("claim-uid"))
	claim.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{ShutdownTime: &claimShutdown}
	sb := readySandbox("box-a", "team-a")
	sb.Spec.ShutdownTime = &sandboxShutdown
	sb.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxClaim", Name: claim.Name, UID: claim.UID, Controller: ptr.To(true),
	}}

	got := listInventoryWithClaims(t, time.Unix(400, 0), []*sandboxv1beta1.Sandbox{sb}, []*extensionsv1beta1.SandboxClaim{claim})

	require.Equal(t, claimShutdown.Time, *got.Sandboxes[0].Lifecycle.ExpiresAt)
	assert.Equal(t, "SandboxClaim", got.Sandboxes[0].Lifecycle.Source)
	assert.Equal(t, "claim-a", got.Sandboxes[0].ClaimName)
	assert.True(t, got.Sandboxes[0].TerminalEligible)
}

func TestInventoryDirectLifecycle(t *testing.T) {
	t.Run("shutdown", func(t *testing.T) {
		shutdown := metav1.NewTime(time.Unix(500, 0))
		sb := readySandbox("box-a", "team-a")
		sb.Spec.ShutdownTime = &shutdown

		record := listInventory(t, "team-a", false, time.Unix(500, 0), sb).Sandboxes[0]

		assert.Equal(t, LifecycleSummary{Source: "Sandbox", ExpiresAt: new(shutdown.Time), Expired: true}, record.Lifecycle)
		assert.False(t, record.TerminalEligible)
	})

	t.Run("no lifecycle", func(t *testing.T) {
		record := listInventory(t, "team-a", false, time.Unix(500, 0), readySandbox("box-a", "team-a")).Sandboxes[0]

		assert.Equal(t, LifecycleSummary{}, record.Lifecycle)
		assert.True(t, record.TerminalEligible)
	})
}

func TestInventoryTTLAfterFinished(t *testing.T) {
	ttl := int32(60)
	finishedAt := metav1.NewTime(time.Unix(400, 0))
	shutdown := metav1.NewTime(time.Unix(900, 0))
	claim := claimForSandbox("claim-a", "team-a", "box-a", types.UID("claim-uid"))
	claim.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{
		ShutdownTime:            &shutdown,
		TTLSecondsAfterFinished: &ttl,
	}
	claim.Status.Conditions = []metav1.Condition{{
		Type:               string(sandboxv1beta1.SandboxConditionFinished),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: finishedAt,
	}}
	sb := readySandbox("box-a", "team-a")
	setClaimOwner(sb, claim)

	record := listInventoryWithClaims(t, time.Unix(470, 0), []*sandboxv1beta1.Sandbox{sb}, []*extensionsv1beta1.SandboxClaim{claim}).Sandboxes[0]

	deadline := time.Unix(460, 0)
	assert.Equal(t, LifecycleSummary{
		Source:                  "SandboxClaim",
		ExpiresAt:               &deadline,
		TTLSecondsAfterFinished: ptr.To(int32(60)),
		RetentionDeadline:       &deadline,
		Expired:                 true,
	}, record.Lifecycle)
	assert.False(t, record.TerminalEligible)
}

func TestInventoryTTLBeforeFinishedShowsPolicyOnly(t *testing.T) {
	ttl := int32(60)
	claim := claimForSandbox("claim-a", "team-a", "box-a", types.UID("claim-uid"))
	claim.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{TTLSecondsAfterFinished: &ttl}
	claim.Status.Conditions = []metav1.Condition{{
		Type:   string(sandboxv1beta1.SandboxConditionFinished),
		Status: metav1.ConditionFalse,
	}}
	sb := readySandbox("box-a", "team-a")
	setClaimOwner(sb, claim)

	record := listInventoryWithClaims(t, time.Unix(470, 0), []*sandboxv1beta1.Sandbox{sb}, []*extensionsv1beta1.SandboxClaim{claim}).Sandboxes[0]

	assert.Equal(t, LifecycleSummary{
		Source:                  "SandboxClaim",
		TTLSecondsAfterFinished: ptr.To(int32(60)),
	}, record.Lifecycle)
	assert.True(t, record.TerminalEligible)
}

func TestInventoryClaimWithoutLifecycleOverridesSandboxLifecycle(t *testing.T) {
	sandboxShutdown := metav1.NewTime(time.Unix(100, 0))
	claim := claimForSandbox("claim-a", "team-a", "box-a", types.UID("claim-uid"))
	sb := readySandbox("box-a", "team-a")
	sb.Spec.ShutdownTime = &sandboxShutdown
	setClaimOwner(sb, claim)

	record := listInventoryWithClaims(t, time.Unix(400, 0), []*sandboxv1beta1.Sandbox{sb}, []*extensionsv1beta1.SandboxClaim{claim}).Sandboxes[0]

	assert.Equal(t, LifecycleSummary{}, record.Lifecycle)
	assert.True(t, record.TerminalEligible)
}

func TestInventoryClaimOwnerMissingDoesNotFallBackToStatusMatch(t *testing.T) {
	owner := claimForSandbox("owner", "team-a", "box-a", types.UID("owner-uid"))
	statusMatch := claimForSandbox("status-match", "team-a", "box-a", types.UID("status-uid"))
	sb := readySandbox("box-a", "team-a")
	setClaimOwner(sb, owner)

	record := listInventoryWithClaims(
		t,
		time.Unix(400, 0),
		[]*sandboxv1beta1.Sandbox{sb},
		[]*extensionsv1beta1.SandboxClaim{statusMatch},
	).Sandboxes[0]

	assert.Equal(t, "owner", record.ClaimName)
	assert.False(t, record.TerminalEligible)
}

func TestMatchClaimOwnerReferenceWinsOverStatusMatch(t *testing.T) {
	sb := readySandbox("box-a", "team-a")
	owner := claimForSandbox("owner", "team-a", "another-box", types.UID("owner-uid"))
	statusMatch := claimForSandbox("status-match", "team-a", "box-a", types.UID("status-uid"))
	setClaimOwner(sb, owner)

	got, warning := matchClaim(sb, []extensionsv1beta1.SandboxClaim{*statusMatch, *owner})

	require.NotNil(t, got)
	assert.Equal(t, owner.Name, got.Name)
	assert.Empty(t, warning)
}

func TestMatchClaimUsesSingleStatusMatchInNamespace(t *testing.T) {
	sb := readySandbox("box-a", "team-a")
	match := claimForSandbox("match", "team-a", "box-a", types.UID("match-uid"))
	otherNamespace := claimForSandbox("other", "team-b", "box-a", types.UID("other-uid"))

	got, warning := matchClaim(sb, []extensionsv1beta1.SandboxClaim{*otherNamespace, *match})

	require.NotNil(t, got)
	assert.Equal(t, match.Name, got.Name)
	assert.Empty(t, warning)
}

func TestMatchClaimRejectsAmbiguousStatusMatches(t *testing.T) {
	sb := readySandbox("box-a", "team-a")
	first := claimForSandbox("claim-a", "team-a", "box-a", types.UID("first-uid"))
	second := claimForSandbox("claim-b", "team-a", "box-a", types.UID("second-uid"))

	got, warning := matchClaim(sb, []extensionsv1beta1.SandboxClaim{*first, *second})

	assert.Nil(t, got)
	assert.Equal(t, "multiple SandboxClaims reference Sandbox team-a/box-a; lifecycle enrichment is unavailable", warning)
}

func TestInventoryMissingReadyIsUnknown(t *testing.T) {
	sb := sandboxWithReady("box", "team-a", nil)
	sb.Status.PodIPs = []string{"10.0.0.9"}

	record := listInventory(t, "team-a", false, time.Unix(200, 0), sb).Sandboxes[0]

	assert.Equal(t, ConditionSummary{Status: "Unknown"}, record.Ready)
	assert.False(t, record.TerminalEligible, "a Pod IP must not substitute for the Ready condition")
	assert.Equal(t, "Running", record.OperatingMode)
	assert.Empty(t, record.RuntimeClass)
	assert.Empty(t, record.User)
	assert.Empty(t, record.Agent)
}

func TestInventoryPodTemplateLabelsPrecedeSandboxLabels(t *testing.T) {
	sb := sandboxWithReady("box", "team-a", readyCondition(metav1.ConditionTrue))
	sb.Labels = map[string]string{testUserLabel: "fallback-user", testAgentLabel: "fallback-agent"}
	sb.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{testUserLabel: "pod-user", testAgentLabel: "pod-agent"}

	record := listInventory(t, "team-a", false, time.Unix(200, 0), sb).Sandboxes[0]

	assert.Equal(t, "pod-user", record.User)
	assert.Equal(t, "pod-agent", record.Agent)
}

func TestInventoryFallsBackToSandboxLabels(t *testing.T) {
	sb := sandboxWithReady("box", "team-a", readyCondition(metav1.ConditionFalse))
	sb.Labels = map[string]string{testUserLabel: "sandbox-user", testAgentLabel: "sandbox-agent"}

	record := listInventory(t, "team-a", false, time.Unix(200, 0), sb).Sandboxes[0]

	assert.Equal(t, "sandbox-user", record.User)
	assert.Equal(t, "sandbox-agent", record.Agent)
	assert.Equal(t, "False", record.Ready.Status)
	assert.False(t, record.TerminalEligible)
}

func TestInventoryDeletingAndSuspendedTerminalEligibility(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	deleting := sandboxWithReady("deleting", "team-a", readyCondition(metav1.ConditionTrue))
	deletionTime := metav1.NewTime(time.Unix(190, 0).UTC())
	deleting.DeletionTimestamp = &deletionTime
	deleting.Finalizers = []string{"test.example/finalizer"}

	suspended := sandboxWithReady("suspended", "team-a", readyCondition(metav1.ConditionFalse))
	suspended.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended

	expired := sandboxWithReady("expired", "team-a", readyCondition(metav1.ConditionTrue))
	expiredAt := metav1.NewTime(now)
	expired.Spec.ShutdownTime = &expiredAt

	future := sandboxWithReady("future", "team-a", readyCondition(metav1.ConditionTrue))
	futureAt := metav1.NewTime(now.Add(time.Second))
	future.Spec.ShutdownTime = &futureAt

	got := listInventory(t, "team-a", false, now, deleting, suspended, expired, future)
	eligible := make(map[string]bool, len(got.Sandboxes))
	expiredStates := make(map[string]bool, len(got.Sandboxes))
	for _, record := range got.Sandboxes {
		eligible[record.Name] = record.TerminalEligible
		expiredStates[record.Name] = record.Lifecycle.Expired
	}

	assert.Equal(t, map[string]bool{
		"deleting":  false,
		"expired":   false,
		"future":    true,
		"suspended": false,
	}, eligible)
	assert.True(t, expiredStates["expired"])
	assert.False(t, expiredStates["future"])
}

func TestInventoryUsesConfiguredNamespace(t *testing.T) {
	client := newSandboxClientset(t,
		sandboxWithReady("visible", "team-a", readyCondition(metav1.ConditionTrue)),
		sandboxWithReady("hidden", "team-b", readyCondition(metav1.ConditionTrue)),
	)
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions("team-a", false, time.Unix(200, 0)),
	)

	got, err := inventory.List(context.Background())
	require.NoError(t, err)
	require.Len(t, got.Sandboxes, 1)
	assert.Equal(t, "visible", got.Sandboxes[0].Name)
	require.Len(t, client.Actions(), 1)
	assert.Equal(t, "team-a", client.Actions()[0].GetNamespace())
}

func TestInventoryEmptyNamespaceDefaultsWithoutAllNamespaces(t *testing.T) {
	client := newSandboxClientset(t,
		sandboxWithReady("visible", metav1.NamespaceDefault, readyCondition(metav1.ConditionTrue)),
		sandboxWithReady("hidden", "team-b", readyCondition(metav1.ConditionTrue)),
	)
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions("", false, time.Unix(200, 0)),
	)

	got, err := inventory.List(context.Background())
	require.NoError(t, err)
	require.Len(t, got.Sandboxes, 1)
	assert.Equal(t, "visible", got.Sandboxes[0].Name)
	assert.Equal(t, metav1.NamespaceDefault, got.Namespace)
	require.Len(t, client.Actions(), 1)
	assert.NotEmpty(t, client.Actions()[0].GetNamespace())
	assert.Equal(t, metav1.NamespaceDefault, client.Actions()[0].GetNamespace())
}

func TestInventoryUsesNamespaceAllOnlyWhenEnabled(t *testing.T) {
	client := newSandboxClientset(t,
		sandboxWithReady("box-a", "team-a", readyCondition(metav1.ConditionTrue)),
		sandboxWithReady("box-b", "team-b", readyCondition(metav1.ConditionTrue)),
	)
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions("team-a", true, time.Unix(200, 0)),
	)

	got, err := inventory.List(context.Background())
	require.NoError(t, err)
	require.Len(t, got.Sandboxes, 2)
	assert.True(t, got.AllNamespaces)
	assert.Empty(t, got.Namespace)
	require.Len(t, client.Actions(), 1)
	assert.Equal(t, metav1.NamespaceAll, client.Actions()[0].GetNamespace())
}

func TestInventorySortsByNamespaceThenName(t *testing.T) {
	got := listInventory(t, "ignored", true, time.Unix(200, 0),
		sandboxWithReady("box-b", "team-b", readyCondition(metav1.ConditionTrue)),
		sandboxWithReady("box-c", "team-a", readyCondition(metav1.ConditionTrue)),
		sandboxWithReady("box-a", "team-a", readyCondition(metav1.ConditionTrue)),
	)

	assert.Equal(t, []string{"team-a/box-a", "team-a/box-c", "team-b/box-b"}, []string{
		got.Sandboxes[0].Namespace + "/" + got.Sandboxes[0].Name,
		got.Sandboxes[1].Namespace + "/" + got.Sandboxes[1].Name,
		got.Sandboxes[2].Namespace + "/" + got.Sandboxes[2].Name,
	})
}

func TestInventoryWrapsSandboxListErrors(t *testing.T) {
	client := clientfake.NewSimpleClientset()
	wantErr := errors.New("API unavailable")
	client.PrependReactor("list", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, wantErr
	})
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions("team-a", false, time.Unix(200, 0)),
	)

	_, err := inventory.List(context.Background())

	require.Error(t, err)
	require.ErrorIs(t, err, wantErr)
	assert.EqualError(t, err, "list Sandboxes: API unavailable")
}

func TestInventoryClaimListFailuresAreSanitized(t *testing.T) {
	claimResource := schema.GroupResource{Group: extensionsv1beta1.GroupVersion.Group, Resource: "sandboxclaims"}
	tests := []struct {
		name    string
		err     error
		warning string
	}{
		{
			name:    "forbidden",
			err:     apierrors.NewForbidden(claimResource, "", errors.New("credential=forbidden-secret")),
			warning: "SandboxClaim access forbidden; lifecycle enrichment is unavailable",
		},
		{
			name:    "API unavailable",
			err:     apierrors.NewNotFound(claimResource, "sandboxclaims"),
			warning: "SandboxClaim API is unavailable; lifecycle enrichment is disabled",
		},
		{
			name:    "temporary failure",
			err:     errors.New("response body contains generic-secret"),
			warning: "SandboxClaim listing failed; lifecycle enrichment is temporarily unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxClient := newSandboxClientset(t, readySandbox("box-a", "team-a"))
			claimClient := extensionsfake.NewSimpleClientset()
			claimClient.PrependReactor("list", "sandboxclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.err
			})
			inventory := NewInventory(
				sandboxClient.AgentsV1beta1(),
				claimClient.ExtensionsV1beta1(),
				inventoryOptions("team-a", false, time.Unix(200, 0)),
			)

			got, err := inventory.List(context.Background())

			require.NoError(t, err)
			require.Len(t, got.Sandboxes, 1)
			assert.Equal(t, []string{tt.warning}, got.Warnings)
			assert.NotContains(t, fmt.Sprint(got), "secret")
		})
	}
}

func TestInventoryClaimListFailurePreservesOwnerNameAndDisablesTerminal(t *testing.T) {
	claim := claimForSandbox("claim-a", "team-a", "box-a", types.UID("claim-uid"))
	sb := readySandbox("box-a", "team-a")
	setClaimOwner(sb, claim)
	sandboxClient := newSandboxClientset(t, sb)
	claimClient := extensionsfake.NewSimpleClientset()
	claimClient.PrependReactor("list", "sandboxclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("temporary failure")
	})
	inventory := NewInventory(
		sandboxClient.AgentsV1beta1(),
		claimClient.ExtensionsV1beta1(),
		inventoryOptions("team-a", false, time.Unix(200, 0)),
	)

	got, err := inventory.List(context.Background())

	require.NoError(t, err)
	require.Len(t, got.Sandboxes, 1)
	assert.Equal(t, "claim-a", got.Sandboxes[0].ClaimName)
	assert.False(t, got.Sandboxes[0].TerminalEligible)
}

func listInventory(
	t *testing.T,
	namespace string,
	allNamespaces bool,
	now time.Time,
	sandboxes ...*sandboxv1beta1.Sandbox,
) InventoryResponse {
	t.Helper()
	client := newSandboxClientset(t, sandboxes...)
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions(namespace, allNamespaces, now),
	)
	got, err := inventory.List(context.Background())
	require.NoError(t, err)
	return got
}

func listInventoryWithClaims(
	t *testing.T,
	now time.Time,
	sandboxes []*sandboxv1beta1.Sandbox,
	claims []*extensionsv1beta1.SandboxClaim,
) InventoryResponse {
	t.Helper()
	sandboxClient := newSandboxClientset(t, sandboxes...)
	claimObjects := make([]runtime.Object, 0, len(claims))
	for _, claim := range claims {
		claimObjects = append(claimObjects, claim)
	}
	claimClient := extensionsfake.NewSimpleClientset(claimObjects...)
	inventory := NewInventory(
		sandboxClient.AgentsV1beta1(),
		claimClient.ExtensionsV1beta1(),
		inventoryOptions("team-a", false, now),
	)
	got, err := inventory.List(context.Background())
	require.NoError(t, err)
	return got
}

func newSandboxClientset(t *testing.T, sandboxes ...*sandboxv1beta1.Sandbox) *clientfake.Clientset {
	t.Helper()
	client := clientfake.NewSimpleClientset()
	// Seed the exact generated resource because the generic tracker guesses the irregular plural as "sandboxs".
	resource := sandboxv1beta1.GroupVersion.WithResource("sandboxes")
	for _, sandbox := range sandboxes {
		require.NoError(t, client.Tracker().Create(resource, sandbox, sandbox.Namespace))
	}
	return client
}

func inventoryOptions(namespace string, allNamespaces bool, now time.Time) InventoryOptions {
	return InventoryOptions{
		Context:       "test-context",
		Namespace:     namespace,
		AllNamespaces: allNamespaces,
		UserLabel:     testUserLabel,
		AgentLabel:    testAgentLabel,
		Now:           func() time.Time { return now },
	}
}

func sandboxWithReady(name, namespace string, ready *metav1.Condition) *sandboxv1beta1.Sandbox {
	sandbox := &sandboxv1beta1.Sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if ready != nil {
		sandbox.Status.Conditions = []metav1.Condition{*ready}
	}
	return sandbox
}

func readySandbox(name, namespace string) *sandboxv1beta1.Sandbox {
	sandbox := sandboxWithReady(name, namespace, readyCondition(metav1.ConditionTrue))
	sandbox.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "workspace"}}
	return sandbox
}

func claimForSandbox(name, namespace, sandboxName string, uid types.UID) *extensionsv1beta1.SandboxClaim {
	return &extensionsv1beta1.SandboxClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       uid,
		},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: sandboxName},
		},
	}
}

func setClaimOwner(sandbox *sandboxv1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim) {
	sandbox.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       extensionsv1beta1.SandboxClaimKind,
		Name:       claim.Name,
		UID:        claim.UID,
		Controller: ptr.To(true),
	}}
}

func readyCondition(status metav1.ConditionStatus) *metav1.Condition {
	return &metav1.Condition{
		Type:   string(sandboxv1beta1.SandboxConditionReady),
		Status: status,
		Reason: "TestReason",
	}
}
