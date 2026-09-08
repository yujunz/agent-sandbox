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
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	terminalInvalidNamespaceMessage            = "invalid namespace"
	terminalInvalidSandboxNameMessage          = "invalid Sandbox name"
	terminalSandboxNotFoundMessage             = "Sandbox not found"
	terminalSandboxForbiddenMessage            = "Sandbox access forbidden"
	terminalSandboxUnavailableMessage          = "Sandbox is temporarily unavailable"
	terminalSandboxNotReadyMessage             = "Sandbox is not Ready"
	terminalSandboxExpiredMessage              = "Sandbox has expired"
	terminalClaimForbiddenMessage              = "SandboxClaim access forbidden"
	terminalClaimUnavailableMessage            = "SandboxClaim is unavailable"
	terminalClaimTemporarilyUnavailableMessage = "SandboxClaim is temporarily unavailable"
	terminalPodForbiddenMessage                = "backing Pod access forbidden"
	terminalPodUnavailableMessage              = "backing Pod is unavailable"
	terminalPodTemporarilyUnavailableMessage   = "backing Pod is temporarily unavailable"
	terminalContainerNotFoundMessage           = "container not found"
)

// TerminalScope restricts terminal resolution to one namespace unless cluster-wide access is explicit.
type TerminalScope struct {
	Namespace     string
	AllNamespaces bool
}

// TerminalTarget identifies the server-selected Pod and container authorized for terminal exec.
type TerminalTarget struct {
	Namespace   string
	SandboxName string
	PodName     string
	Container   string
}

// TerminalError separates a fixed public HTTP response from its internal diagnostic cause.
type TerminalError struct {
	Status  int
	Message string
	Err     error
}

// Error returns only the fixed public message.
func (e *TerminalError) Error() string {
	return e.Message
}

// Unwrap exposes the internal cause to server-side structured logging and inspection.
func (e *TerminalError) Unwrap() error {
	return e.Err
}

// TerminalResolver authorizes a fresh Sandbox, claim lifecycle, Pod, and container target.
type TerminalResolver struct {
	sandboxes SandboxClient
	claims    ClaimClient
	pods      typedcorev1.PodsGetter
	scope     TerminalScope
	now       func() time.Time
}

// NewTerminalResolver constructs a fresh-read terminal target resolver.
func NewTerminalResolver(
	sandboxes SandboxClient,
	claims ClaimClient,
	pods typedcorev1.PodsGetter,
	scope TerminalScope,
	now func() time.Time,
) *TerminalResolver {
	if now == nil {
		now = time.Now
	}
	return &TerminalResolver{
		sandboxes: sandboxes,
		claims:    claims,
		pods:      pods,
		scope:     scope,
		now:       now,
	}
}

// Resolve performs fresh scoped reads and returns only a Sandbox-controlled live Pod container.
func (r *TerminalResolver) Resolve(
	ctx context.Context,
	namespace string,
	sandboxName string,
	containerName string,
) (TerminalTarget, error) {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return TerminalTarget{}, terminalError(http.StatusBadRequest, terminalInvalidNamespaceMessage, nil)
	}
	if len(validation.IsDNS1123Subdomain(sandboxName)) != 0 {
		return TerminalTarget{}, terminalError(http.StatusBadRequest, terminalInvalidSandboxNameMessage, nil)
	}
	if !r.scope.AllNamespaces && (r.scope.Namespace == "" || namespace != r.scope.Namespace) {
		return TerminalTarget{}, terminalError(http.StatusNotFound, terminalSandboxNotFoundMessage, nil)
	}

	sandbox, err := r.sandboxes.Sandboxes(namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		return TerminalTarget{}, sandboxReadError(namespace, sandboxName, err)
	}

	var claim *extensionsv1beta1.SandboxClaim
	if owner := claimControllerOwner(sandbox); owner != nil {
		claim, err = r.claims.SandboxClaims(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return TerminalTarget{}, claimReadError(namespace, owner.Name, err)
		}
		if owner.UID == "" || claim.UID == "" || owner.UID != claim.UID {
			return TerminalTarget{}, terminalError(http.StatusConflict, terminalClaimUnavailableMessage, nil)
		}
	}

	if sandbox.DeletionTimestamp != nil ||
		sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended ||
		!apiMeta.IsStatusConditionTrue(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady)) {
		return TerminalTarget{}, terminalError(http.StatusConflict, terminalSandboxNotReadyMessage, nil)
	}
	if expiresAt := authoritativeExpiry(sandbox, claim); expiresAt != nil && !r.now().Before(*expiresAt) {
		return TerminalTarget{}, terminalError(http.StatusGone, terminalSandboxExpiredMessage, nil)
	}

	podName := sandbox.Name
	if annotatedPodName := sandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]; annotatedPodName != "" {
		podName = annotatedPodName
	}
	pod, err := r.pods.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return TerminalTarget{}, podReadError(namespace, podName, err)
	}
	if sandbox.UID == "" || !metav1.IsControlledBy(pod, sandbox) || pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
		return TerminalTarget{}, terminalError(http.StatusConflict, terminalPodUnavailableMessage, nil)
	}

	if containerName == "" {
		if len(pod.Spec.Containers) == 0 {
			return TerminalTarget{}, terminalError(http.StatusConflict, terminalPodUnavailableMessage, nil)
		}
		containerName = pod.Spec.Containers[0].Name
	} else if !regularContainerExists(pod, containerName) {
		return TerminalTarget{}, terminalError(http.StatusBadRequest, terminalContainerNotFoundMessage, nil)
	}

	return TerminalTarget{
		Namespace:   namespace,
		SandboxName: sandbox.Name,
		PodName:     pod.Name,
		Container:   containerName,
	}, nil
}

func regularContainerExists(pod *corev1.Pod, name string) bool {
	for idx := range pod.Spec.Containers {
		if pod.Spec.Containers[idx].Name == name {
			return true
		}
	}
	return false
}

func podReady(pod *corev1.Pod) bool {
	for idx := range pod.Status.Conditions {
		condition := &pod.Status.Conditions[idx]
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func sandboxReadError(namespace, name string, err error) error {
	wrapped := fmt.Errorf("get Sandbox %s/%s: %w", namespace, name, err)
	switch {
	case apierrors.IsForbidden(err):
		return terminalError(http.StatusForbidden, terminalSandboxForbiddenMessage, wrapped)
	case apierrors.IsNotFound(err):
		return terminalError(http.StatusNotFound, terminalSandboxNotFoundMessage, wrapped)
	default:
		return terminalError(http.StatusServiceUnavailable, terminalSandboxUnavailableMessage, wrapped)
	}
}

func claimReadError(namespace, name string, err error) error {
	wrapped := fmt.Errorf("get SandboxClaim %s/%s: %w", namespace, name, err)
	switch {
	case apierrors.IsForbidden(err):
		return terminalError(http.StatusForbidden, terminalClaimForbiddenMessage, wrapped)
	case apierrors.IsNotFound(err):
		return terminalError(http.StatusConflict, terminalClaimUnavailableMessage, wrapped)
	default:
		return terminalError(http.StatusServiceUnavailable, terminalClaimTemporarilyUnavailableMessage, wrapped)
	}
}

func podReadError(namespace, name string, err error) error {
	wrapped := fmt.Errorf("get Pod %s/%s: %w", namespace, name, err)
	switch {
	case apierrors.IsForbidden(err):
		return terminalError(http.StatusForbidden, terminalPodForbiddenMessage, wrapped)
	case apierrors.IsNotFound(err):
		return terminalError(http.StatusConflict, terminalPodUnavailableMessage, wrapped)
	default:
		return terminalError(http.StatusServiceUnavailable, terminalPodTemporarilyUnavailableMessage, wrapped)
	}
}

func terminalError(status int, message string, err error) *TerminalError {
	return &TerminalError{Status: status, Message: message, Err: err}
}
