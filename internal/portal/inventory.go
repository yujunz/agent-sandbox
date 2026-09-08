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
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	agentsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/typed/api/v1beta1"
	claimsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/typed/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/lifecycle"
)

// SandboxClient provides namespace-scoped generated Sandbox clients.
type SandboxClient interface {
	Sandboxes(namespace string) agentsv1beta1.SandboxInterface
}

// ClaimClient provides namespace-scoped generated SandboxClaim clients.
type ClaimClient interface {
	SandboxClaims(namespace string) claimsv1beta1.SandboxClaimInterface
}

// InventoryOptions configures inventory scope, ownership labels, and link generation.
type InventoryOptions struct {
	Context          string
	Namespace        string
	UserLabel        string
	AgentLabel       string
	RouterPathPrefix string
	AllNamespaces    bool
	RouterURL        *url.URL
	Now              func() time.Time
}

// Inventory projects Kubernetes resources into the portal's stable response model.
type Inventory struct {
	sandboxes SandboxClient
	claims    ClaimClient
	opts      InventoryOptions
}

// NewInventory constructs an Inventory using generated typed client boundaries.
func NewInventory(sandboxes SandboxClient, claims ClaimClient, opts InventoryOptions) *Inventory {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Inventory{sandboxes: sandboxes, claims: claims, opts: opts}
}

// List returns the Sandbox inventory in deterministic namespace and name order.
func (i *Inventory) List(ctx context.Context) (InventoryResponse, error) {
	namespace := i.opts.Namespace
	if i.opts.AllNamespaces {
		namespace = metav1.NamespaceAll
	} else if namespace == "" {
		namespace = metav1.NamespaceDefault
	}

	list, err := i.sandboxes.Sandboxes(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return InventoryResponse{}, fmt.Errorf("list Sandboxes: %w", err)
	}
	claims, claimErr := i.claims.SandboxClaims(namespace).List(ctx, metav1.ListOptions{})
	claimDataAvailable := claimErr == nil
	warnings := make([]string, 0)
	if claimErr != nil {
		warnings = append(warnings, claimListWarning(claimErr))
	}

	now := i.opts.Now()
	records := make([]SandboxRecord, 0, len(list.Items))
	for idx := range list.Items {
		var claim *extensionsv1beta1.SandboxClaim
		if claimDataAvailable {
			var warning string
			claim, warning = matchClaim(&list.Items[idx], claims.Items)
			if warning != "" {
				warnings = append(warnings, warning)
			}
		}
		records = append(records, i.projectSandbox(&list.Items[idx], claim, now))
	}
	slices.SortFunc(records, func(left, right SandboxRecord) int {
		if namespaceOrder := cmp.Compare(left.Namespace, right.Namespace); namespaceOrder != 0 {
			return namespaceOrder
		}
		return cmp.Compare(left.Name, right.Name)
	})

	return InventoryResponse{
		GeneratedAt:   now,
		Context:       i.opts.Context,
		Namespace:     namespace,
		AllNamespaces: i.opts.AllNamespaces,
		Warnings:      warnings,
		Sandboxes:     records,
	}, nil
}

func (i *Inventory) projectSandbox(
	sandbox *sandboxv1beta1.Sandbox,
	claim *extensionsv1beta1.SandboxClaim,
	now time.Time,
) SandboxRecord {
	ready := summarizeReady(sandbox.Status.Conditions)
	operatingMode := sandbox.Spec.OperatingMode
	if operatingMode == "" {
		operatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	}

	var authoritativeClaim *extensionsv1beta1.SandboxClaim
	if controllerOwnerClaimVerified(sandbox, claim) {
		authoritativeClaim = claim
	}
	lifecycle := lifecycleSummary(sandbox, authoritativeClaim, now)
	claimName := ""
	if claim != nil {
		claimName = claim.Name
	} else if owner := claimControllerOwner(sandbox); owner != nil {
		claimName = owner.Name
	}
	claimLifecycleUnavailable := claimControllerOwner(sandbox) != nil && authoritativeClaim == nil
	return SandboxRecord{
		Namespace:     sandbox.Namespace,
		Name:          sandbox.Name,
		ClaimName:     claimName,
		CreatedAt:     sandbox.CreationTimestamp.Time,
		OperatingMode: string(operatingMode),
		Ready:         ready,
		User:          preferredLabel(sandbox.Spec.PodTemplate.ObjectMeta.Labels, sandbox.Labels, i.opts.UserLabel),
		Agent:         preferredLabel(sandbox.Spec.PodTemplate.ObjectMeta.Labels, sandbox.Labels, i.opts.AgentLabel),
		Containers:    projectContainers(sandbox.Spec.PodTemplate.Spec.Containers),
		RuntimeClass:  valueOrEmpty(sandbox.Spec.PodTemplate.Spec.RuntimeClassName),
		Lifecycle:     lifecycle,
		PodIPs:        append([]string{}, sandbox.Status.PodIPs...),
		ServiceFQDN:   sandbox.Status.ServiceFQDN,
		Connections:   connectionRecords(sandbox, i.opts.RouterURL, i.opts.RouterPathPrefix),
		TerminalEligible: ready.Status == string(metav1.ConditionTrue) &&
			operatingMode != sandboxv1beta1.SandboxOperatingModeSuspended &&
			sandbox.DeletionTimestamp == nil &&
			!lifecycle.Expired &&
			!claimLifecycleUnavailable,
	}
}

func controllerOwnerClaimVerified(sandbox *sandboxv1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim) bool {
	owner := claimControllerOwner(sandbox)
	return owner != nil && claim != nil &&
		owner.UID != "" && claim.UID != "" && owner.UID == claim.UID &&
		claim.Namespace == sandbox.Namespace && claim.Name == owner.Name
}

func summarizeReady(conditions []metav1.Condition) ConditionSummary {
	condition := apiMeta.FindStatusCondition(conditions, string(sandboxv1beta1.SandboxConditionReady))
	if condition == nil {
		return ConditionSummary{Status: string(metav1.ConditionUnknown)}
	}
	transitionTime := condition.LastTransitionTime.Time
	return ConditionSummary{
		Status:         string(condition.Status),
		Reason:         condition.Reason,
		Message:        condition.Message,
		TransitionTime: &transitionTime,
	}
}

func matchClaim(sandbox *sandboxv1beta1.Sandbox, claims []extensionsv1beta1.SandboxClaim) (*extensionsv1beta1.SandboxClaim, string) {
	if owner := claimControllerOwner(sandbox); owner != nil {
		for idx := range claims {
			claim := &claims[idx]
			if claim.Namespace == sandbox.Namespace && claim.Name == owner.Name && claim.UID == owner.UID {
				return claim, ""
			}
		}
		return nil, ""
	}

	var matched *extensionsv1beta1.SandboxClaim
	for idx := range claims {
		claim := &claims[idx]
		if claim.Namespace != sandbox.Namespace || claim.Status.SandboxStatus.Name != sandbox.Name {
			continue
		}
		if matched != nil {
			return nil, fmt.Sprintf(
				"multiple SandboxClaims reference Sandbox %s/%s; lifecycle enrichment is unavailable",
				sandbox.Namespace,
				sandbox.Name,
			)
		}
		matched = claim
	}
	return matched, ""
}

func claimControllerOwner(sandbox *sandboxv1beta1.Sandbox) *metav1.OwnerReference {
	for idx := range sandbox.OwnerReferences {
		owner := &sandbox.OwnerReferences[idx]
		if owner.Controller == nil || !*owner.Controller || owner.Kind != extensionsv1beta1.SandboxClaimKind {
			continue
		}
		groupVersion, err := schema.ParseGroupVersion(owner.APIVersion)
		if err == nil && groupVersion.Group == extensionsv1beta1.GroupVersion.Group {
			return owner
		}
	}
	return nil
}

func authoritativeExpiry(sandbox *sandboxv1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim) *time.Time {
	if claim == nil {
		return lifecycle.ExpireAt(sandbox.Spec.ShutdownTime, nil, nil)
	}
	if claim.Spec.Lifecycle == nil {
		return nil
	}
	finished := lifecycle.FinishedCondition(claim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
	return lifecycle.ExpireAt(claim.Spec.Lifecycle.ShutdownTime, claim.Spec.Lifecycle.TTLSecondsAfterFinished, finished)
}

func lifecycleSummary(sandbox *sandboxv1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim, now time.Time) LifecycleSummary {
	shutdown := sandbox.Spec.ShutdownTime
	var ttl *int32
	var finished *metav1.Condition
	source := "Sandbox"
	if claim != nil {
		source = "SandboxClaim"
		shutdown = nil
		if claim.Spec.Lifecycle != nil {
			shutdown = claim.Spec.Lifecycle.ShutdownTime
			ttl = claim.Spec.Lifecycle.TTLSecondsAfterFinished
		}
		finished = lifecycle.FinishedCondition(claim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
	}
	expiresAt := authoritativeExpiry(sandbox, claim)
	var retentionDeadline *time.Time
	if ttl != nil && finished != nil {
		finishedAt := finished.LastTransitionTime.Time
		deadline := finishedAt.Add(time.Duration(*ttl) * time.Second)
		retentionDeadline = &deadline
	}
	if shutdown == nil && ttl == nil {
		source = ""
	}
	return LifecycleSummary{
		Source:                  source,
		ExpiresAt:               expiresAt,
		TTLSecondsAfterFinished: ttl,
		RetentionDeadline:       retentionDeadline,
		Expired:                 expiresAt != nil && !now.Before(*expiresAt),
	}
}

func claimListWarning(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "SandboxClaim access forbidden; lifecycle enrichment is unavailable"
	case apierrors.IsNotFound(err):
		return "SandboxClaim API is unavailable; lifecycle enrichment is disabled"
	default:
		return "SandboxClaim listing failed; lifecycle enrichment is temporarily unavailable"
	}
}

func preferredLabel(primary, fallback map[string]string, key string) string {
	if value, ok := primary[key]; ok {
		return value
	}
	return fallback[key]
}

func projectContainers(containers []corev1.Container) []ContainerRecord {
	records := make([]ContainerRecord, 0, len(containers))
	for _, container := range containers {
		ports := make([]PortRecord, 0, len(container.Ports))
		for _, port := range container.Ports {
			ports = append(ports, PortRecord{
				Name:     port.Name,
				Protocol: string(port.Protocol),
				Port:     port.ContainerPort,
			})
		}
		records = append(records, ContainerRecord{
			Name:  container.Name,
			Image: container.Image,
			Ports: ports,
		})
	}
	return records
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
