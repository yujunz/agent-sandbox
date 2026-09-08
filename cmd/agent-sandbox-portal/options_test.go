/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateListenAddress(t *testing.T) {
	tests := []struct {
		address string
		wantErr bool
	}{
		{"127.0.0.1:8080", false},
		{"localhost:8080", false},
		{"[::1]:8080", false},
		{"0.0.0.0:8080", true},
		{"192.0.2.1:8080", true},
		{"portal.example:8080", true},
		{"127.0.0.1", true},
		{"unix:///tmp/portal.sock", true},
	}
	for _, tc := range tests {
		t.Run(tc.address, func(t *testing.T) {
			err := validateListenAddress(tc.address)
			assert.Equal(t, tc.wantErr, err != nil)
		})
	}
}

func TestValidateOptions(t *testing.T) {
	tests := []struct {
		name              string
		opts              options
		namespaceExplicit bool
		wantErr           string
	}{
		{"defaults", defaultOptions(), false, ""},
		{"namespace and all namespaces", options{listenAddress: "127.0.0.1:8080", namespace: "team-a", allNamespaces: true, userLabel: "sandbox.users.io/user", agentLabel: "sandbox.users.io/agent"}, true, "mutually exclusive"},
		{"invalid user label", options{listenAddress: "127.0.0.1:8080", userLabel: "bad label", agentLabel: "sandbox.users.io/agent"}, false, "--user-label"},
		{"router credentials", options{listenAddress: "127.0.0.1:8080", userLabel: "sandbox.users.io/user", agentLabel: "sandbox.users.io/agent", routerURL: "https://user@example.test"}, false, "credentials"},
		{"router query", options{listenAddress: "127.0.0.1:8080", userLabel: "sandbox.users.io/user", agentLabel: "sandbox.users.io/agent", routerURL: "https://example.test/base?q=x"}, false, "query"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOptions(&tc.opts, tc.namespaceExplicit)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestResolveConfigUsesCurrentContextNamespace(t *testing.T) {
	resolved := resolveTestConfig(t, "")
	assert.Equal(t, "dev", resolved.contextName)
	assert.Equal(t, "team-a", resolved.namespace)
}

func TestResolveConfigDefaultsNamespace(t *testing.T) {
	resolved := resolveTestConfig(t, "admin")
	assert.Equal(t, "admin", resolved.contextName)
	assert.Equal(t, "default", resolved.namespace)
}

func TestResolveConfigHonorsContextOverride(t *testing.T) {
	resolved := resolveTestConfig(t, "dev")
	assert.Equal(t, "dev", resolved.contextName)
	assert.Equal(t, "team-a", resolved.namespace)
}

func resolveTestConfig(t *testing.T, contextName string) *resolvedConfig {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "config")
	contents := []byte(`apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: local
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
users:
- name: user
  user: {}
contexts:
- name: dev
  context:
    cluster: local
    user: user
    namespace: team-a
- name: admin
  context:
    cluster: local
    user: user
`)
	require.NoError(t, os.WriteFile(kubeconfig, contents, 0600))
	opts := defaultOptions()
	fs := newFlagSet(&opts, &bytes.Buffer{})
	args := []string{"--kubeconfig", kubeconfig}
	if contextName != "" {
		args = append(args, "--context", contextName)
	}
	require.NoError(t, fs.Parse(args))
	returnConfig, err := resolveConfig(&opts, fs)
	require.NoError(t, err)
	return returnConfig
}
