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
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestPortalVersionFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := run(context.Background(), []string{"--version"}, &stdout, &stderr)

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(stdout.String(), "agent-sandbox-portal, version"), stdout.String())
	assert.Empty(t, stderr.String())
}

func TestPortalRejectsPublicListenerBeforeLoadingKubeconfig(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	nonexistentKubeconfig := filepath.Join(t.TempDir(), "does-not-exist")

	err := run(context.Background(), []string{
		"--listen-address=0.0.0.0:8080",
		"--kubeconfig=" + nonexistentKubeconfig,
	}, &stdout, &stderr)

	require.ErrorContains(t, err, "not loopback")
	assert.NotContains(t, err.Error(), "kubeconfig")
}

func TestPortalFlagParseError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := run(context.Background(), []string{"--does-not-exist"}, &stdout, &stderr)

	require.ErrorContains(t, err, "flag provided but not defined")
	assert.Contains(t, stderr.String(), "Usage of agent-sandbox-portal:")
}

func TestRunMainDoesNotLogKubeconfigFailureDetails(t *testing.T) {
	var logs bytes.Buffer
	sensitivePath := filepath.Join(t.TempDir(), "sensitive-kubeconfig-location")
	logger := logzap.New(logzap.UseDevMode(false), logzap.WriteTo(&logs))

	exitCode := runMain(
		context.Background(),
		[]string{"--kubeconfig=" + sensitivePath},
		io.Discard,
		io.Discard,
		logger,
	)

	assert.Equal(t, 1, exitCode)
	rawLogs := logs.String()
	assert.NotEmpty(t, rawLogs, "the command failure must be logged")
	var entry struct {
		Message  string `json:"msg"`
		Error    string `json:"error"`
		Category string `json:"category"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(rawLogs)).Decode(&entry))
	assert.Equal(t, "run Sandbox portal", entry.Message)
	assert.Equal(t, "portal command failed", entry.Error)
	assert.Equal(t, "startup", entry.Category)
	assert.NotContains(t, rawLogs, sensitivePath)
	assert.NotContains(t, rawLogs, "sensitive-kubeconfig-location")
	assert.NotContains(t, rawLogs, "no such file or directory")
}

func TestListenLoopbackRejectsResolvedNonLoopbackAddress(t *testing.T) {
	bound := &stubListener{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8080}}

	listener, err := listenLoopback("localhost:8080", func(network string, address string) (net.Listener, error) {
		assert.Equal(t, "tcp", network)
		assert.Equal(t, "localhost:8080", address)
		return bound, nil
	})

	require.ErrorContains(t, err, "resolved listen address")
	require.ErrorContains(t, err, "not loopback")
	assert.Nil(t, listener)
	assert.True(t, bound.closed, "unsafe listener must be closed")
}

func TestListenLoopbackAcceptsResolvedLoopbackAddress(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{name: "IPv4", ip: "127.0.0.1"},
		{name: "IPv6", ip: "::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bound := &stubListener{addr: &net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 8080}}

			listener, err := listenLoopback("localhost:8080", func(string, string) (net.Listener, error) {
				return bound, nil
			})

			require.NoError(t, err)
			assert.Same(t, bound, listener)
			assert.False(t, bound.closed)
			require.NoError(t, listener.Close())
		})
	}
}

func TestMakefileBuildIncludesPortal(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	require.NoError(t, err)
	makefile := string(contents)

	assert.Contains(t, makefile, "build: build-controller build-sandbox-router build-sandboxd build-portal")
	assert.Contains(t, makefile, "bin/agent-sandbox-portal")
}

type stubListener struct {
	addr   net.Addr
	closed bool
}

func (l *stubListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (l *stubListener) Close() error {
	l.closed = true
	return nil
}

func (l *stubListener) Addr() net.Addr {
	return l.addr
}
