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
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const fixedTerminalShell = "/bin/sh"

// ExecRequest describes the fully validated Kubernetes exec stream.
type ExecRequest struct {
	Target  TerminalTarget
	Command []string
	Stdin   bool
	Stdout  bool
	TTY     bool
}

// ExecutorFactory creates an executor for a fully validated terminal target.
type ExecutorFactory interface {
	NewExecutor(ExecRequest) (remotecommand.Executor, error)
}

// SPDYExecutorFactory creates fixed-shell Kubernetes SPDY executors.
type SPDYExecutorFactory struct {
	restConfig *rest.Config
	core       typedcorev1.CoreV1Interface
}

// NewSPDYExecutorFactory constructs a factory using a private copy of the REST configuration.
func NewSPDYExecutorFactory(config *rest.Config, core typedcorev1.CoreV1Interface) *SPDYExecutorFactory {
	return &SPDYExecutorFactory{restConfig: rest.CopyConfig(config), core: core}
}

// NewExecutor builds an interactive /bin/sh request and rejects broader exec requests.
func (f *SPDYExecutorFactory) NewExecutor(execRequest ExecRequest) (remotecommand.Executor, error) {
	if len(execRequest.Command) != 1 || execRequest.Command[0] != fixedTerminalShell ||
		!execRequest.Stdin || !execRequest.Stdout || !execRequest.TTY {
		return nil, errors.New("only an interactive /bin/sh terminal is allowed")
	}
	if f.core == nil || f.core.RESTClient() == nil {
		return nil, errors.New("create Kubernetes exec stream: Kubernetes core client is required")
	}

	req := f.core.RESTClient().Post().
		Resource("pods").
		Name(execRequest.Target.PodName).
		Namespace(execRequest.Target.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: execRequest.Target.Container,
			Command:   []string{fixedTerminalShell},
			Stdin:     true,
			Stdout:    true,
			Stderr:    false,
			TTY:       true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(f.restConfig, http.MethodPost, req.URL())
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes exec stream: %w", err)
	}
	return executor, nil
}
