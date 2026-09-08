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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestMakefileBuildIncludesPortal(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	require.NoError(t, err)
	makefile := string(contents)

	assert.Contains(t, makefile, "build: build-controller build-sandbox-router build-sandboxd build-portal")
	assert.Contains(t, makefile, "bin/agent-sandbox-portal")
}
