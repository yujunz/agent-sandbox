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
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
)

const testContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

const htmlLicenseHeader = `<!--
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
-->
`

const blockLicenseHeader = `/*
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
`

func TestServerRoutes(t *testing.T) {
	tests := []struct {
		method      string
		path        string
		status      int
		contentType string
	}{
		{http.MethodGet, "/", http.StatusOK, "text/html"},
		{http.MethodGet, "/app.css", http.StatusOK, "text/css"},
		{http.MethodGet, "/app.js", http.StatusOK, "text/javascript"},
		{http.MethodGet, "/healthz", http.StatusOK, "application/json"},
		{http.MethodGet, "/api/v1/sandboxes", http.StatusOK, "application/json"},
		{http.MethodPost, "/api/v1/sandboxes", http.StatusMethodNotAllowed, "application/json"},
		{http.MethodGet, "/api/v1/unknown", http.StatusNotFound, "application/json"},
		{http.MethodGet, "/missing", http.StatusNotFound, "text/plain"},
	}

	server := newHTTPTestServer(t, ServerOptions{})
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := request(t, server, tt.method, tt.path)
			defer response.Body.Close()
			assert.Equal(t, tt.status, response.StatusCode)
			assert.True(t, strings.HasPrefix(response.Header.Get("Content-Type"), tt.contentType), response.Header.Get("Content-Type"))
			if tt.status == http.StatusMethodNotAllowed {
				assert.Equal(t, http.MethodGet, response.Header.Get("Allow"))
			}
		})
	}
}

func TestServerSecurityHeaders(t *testing.T) {
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	})
	server := newHTTPTestServer(t, ServerOptions{Terminal: terminal})
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/app.css"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/api/v1/sandboxes"},
		{http.MethodPost, "/api/v1/sandboxes"},
		{http.MethodGet, "/api/v1/unknown"},
		{http.MethodGet, "/api/v1/namespaces/team-a/sandboxes/box-a/terminal"},
		{http.MethodGet, "/missing"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := request(t, server, tt.method, tt.path)
			defer response.Body.Close()
			assert.Equal(t, testContentSecurityPolicy, response.Header.Get("Content-Security-Policy"))
			assert.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
			assert.Equal(t, "no-referrer", response.Header.Get("Referrer-Policy"))
			assert.Equal(t, "DENY", response.Header.Get("X-Frame-Options"))
		})
	}
}

func TestInventoryResponseNoStore(t *testing.T) {
	server := newHTTPTestServer(t, ServerOptions{})
	for _, tt := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/sandboxes"},
		{http.MethodPost, "/api/v1/sandboxes"},
		{http.MethodGet, "/api/v1/unknown"},
	} {
		response := request(t, server, tt.method, tt.path)
		response.Body.Close()
		assert.Equal(t, "no-store", response.Header.Get("Cache-Control"), "%s %s", tt.method, tt.path)
	}
}

func TestInventoryFailureReturnsSanitized503(t *testing.T) {
	client := newSandboxClientset(t)
	client.PrependReactor("list", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("raw response includes team-a/secret-box token=credential")
	})
	var logs strings.Builder
	logger := funcr.New(func(prefix, args string) {
		logs.WriteString(prefix)
		logs.WriteString(args)
	}, funcr.Options{})
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions("team-a", false, time.Unix(200, 0)),
	)
	server := newHTTPTestServer(t, ServerOptions{Inventory: inventory, Log: logger})

	response := request(t, server, http.MethodGet, "/api/v1/sandboxes")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.JSONEq(t, `{"error":"Sandbox inventory is temporarily unavailable"}`, string(body))
	assert.NotContains(t, string(body), "secret-box")
	assert.Contains(t, logs.String(), "list Sandbox inventory")
	assert.NotContains(t, logs.String(), "secret-box")
	assert.NotContains(t, logs.String(), "credential")
}

func TestInventoryJSONSortOrder(t *testing.T) {
	client := newSandboxClientset(t,
		readySandbox("box-b", "team-b"),
		readySandbox("box-c", "team-a"),
		readySandbox("box-a", "team-a"),
	)
	inventory := NewInventory(
		client.AgentsV1beta1(),
		extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
		inventoryOptions("ignored", true, time.Unix(200, 0)),
	)
	server := newHTTPTestServer(t, ServerOptions{Inventory: inventory})

	response := request(t, server, http.MethodGet, "/api/v1/sandboxes")
	defer response.Body.Close()
	var got InventoryResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&got))
	require.Len(t, got.Sandboxes, 3)
	assert.Equal(t, []string{"team-a/box-a", "team-a/box-c", "team-b/box-b"}, []string{
		got.Sandboxes[0].Namespace + "/" + got.Sandboxes[0].Name,
		got.Sandboxes[1].Namespace + "/" + got.Sandboxes[1].Name,
		got.Sandboxes[2].Namespace + "/" + got.Sandboxes[2].Name,
	})
}

func TestServerTerminalRouteUsesConfiguredHandler(t *testing.T) {
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "team-a", r.PathValue("namespace"))
		assert.Equal(t, "box-a", r.PathValue("sandbox"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	})
	server := newHTTPTestServer(t, ServerOptions{Terminal: terminal})

	response := request(t, server, http.MethodGet, "/api/v1/namespaces/team-a/sandboxes/box-a/terminal")
	response.Body.Close()
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))

	response = request(t, server, http.MethodPost, "/api/v1/namespaces/team-a/sandboxes/box-a/terminal")
	defer response.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, response.StatusCode)
	assert.Equal(t, http.MethodGet, response.Header.Get("Allow"))
	assert.True(t, strings.HasPrefix(response.Header.Get("Content-Type"), "application/json"))
}

func TestServerTerminalResolutionErrorIsSanitized(t *testing.T) {
	fixture := newTerminalFixture()
	resolver, sandboxClient, _, _ := fixture.resolver(t, TerminalScope{Namespace: "team-a"}, time.Unix(500, 0))
	sandboxClient.PrependReactor("get", "sandboxes", terminalErrorReactor(apierrors.NewForbidden(
		schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"},
		"box-a",
		errors.New("raw API body token=credential"),
	)))
	factory := executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return nil, errors.New("executor must not be created")
	})
	terminal := NewTerminalHandler(t.Context(), resolver, factory, logr.Discard())
	server := newHTTPTestServer(t, ServerOptions{Terminal: terminal})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/namespaces/team-a/sandboxes/box-a/terminal", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", terminalOrigin(t, server.URL))

	response, err := server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.JSONEq(t, `{"error":"Sandbox access forbidden"}`, string(body))
	assert.NotContains(t, string(body), "credential")
	assert.NotContains(t, string(body), "raw API body")
}

func TestServerRejectsMissingInventory(t *testing.T) {
	handler, err := NewServer(ServerOptions{})

	assert.Nil(t, handler)
	assert.EqualError(t, err, "inventory is required")
}

func TestServerShellUsesExternalAssets(t *testing.T) {
	server := newHTTPTestServer(t, ServerOptions{})
	response := request(t, server, http.MethodGet, "/")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Contains(t, html, `<link rel="stylesheet" href="/app.css">`)
	assert.Contains(t, html, `<script src="/app.js" defer></script>`)
	assert.NotContains(t, html, "<style")
	assert.NotContains(t, html, "<script>")
	assert.Contains(t, html, `aria-live="polite"`)
	assert.Contains(t, html, `id="terminal-drawer"`)
}

func TestEmbeddedPortalAssets(t *testing.T) {
	server := newHTTPTestServer(t, ServerOptions{})
	tests := []struct {
		path        string
		contentType string
	}{
		{"/", "text/html"},
		{"/app.css", "text/css"},
		{"/app.js", "text/javascript"},
		{"/vendor/xterm.js", "text/javascript"},
		{"/vendor/xterm.css", "text/css"},
		{"/vendor/addon-fit.js", "text/javascript"},
		{"/vendor/LICENSE.xterm", "text/plain"},
		{"/vendor/LICENSE.addon-fit", "text/plain"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			response := request(t, server, http.MethodGet, tt.path)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.NotEmpty(t, body)
			assert.True(t, strings.HasPrefix(response.Header.Get("Content-Type"), tt.contentType), response.Header.Get("Content-Type"))
		})
	}

	response := request(t, server, http.MethodGet, "/")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	html := string(body)
	documentStart := strings.Index(html, "<!doctype html>")
	require.NotEqual(t, -1, documentStart)
	document := html[documentStart:]

	for _, reference := range []string{
		`href="/vendor/xterm.css"`,
		`src="/vendor/xterm.js"`,
		`src="/vendor/addon-fit.js"`,
		`href="/app.css"`,
		`src="/app.js"`,
	} {
		assert.Contains(t, document, reference)
	}
	assert.NotContains(t, document, "http://")
	assert.NotContains(t, document, "https://")
	assert.NotRegexp(t, regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>\s*[^<\s]`), document)
	assert.NotRegexp(t, regexp.MustCompile(`(?i)<style(?:\s|>)`), document)
	assert.NotRegexp(t, regexp.MustCompile(`(?i)\son[a-z]+\s*=`), document)
}

func TestPortalJavaScriptContract(t *testing.T) {
	body, err := fs.ReadFile(embeddedWeb, "web/app.js")
	require.NoError(t, err)
	javascript := string(body)

	for _, contract := range []string{
		"/api/v1/sandboxes",
		"5000",
		"visibilitychange",
		"/api/v1/namespaces/${encodeURIComponent(record.namespace)}/sandboxes/${encodeURIComponent(record.name)}/terminal",
		"namespace",
		"name",
		"claimName",
		"createdAt",
		"operatingMode",
		"ready",
		"user",
		"agent",
		"containers",
		"runtimeClass",
		"lifecycle",
		"podIPs",
		"serviceFQDN",
		"connections",
		"terminalEligible",
		"input",
		"resize",
	} {
		assert.Contains(t, javascript, contract)
	}
	assert.NotContains(t, javascript, ".innerHTML")
	assert.NotContains(t, javascript, ".outerHTML")
	assert.NotContains(t, javascript, "insertAdjacentHTML")
}

func TestServerFirstPartyAssetsHaveLicenseHeaders(t *testing.T) {
	server := newHTTPTestServer(t, ServerOptions{})
	tests := []struct {
		path       string
		wantHeader string
	}{
		{"/", htmlLicenseHeader},
		{"/app.css", blockLicenseHeader},
		{"/app.js", blockLicenseHeader},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			response := request(t, server, http.MethodGet, tt.path)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			assert.True(t, strings.HasPrefix(string(body), tt.wantHeader), "asset %s must begin with the repository license header", tt.path)
		})
	}
}

func newHTTPTestServer(t *testing.T, opts ServerOptions) *httptest.Server {
	t.Helper()
	if opts.Inventory == nil {
		client := newSandboxClientset(t)
		opts.Inventory = NewInventory(
			client.AgentsV1beta1(),
			extensionsfake.NewSimpleClientset().ExtensionsV1beta1(),
			inventoryOptions("team-a", false, time.Unix(200, 0)),
		)
	}
	if opts.Log.GetSink() == nil {
		opts.Log = logr.Discard()
	}
	handler, err := NewServer(opts)
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func request(t *testing.T, server *httptest.Server, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	return response
}
