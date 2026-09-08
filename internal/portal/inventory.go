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
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	agentsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/typed/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/typed/api/v1beta1"
)

// SandboxClient provides namespace-scoped generated Sandbox clients.
type SandboxClient interface {
	Sandboxes(namespace string) agentsv1beta1.SandboxInterface
}

// ClaimClient provides namespace-scoped generated SandboxClaim clients.
type ClaimClient interface {
	SandboxClaims(namespace string) extensionsv1beta1.SandboxClaimInterface
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
	}

	list, err := i.sandboxes.Sandboxes(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return InventoryResponse{}, fmt.Errorf("list Sandboxes: %w", err)
	}

	now := i.opts.Now()
	records := make([]SandboxRecord, 0, len(list.Items))
	for idx := range list.Items {
		records = append(records, i.projectSandbox(&list.Items[idx], now))
	}
	slices.SortFunc(records, func(left, right SandboxRecord) int {
		if namespaceOrder := cmp.Compare(left.Namespace, right.Namespace); namespaceOrder != 0 {
			return namespaceOrder
		}
		return cmp.Compare(left.Name, right.Name)
	})

	responseNamespace := i.opts.Namespace
	if i.opts.AllNamespaces {
		responseNamespace = ""
	}
	return InventoryResponse{
		GeneratedAt:   now,
		Context:       i.opts.Context,
		Namespace:     responseNamespace,
		AllNamespaces: i.opts.AllNamespaces,
		Sandboxes:     records,
	}, nil
}

func (i *Inventory) projectSandbox(sandbox *sandboxv1beta1.Sandbox, now time.Time) SandboxRecord {
	ready := summarizeReady(sandbox.Status.Conditions)
	operatingMode := sandbox.Spec.OperatingMode
	if operatingMode == "" {
		operatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	}

	lifecycle := summarizeSandboxLifecycle(sandbox.Spec.ShutdownTime, now)
	return SandboxRecord{
		Namespace:        sandbox.Namespace,
		Name:             sandbox.Name,
		CreatedAt:        sandbox.CreationTimestamp.Time,
		OperatingMode:    string(operatingMode),
		Ready:            ready,
		User:             preferredLabel(sandbox.Spec.PodTemplate.ObjectMeta.Labels, sandbox.Labels, i.opts.UserLabel),
		Agent:            preferredLabel(sandbox.Spec.PodTemplate.ObjectMeta.Labels, sandbox.Labels, i.opts.AgentLabel),
		Containers:       projectContainers(sandbox.Spec.PodTemplate.Spec.Containers),
		RuntimeClass:     valueOrEmpty(sandbox.Spec.PodTemplate.Spec.RuntimeClassName),
		Lifecycle:        lifecycle,
		PodIPs:           append([]string{}, sandbox.Status.PodIPs...),
		ServiceFQDN:      sandbox.Status.ServiceFQDN,
		Connections:      []ConnectionRecord{},
		TerminalEligible: ready.Status == string(metav1.ConditionTrue) && sandbox.DeletionTimestamp == nil && !lifecycle.Expired,
	}
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

func summarizeSandboxLifecycle(shutdownTime *metav1.Time, now time.Time) LifecycleSummary {
	if shutdownTime == nil {
		return LifecycleSummary{}
	}
	expiresAt := shutdownTime.Time
	return LifecycleSummary{
		Source:    "Sandbox",
		ExpiresAt: &expiresAt,
		Expired:   !now.Before(expiresAt),
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
