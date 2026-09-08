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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func TestSPDYExecutorFactoryBuildsFixedShellRequest(t *testing.T) {
	requestReceived := make(chan *http.Request, 1)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived <- r.Clone(r.Context())
		http.Error(w, "upgrade unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(apiServer.Close)

	config := &rest.Config{
		Host:    apiServer.URL,
		APIPath: "/api",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &corev1.SchemeGroupVersion,
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		},
	}
	coreClient, err := typedcorev1.NewForConfig(config)
	require.NoError(t, err)
	factory := NewSPDYExecutorFactory(config, coreClient)

	executor, err := factory.NewExecutor(ExecRequest{
		Target: TerminalTarget{
			Namespace: "team-a",
			PodName:   "adopted-pod",
			Container: "workspace",
		},
		Command: []string{"/bin/sh"},
		Stdin:   true,
		Stdout:  true,
		TTY:     true,
	})
	require.NoError(t, err)
	require.Error(t, executor.StreamWithContext(context.Background(), remotecommand.StreamOptions{}))

	request := <-requestReceived
	assert.Equal(t, http.MethodPost, request.Method)
	assert.Equal(t, "/api/v1/namespaces/team-a/pods/adopted-pod/exec", request.URL.Path)
	assert.Equal(t, "workspace", request.URL.Query().Get("container"))
	assert.Equal(t, []string{"/bin/sh"}, request.URL.Query()["command"])
	assert.Equal(t, "true", request.URL.Query().Get("stdin"))
	assert.Equal(t, "true", request.URL.Query().Get("stdout"))
	assert.Equal(t, "true", request.URL.Query().Get("tty"))
	assert.NotContains(t, request.URL.RawQuery, "user-command")
}

func TestSPDYExecutorFactoryRejectsUnsafeRequests(t *testing.T) {
	factory := NewSPDYExecutorFactory(&rest.Config{}, nil)
	tests := []struct {
		name    string
		request ExecRequest
	}{
		{
			name: "arbitrary command",
			request: ExecRequest{
				Command: []string{"/bin/sh", "-c", "user-command"},
				Stdin:   true,
				Stdout:  true,
				TTY:     true,
			},
		},
		{
			name:    "stdin disabled",
			request: ExecRequest{Command: []string{"/bin/sh"}, Stdout: true, TTY: true},
		},
		{
			name:    "stdout disabled",
			request: ExecRequest{Command: []string{"/bin/sh"}, Stdin: true, TTY: true},
		},
		{
			name:    "TTY disabled",
			request: ExecRequest{Command: []string{"/bin/sh"}, Stdin: true, Stdout: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor, err := factory.NewExecutor(tt.request)
			assert.Nil(t, executor)
			assert.EqualError(t, err, "only an interactive /bin/sh terminal is allowed")
		})
	}
}
