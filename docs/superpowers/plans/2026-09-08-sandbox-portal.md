# Sandbox Portal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a loopback-only `agent-sandbox-portal` Go binary that inventories Kubernetes Sandboxes and provides safe, one-click browser terminal sessions.

**Architecture:** A new `internal/portal` package converts typed Kubernetes resources into a stable JSON inventory, serves an embedded framework-free browser application, and bridges same-origin WebSockets to validated `pods/exec` sessions. A new `cmd/agent-sandbox-portal` command loads the operator's kubeconfig, preserves its RBAC boundary, constructs the typed clients, and serves only on a loopback listener.

**Tech Stack:** Go 1.26, client-go typed clients and `remotecommand`, `github.com/gorilla/websocket`, embedded HTML/CSS/JavaScript, vendored xterm.js 5.5.0 and `@xterm/addon-fit` 0.10.0.

**Spec:** `docs/superpowers/specs/2026-09-08-sandbox-portal-design.md`

## Global Constraints

- Keep `go 1.26.0` and `toolchain go1.26.4`; do not add a new Go module dependency because Gorilla WebSocket and client-go are already direct dependencies.
- Do not change CRDs, API types, controllers, generated clients, sandbox-router, Kubernetes manifests, or controller images.
- Accept only `localhost`, IPv4 loopback, or IPv6 loopback listener hosts; there is no public-listen override.
- Use the current kubeconfig context and its namespace by default; require `--all-namespaces` for cluster-wide listing.
- Treat the Sandbox `Ready` condition as authoritative and the owning SandboxClaim lifecycle as authoritative when present.
- The terminal endpoint accepts a Sandbox and optional container, never a Pod name or command; the only command is `/bin/sh` with TTY enabled.
- Re-read the Sandbox, owning claim, and Pod before every exec and require Ready, unexpired, Running, Pod Ready, and matching controller ownership.
- Never log credentials, Secrets, environment values, terminal input, terminal output, or raw API response bodies.
- Keep all browser assets embedded and offline-capable; scripts and styles must not use inline code or a CDN.
- Follow test-driven development: observe every focused test fail before adding its production behavior.
- The repository-wide baseline on macOS has one existing failure in `packages/sandboxd/pkg/pathutil TestSanitizePathAbsolutePathConfined`; the portal change must introduce no additional failure.

## File Structure

- `internal/portal/model.go`: JSON response, record, lifecycle, connection, and terminal protocol types.
- `internal/portal/inventory.go`: typed-client inventory reads, condition/metadata projection, claim matching, lifecycle calculations, sorting, and partial claim warnings.
- `internal/portal/connections.go`: safe router URL construction and connection-record projection.
- `internal/portal/server.go`: routes, JSON errors, security headers, cache policy, and embedded static-file serving.
- `internal/portal/terminal.go`: namespace/lifecycle/readiness/Pod/container validation and public terminal errors.
- `internal/portal/session.go`: WebSocket control protocol, terminal I/O adapters, resize queue, ping/pong, cancellation, and serialized writes.
- `internal/portal/executor.go`: fixed Kubernetes `pods/exec` request and SPDY executor construction.
- `internal/portal/web/index.html`: accessible dashboard and terminal drawer structure.
- `internal/portal/web/app.css`: responsive dashboard and xterm layout.
- `internal/portal/web/app.js`: polling, filtering, rendering, copy/open actions, countdowns, and terminal lifecycle.
- `internal/portal/web/vendor/`: pinned xterm.js, fit addon, CSS, and upstream licenses.
- `cmd/agent-sandbox-portal/options.go`: flag registration, kubeconfig/context/namespace resolution, label and URL validation, and loopback address validation.
- `cmd/agent-sandbox-portal/main.go`: client construction, listener/server lifetime, logging, signals, version output, and graceful shutdown.
- `cmd/agent-sandbox-portal/README.md`: operator build, access, flags, router, and RBAC documentation.
- `Makefile`: `build-portal` target and inclusion in `build`.

---

### Task 1: Command Configuration and Loopback Boundary

**Files:**
- Create: `cmd/agent-sandbox-portal/options.go`
- Create: `cmd/agent-sandbox-portal/options_test.go`

**Interfaces:**
- Produces: `type options`, `newFlagSet(*options, io.Writer) *flag.FlagSet`, `resolveConfig(*options, *flag.FlagSet) (*resolvedConfig, error)`, `validateListenAddress(string) error`, and `validateOptions(*options, bool) error`.
- Produces: `resolvedConfig.restConfig *rest.Config`, `contextName string`, `namespace string`, and `allNamespaces bool` for Task 8.

- [ ] **Step 1: Write failing flag, URL, label, and listener tests**

Create table-driven tests with these exact cases:

```go
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
		name string
		opts options
		namespaceExplicit bool
		wantErr string
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
			if tc.wantErr == "" { require.NoError(t, err); return }
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
```

Add `TestResolveConfigUsesCurrentContextNamespace`, `TestResolveConfigDefaultsNamespace`, and `TestResolveConfigHonorsContextOverride`. Each test writes a temporary kubeconfig containing contexts `dev/team-a` and `admin/""`, parses flags with `newFlagSet`, and asserts the resulting context and namespace without contacting an API server.

- [ ] **Step 2: Run the configuration tests and verify the package does not compile**

Run: `go test ./cmd/agent-sandbox-portal -run 'Test(Validate|Resolve)' -count=1`

Expected: FAIL because `options`, `validateListenAddress`, `validateOptions`, and `resolveConfig` do not exist.

- [ ] **Step 3: Implement the validated option surface and kubeconfig resolution**

Use these concrete types and defaults in `options.go`:

```go
const defaultListenAddress = "127.0.0.1:8080"

type options struct {
	listenAddress   string
	kubeconfig      string
	context         string
	namespace       string
	allNamespaces   bool
	userLabel       string
	agentLabel      string
	routerURL       string
	routerPathPrefix string
	printVersion    bool
}

type resolvedConfig struct {
	restConfig    *rest.Config
	contextName   string
	namespace     string
	allNamespaces bool
	routerURL     *url.URL
}

func defaultOptions() options {
	return options{
		listenAddress: defaultListenAddress,
		userLabel: "sandbox.users.io/user",
		agentLabel: "sandbox.users.io/agent",
		routerPathPrefix: "/sandboxes",
	}
}
```

Register every flag from the spec on a private `flag.FlagSet` with `ContinueOnError`. In `validateListenAddress`, use `net.SplitHostPort`, accept only case-insensitive `localhost` or `net.ParseIP(host).IsLoopback()`, and require a numeric port in `1..65535`. Validate label keys with `validation.IsQualifiedName`. Parse `--router-url` with `url.ParseRequestURI`; require `http` or `https`, a non-empty host, no `User`, `RawQuery`, or `Fragment`, and a router prefix beginning with exactly one `/` path boundary.

Implement kubeconfig resolution with client-go loading rules:

```go
func resolveConfig(opts *options, fs *flag.FlagSet) (*resolvedConfig, error) {
	namespaceExplicit := false
	fs.Visit(func(f *flag.Flag) { namespaceExplicit = namespaceExplicit || f.Name == "namespace" })
	if err := validateOptions(opts, namespaceExplicit); err != nil { return nil, err }

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.kubeconfig != "" { rules.ExplicitPath = opts.kubeconfig }
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	if namespaceExplicit { overrides.Context.Namespace = opts.namespace }
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := loader.ClientConfig()
	if err != nil { return nil, fmt.Errorf("load kubeconfig: %w", err) }
	namespace, _, err := loader.Namespace()
	if err != nil { return nil, fmt.Errorf("resolve namespace: %w", err) }
	if namespace == "" { namespace = metav1.NamespaceDefault }
	raw, err := loader.RawConfig()
	if err != nil { return nil, fmt.Errorf("read kubeconfig contexts: %w", err) }
	contextName := opts.context
	if contextName == "" { contextName = raw.CurrentContext }
	routerURL, err := parseRouterURL(opts.routerURL)
	if err != nil { return nil, err }
	return &resolvedConfig{restConfig: restConfig, contextName: contextName, namespace: namespace, allNamespaces: opts.allNamespaces, routerURL: routerURL}, nil
}
```

`parseRouterURL("")` returns `(nil, nil)`; for non-empty input it enforces the absolute HTTP(S), host, credential, query, and fragment rules used by `validateOptions` and returns the parsed URL.

- [ ] **Step 4: Run the configuration tests**

Run: `go test ./cmd/agent-sandbox-portal -run 'Test(Validate|Resolve)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the configuration boundary**

```bash
git add cmd/agent-sandbox-portal/options.go cmd/agent-sandbox-portal/options_test.go
git commit -m "feat: constrain portal access to local kubeconfig users"
```

---

### Task 2: Core Sandbox Inventory Projection

**Files:**
- Create: `internal/portal/model.go`
- Create: `internal/portal/inventory.go`
- Create: `internal/portal/inventory_test.go`

**Interfaces:**
- Consumes: generated `agentsv1beta1.SandboxInterface` and `extensionsv1beta1.SandboxClaimInterface` returned by namespace-scoped client getters.
- Produces: `SandboxClient`, `ClaimClient`, `InventoryOptions`, `Inventory`, `NewInventory`, and `(*Inventory).List(context.Context) (InventoryResponse, error)`.
- Produces: JSON types `InventoryResponse`, `SandboxRecord`, `ConditionSummary`, `ContainerRecord`, `PortRecord`, `LifecycleSummary`, and `ConnectionRecord`.

- [ ] **Step 1: Write failing inventory projection tests**

Define test helpers that create generated fake clientsets, then cover Ready True, Ready False, and missing Ready condition; pod-template label precedence; fallback Sandbox labels; multiple containers; runtime class; status Pod IPs and Service FQDN; and deterministic namespace/name sorting.

```go
func TestInventoryProjectsCoreSandboxFields(t *testing.T) {
	runtimeClass := "gvisor"
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "box-b", Namespace: "team-a", CreationTimestamp: metav1.NewTime(time.Unix(100, 0)),
			Labels: map[string]string{"sandbox.users.io/user": "fallback"},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{
					"sandbox.users.io/user": "alice", "sandbox.users.io/agent": "planner",
				}},
				Spec: corev1.PodSpec{RuntimeClassName: &runtimeClass, Containers: []corev1.Container{
					{Name: "workspace", Image: "registry.test/workspace:v1"},
					{Name: "sidecar", Image: "registry.test/sidecar:v2"},
				}},
			}},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{"10.0.0.8", "2001:db8::8"},
			ServiceFQDN: "box-b.team-a.svc.cluster.local",
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "DependenciesReady", Message: "ready"}},
		},
	}
	got := listInventory(t, "team-a", sb)
	require.Len(t, got.Sandboxes, 1)
	record := got.Sandboxes[0]
	assert.Equal(t, "alice", record.User)
	assert.Equal(t, "planner", record.Agent)
	assert.Equal(t, "gvisor", record.RuntimeClass)
	assert.Equal(t, "True", record.Ready.Status)
	assert.Equal(t, []string{"10.0.0.8", "2001:db8::8"}, record.PodIPs)
	assert.Equal(t, "box-b.team-a.svc.cluster.local", record.ServiceFQDN)
	assert.Equal(t, []string{"workspace", "sidecar"}, []string{record.Containers[0].Name, record.Containers[1].Name})
}
```

Add `TestInventoryMissingReadyIsUnknown`, `TestInventoryPodTemplateLabelsPrecedeSandboxLabels`, `TestInventoryFallsBackToSandboxLabels`, `TestInventoryDeletingAndSuspendedTerminalEligibility`, `TestInventoryUsesConfiguredNamespace`, `TestInventoryUsesNamespaceAllOnlyWhenEnabled`, and `TestInventorySortsByNamespaceThenName`. Assert unassigned user/agent encode as empty strings, unset operating mode projects as `Running`, and unset runtime class encodes as empty string so the UI can display its own localized fallback.

- [ ] **Step 2: Run the focused inventory tests and verify failure**

Run: `go test ./internal/portal -run 'TestInventory(Projects|Missing|PodTemplate|FallsBack|Sorts)' -count=1`

Expected: FAIL because the package and inventory types do not exist.

- [ ] **Step 3: Implement stable inventory types and core projection**

Define the response contract exactly once in `model.go`:

```go
type InventoryResponse struct {
	GeneratedAt   time.Time       `json:"generatedAt"`
	Context       string          `json:"context"`
	Namespace     string          `json:"namespace,omitempty"`
	AllNamespaces bool            `json:"allNamespaces"`
	Warnings      []string        `json:"warnings,omitempty"`
	Sandboxes     []SandboxRecord `json:"sandboxes"`
}

type SandboxRecord struct {
	Namespace        string             `json:"namespace"`
	Name             string             `json:"name"`
	ClaimName        string             `json:"claimName,omitempty"`
	CreatedAt        time.Time          `json:"createdAt"`
	OperatingMode    string             `json:"operatingMode"`
	Ready            ConditionSummary   `json:"ready"`
	User             string             `json:"user,omitempty"`
	Agent            string             `json:"agent,omitempty"`
	Containers       []ContainerRecord  `json:"containers"`
	RuntimeClass     string             `json:"runtimeClass,omitempty"`
	Lifecycle        LifecycleSummary   `json:"lifecycle"`
	PodIPs           []string           `json:"podIPs"`
	ServiceFQDN      string             `json:"serviceFQDN,omitempty"`
	Connections      []ConnectionRecord `json:"connections"`
	TerminalEligible bool               `json:"terminalEligible"`
}

type ConditionSummary struct {
	Status         string     `json:"status"`
	Reason         string     `json:"reason,omitempty"`
	Message        string     `json:"message,omitempty"`
	TransitionTime *time.Time `json:"transitionTime,omitempty"`
}
type PortRecord struct {
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol"`
	Port     int32  `json:"port"`
}
type ContainerRecord struct {
	Name  string       `json:"name"`
	Image string       `json:"image"`
	Ports []PortRecord `json:"ports"`
}
type LifecycleSummary struct {
	Source                  string     `json:"source,omitempty"`
	ExpiresAt               *time.Time `json:"expiresAt,omitempty"`
	TTLSecondsAfterFinished *int32     `json:"ttlSecondsAfterFinished,omitempty"`
	RetentionDeadline       *time.Time `json:"retentionDeadline,omitempty"`
	Expired                 bool       `json:"expired"`
}
type ConnectionRecord struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	URL       string `json:"url,omitempty"`
	Container string `json:"container,omitempty"`
	Port      int32  `json:"port,omitempty"`
}
```

Define the client boundaries and options in `inventory.go`:

```go
type SandboxClient interface { Sandboxes(namespace string) agentsv1beta1.SandboxInterface }
type ClaimClient interface { SandboxClaims(namespace string) extensionsv1beta1.SandboxClaimInterface }

type InventoryOptions struct {
	Context, Namespace, UserLabel, AgentLabel, RouterPathPrefix string
	AllNamespaces bool
	RouterURL *url.URL
	Now func() time.Time
}

type Inventory struct { sandboxes SandboxClient; claims ClaimClient; opts InventoryOptions }

func NewInventory(sandboxes SandboxClient, claims ClaimClient, opts InventoryOptions) *Inventory {
	if opts.Now == nil { opts.Now = time.Now }
	return &Inventory{sandboxes: sandboxes, claims: claims, opts: opts}
}
```

`List` chooses `metav1.NamespaceAll` only when `AllNamespaces` is true, lists Sandboxes first, and returns `fmt.Errorf("list Sandboxes: %w", err)` on failure. Project Ready using `meta.FindStatusCondition`, copy slices before sorting or returning them, and sort records with namespace as the primary key and name as the secondary key. Set `TerminalEligible` only when Ready is True, the object is not deleting, and the direct Sandbox shutdown time has not elapsed; Task 3 refines this with claim lifecycle.

- [ ] **Step 4: Run the core inventory tests**

Run: `go test ./internal/portal -run 'TestInventory(Projects|Missing|PodTemplate|FallsBack|Sorts)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the core inventory contract**

```bash
git add internal/portal/model.go internal/portal/inventory.go internal/portal/inventory_test.go
git commit -m "feat: expose Sandbox readiness and runtime inventory"
```

---

### Task 3: Claim Lifecycle Enrichment and Connection Links

**Files:**
- Modify: `internal/portal/inventory.go`
- Modify: `internal/portal/inventory_test.go`
- Create: `internal/portal/connections.go`
- Create: `internal/portal/connections_test.go`

**Interfaces:**
- Consumes: `InventoryOptions.RouterURL`, `RouterPathPrefix`, and model types from Task 2.
- Produces: `matchClaim(*Sandbox, []SandboxClaim) (*SandboxClaim, string)`, `lifecycleSummary(*Sandbox, *SandboxClaim, time.Time) LifecycleSummary`, and `connectionRecords(*Sandbox, *url.URL, string) []ConnectionRecord`.

- [ ] **Step 1: Write failing lifecycle, claim matching, warning, and router tests**

Add these named cases:

```go
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
}

func TestConnectionRecordsEscapeRouterSegments(t *testing.T) {
	base, err := url.Parse("https://router.example/base")
	require.NoError(t, err)
	sb := readySandbox("box-a", "team-a")
	sb.Spec.PodTemplate.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{Name: "web", ContainerPort: 8080},
		{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
	}
	got := connectionRecords(sb, base, "/sandboxes")
	assert.Contains(t, got, ConnectionRecord{Kind: "router", Label: "web", URL: "https://router.example/base/sandboxes/team-a/box-a/8080/", Container: "workspace", Port: 8080})
	assert.NotContains(t, connectionURLs(got), "https://router.example/base/sandboxes/team-a/box-a/53/")
}
```

Also add tests for:

- direct Sandbox shutdown and no-lifecycle objects;
- `ttlSecondsAfterFinished` using `Finished=True.lastTransitionTime` and `internal/lifecycle.ExpireAt`;
- unfinished TTL showing the policy without a retention deadline;
- owner reference winning over a status-only match;
- exactly one status-only match succeeding;
- two status-only matches producing an ambiguity warning and no claim selection;
- claim-list Forbidden, NotFound, and generic errors returning Sandbox records with fixed, sanitized warning text;
- an owner-referenced Sandbox with unavailable claim data retaining its claim name but reporting `terminalEligible=false`;
- Pod IP and Service FQDN connection records with no router URL;
- omitted, zero, and UDP ports never producing router links;
- router base paths and trailing slashes producing one canonical slash between segments and one trailing slash.

- [ ] **Step 2: Run the lifecycle and connection tests and verify failure**

Run: `go test ./internal/portal -run 'Test(InventoryClaim|InventoryDirect|InventoryTTL|InventoryClaimList|MatchClaim|ConnectionRecords)' -count=1`

Expected: FAIL because claim enrichment and connection helpers are absent.

- [ ] **Step 3: Implement deterministic claim matching and lifecycle calculation**

Use the controller owner reference first, checking group, kind, name, and UID. Use status-only matching only when there is no matching owner reference and exactly one same-namespace claim has `claim.Status.SandboxStatus.Name == sandbox.Name`.

```go
func lifecycleSummary(sb *sandboxv1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim, now time.Time) LifecycleSummary {
	shutdown := sb.Spec.ShutdownTime
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
	expiresAt := lifecycle.ExpireAt(shutdown, ttl, finished)
	var retentionDeadline *time.Time
	if ttl != nil && finished != nil {
		finishedAt := finished.LastTransitionTime.Time
		deadline := finishedAt.Add(time.Duration(*ttl) * time.Second)
		retentionDeadline = &deadline
	}
	if shutdown == nil && ttl == nil { source = "" }
	return LifecycleSummary{Source: source, ExpiresAt: expiresAt, TTLSecondsAfterFinished: ttl, RetentionDeadline: retentionDeadline, Expired: expiresAt != nil && !now.Before(*expiresAt)}
}
```

On claim list failure, continue with direct Sandbox data and append exactly one of: `SandboxClaim access forbidden; lifecycle enrichment is unavailable`, `SandboxClaim API is unavailable; lifecycle enrichment is disabled`, or `SandboxClaim listing failed; lifecycle enrichment is temporarily unavailable`. Do not include `err.Error()` in the response. Merge per-Sandbox ambiguity warnings after the claim-list warning.

- [ ] **Step 4: Implement safe connection records**

Always add one `podIP` record per non-empty status IP and one `serviceFQDN` record for a non-empty FQDN. Add router records only for positive TCP ports when a base URL exists.

```go
func routerLink(base *url.URL, prefix, namespace, sandbox string, port int32) (string, error) {
	joined, err := url.JoinPath(base.String(), strings.Trim(prefix, "/"), namespace, sandbox, strconv.FormatInt(int64(port), 10))
	if err != nil { return "", fmt.Errorf("build router link: %w", err) }
	return strings.TrimSuffix(joined, "/") + "/", nil
}
```

If `routerLink` returns an error, omit only that router connection. Kubernetes namespace and Sandbox names are already DNS-safe path segments; test a router base URL containing an escaped path segment to prevent double escaping of the configured base path.

- [ ] **Step 5: Run all internal portal inventory tests**

Run: `go test ./internal/portal -run 'Test(Inventory|MatchClaim|ConnectionRecords)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit lifecycle and connection enrichment**

```bash
git add internal/portal/inventory.go internal/portal/inventory_test.go internal/portal/connections.go internal/portal/connections_test.go
git commit -m "feat: surface Sandbox lifecycle and connections"
```

---

### Task 4: HTTP API, Embedded Assets, and Security Headers

**Files:**
- Create: `internal/portal/server.go`
- Create: `internal/portal/server_test.go`
- Create: `internal/portal/web/index.html`
- Create: `internal/portal/web/app.css`
- Create: `internal/portal/web/app.js`

**Interfaces:**
- Consumes: `Inventory.List` and `InventoryResponse` from Tasks 2-3.
- Produces: `type ServerOptions struct { Inventory *Inventory; Terminal http.Handler; Log logr.Logger }` and `NewServer(ServerOptions) (http.Handler, error)`.
- Produces: `writeAPIError(http.ResponseWriter, int, string)` with JSON shape `{"error":"safe message"}`.

- [ ] **Step 1: Write failing server route and middleware tests**

Use `httptest.NewServer` and fake typed clients to assert this matrix:

```go
func TestServerRoutes(t *testing.T) {
	tests := []struct{ method, path string; status int; contentType string }{
		{"GET", "/", http.StatusOK, "text/html"},
		{"GET", "/app.css", http.StatusOK, "text/css"},
		{"GET", "/app.js", http.StatusOK, "text/javascript"},
		{"GET", "/healthz", http.StatusOK, "application/json"},
		{"GET", "/api/v1/sandboxes", http.StatusOK, "application/json"},
		{"POST", "/api/v1/sandboxes", http.StatusMethodNotAllowed, "application/json"},
		{"GET", "/api/v1/unknown", http.StatusNotFound, "application/json"},
		{"GET", "/missing", http.StatusNotFound, "text/plain"},
	}
	for _, tc := range tests { assertRoute(t, tc.method, tc.path, tc.status, tc.contentType) }
}
```

Add `TestServerSecurityHeaders` asserting `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and `X-Frame-Options: DENY` on HTML, assets, health, API success, and API errors. Add `TestInventoryResponseNoStore`, `TestInventoryFailureReturnsSanitized503`, and `TestInventoryJSONSortOrder`.

- [ ] **Step 2: Run server tests and verify failure**

Run: `go test ./internal/portal -run 'Test(Server|InventoryResponse|InventoryFailure|InventoryJSON)' -count=1`

Expected: FAIL because `NewServer` and embedded assets do not exist.

- [ ] **Step 3: Implement the HTTP surface with a private ServeMux**

Embed the whole web directory recursively and serve its sub-files without directory listing:

```go
//go:embed web
var embeddedWeb embed.FS

func NewServer(opts ServerOptions) (http.Handler, error) {
	if opts.Inventory == nil { return nil, errors.New("inventory is required") }
	webRoot, err := fs.Sub(embeddedWeb, "web")
	if err != nil { return nil, fmt.Errorf("open embedded portal assets: %w", err) }
	s := &server{inventory: opts.Inventory, log: opts.Log}
	mux := http.NewServeMux()
	mux.Handle("/healthz", onlyMethod(http.MethodGet, http.HandlerFunc(healthHandler)))
	if opts.Terminal != nil {
		mux.Handle("/api/v1/namespaces/{namespace}/sandboxes/{sandbox}/terminal", apiMethod(http.MethodGet, opts.Terminal))
	}
	mux.Handle("/api/v1/sandboxes", apiMethod(http.MethodGet, http.HandlerFunc(s.inventoryHandler)))
	mux.HandleFunc("/api/v1/", apiNotFound)
	mux.Handle("GET /", exactStaticFiles(webRoot))
	return securityHeaders(mux), nil
}
```

Register the terminal route before the API catch-all. `apiMethod` returns a JSON 405 with an `Allow` header, while `apiNotFound` returns a JSON 404 for every unknown API path. `exactStaticFiles` maps `/` to `index.html`, permits existing embedded files, and returns `http.NotFound` for missing files instead of serving a directory index. Marshal inventory with `json.NewEncoder`; on failure return the fixed message `Sandbox inventory is temporarily unavailable` and log only the contextual error plus no resource content. Set `Cache-Control: no-store` on every `/api/` response.

Create an external-script HTML shell containing the header, summary region, filter controls, table headings, status region with `aria-live="polite"`, and terminal drawer. `app.js` should perform one fetch and render an empty state at this stage; Task 7 completes the browser interactions.

- [ ] **Step 4: Run the server tests**

Run: `go test ./internal/portal -run 'Test(Server|InventoryResponse|InventoryFailure|InventoryJSON)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the secured local HTTP surface**

```bash
git add internal/portal/server.go internal/portal/server_test.go internal/portal/web/index.html internal/portal/web/app.css internal/portal/web/app.js
git commit -m "feat: serve the Sandbox portal API and embedded shell"
```

---

### Task 5: Fresh Terminal Target Validation

**Files:**
- Create: `internal/portal/terminal.go`
- Create: `internal/portal/terminal_test.go`

**Interfaces:**
- Consumes: `SandboxClient` and `ClaimClient` from Task 2 plus client-go `typedcorev1.PodsGetter`.
- Produces: `TerminalScope`, `TerminalTarget`, `TerminalResolver`, `NewTerminalResolver(SandboxClient, ClaimClient, typedcorev1.PodsGetter, TerminalScope, func() time.Time) *TerminalResolver`, `(*TerminalResolver).Resolve(context.Context, string, string, string) (TerminalTarget, error)`, and `*TerminalError` with safe HTTP status/message.

- [ ] **Step 1: Write failing terminal authorization and target tests**

Build fake Sandbox, claim, and Kubernetes clientsets and add one table whose success fixture contains:

```go
want := TerminalTarget{Namespace: "team-a", SandboxName: "box-a", PodName: "adopted-pod", Container: "workspace"}
```

The Sandbox must be Ready=True, carry `agents.x-k8s.io/pod-name=adopted-pod`, and own a Running Pod with `PodReady=True`; both objects must have non-empty matching UIDs in their controller reference.

Name and assert these rejection cases and statuses:

- `namespace outside single namespace scope`: 404;
- `Sandbox missing`: 404;
- `Sandbox list/get forbidden`: 403;
- `Sandbox deleting`: 409;
- `Ready missing`, `Ready False`, and `operatingMode Suspended`: 409;
- direct Sandbox shutdown elapsed: 410;
- owner-referenced claim shutdown or finished TTL elapsed: 410;
- owner-referenced claim missing or forbidden: 409 or 403 respectively;
- Pod missing: 409;
- Pod controlled by another UID: 409;
- Pod Pending, Succeeded, Failed, or Running without PodReady=True: 409;
- named container absent: 400;
- empty container query selects the first regular live Pod container;
- no regular containers: 409;
- non-empty legacy Pod annotation wins; empty annotation falls back to the Sandbox name.

Use a fixed `Now: func() time.Time { return time.Unix(500, 0) }` so expiry assertions are deterministic.

- [ ] **Step 2: Run terminal resolver tests and verify failure**

Run: `go test ./internal/portal -run TestTerminalResolver -count=1`

Expected: FAIL because terminal resolver types do not exist.

- [ ] **Step 3: Implement conservative fresh-read validation**

Use these exact boundaries:

```go
type TerminalScope struct { Namespace string; AllNamespaces bool }
type TerminalTarget struct { Namespace, SandboxName, PodName, Container string }
type TerminalError struct { Status int; Message string; Err error }
func (e *TerminalError) Error() string { return e.Message }
func (e *TerminalError) Unwrap() error { return e.Err }

type TerminalResolver struct {
	sandboxes SandboxClient
	claims ClaimClient
	pods typedcorev1.PodsGetter
	scope TerminalScope
	now func() time.Time
}
```

In `Resolve`, validate the namespace with `validation.IsDNS1123Label` and the Sandbox name with `validation.IsDNS1123Subdomain` before any API call. Get the Sandbox in that namespace. If its controller reference is a `SandboxClaim`, get that exact same-namespace claim and reject the session when verification fails. Handle a nil claim lifecycle safely and compute authoritative expiry with:

```go
var shutdown *metav1.Time
var ttl *int32
if claim.Spec.Lifecycle != nil {
	shutdown = claim.Spec.Lifecycle.ShutdownTime
	ttl = claim.Spec.Lifecycle.TTLSecondsAfterFinished
}
finished := lifecycle.FinishedCondition(claim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
expiresAt := lifecycle.ExpireAt(shutdown, ttl, finished)
```

For a direct Sandbox, compute expiry from `sandbox.Spec.ShutdownTime`. Require non-deleting, unexpired, `OperatingMode != Suspended`, and Ready=True. Resolve the Pod name, get it, require `metav1.IsControlledBy(pod, sandbox)`, phase Running, PodReady=True, and a matching regular container.

Map API `Forbidden` to 403, `NotFound` to 404 only for the requested Sandbox, and dependency disappearance to 409. Map other Kubernetes errors to 503. Return fixed public messages such as `Sandbox is not Ready`, `Sandbox has expired`, and `backing Pod is unavailable`; preserve the wrapped error for structured server logging but never serialize it.

- [ ] **Step 4: Run terminal resolver tests**

Run: `go test ./internal/portal -run TestTerminalResolver -count=1`

Expected: PASS.

- [ ] **Step 5: Commit terminal target validation**

```bash
git add internal/portal/terminal.go internal/portal/terminal_test.go
git commit -m "feat: validate Sandbox terminal targets before exec"
```

---

### Task 6: WebSocket Terminal Bridge and Kubernetes Exec

**Files:**
- Create: `internal/portal/executor.go`
- Create: `internal/portal/executor_test.go`
- Create: `internal/portal/session.go`
- Create: `internal/portal/session_test.go`
- Modify: `internal/portal/server.go`
- Modify: `internal/portal/server_test.go`

**Interfaces:**
- Consumes: `TerminalResolver.Resolve` and `TerminalTarget` from Task 5.
- Produces: `ExecRequest`, `ExecutorFactory`, `SPDYExecutorFactory`, `NewSPDYExecutorFactory(*rest.Config, typedcorev1.CoreV1Interface) *SPDYExecutorFactory`, `TerminalHandler`, and `NewTerminalHandler(context.Context, *TerminalResolver, ExecutorFactory, logr.Logger) http.Handler`.
- Produces browser input controls `ClientControl{Type, Data, Cols, Rows}` and server controls `ServerControl{Type, Message}`.

- [ ] **Step 1: Write failing fixed-exec and session protocol tests**

Define the test seam:

```go
type ExecRequest struct {
	Target TerminalTarget
	Command []string
	Stdin, Stdout, TTY bool
}

type ExecutorFactory interface {
	NewExecutor(ExecRequest) (remotecommand.Executor, error)
}
```

Add `TestSPDYExecutorFactoryBuildsFixedShellRequest` with an `httptest.Server`-backed REST client. Assert the request URL path is `/api/v1/namespaces/team-a/pods/adopted-pod/exec`, the query contains `container=workspace`, `command=/bin/sh`, `stdin=true`, `stdout=true`, `tty=true`, and does not contain a user-supplied command.

Add real WebSocket tests around `httptest.NewServer(NewServer(...))`:

```go
func TestTerminalSessionBridgesInputOutputAndResize(t *testing.T) {
	conn := dialTerminal(t, serverURL, "http://"+serverHost, "team-a", "box-a", "workspace")
	require.NoError(t, conn.WriteJSON(ClientControl{Type: "resize", Cols: 120, Rows: 40}))
	require.NoError(t, conn.WriteJSON(ClientControl{Type: "input", Data: "echo ready\n"}))
	messageType, output, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, messageType)
	assert.Equal(t, "ready\r\n", string(output))
	assert.Equal(t, remotecommand.TerminalSize{Width: 120, Height: 40}, fakeExecutor.Size())
	assert.Equal(t, "echo ready\n", fakeExecutor.Input())
}
```

Also add:

- `TestTerminalRejectsMissingCrossOriginAndMalformedOriginBeforeUpgrade`;
- `TestTerminalRejectsOversizedAndUnknownControlMessages`;
- `TestTerminalUsesBinaryFramesForOutputAndJSONForExit`;
- `TestTerminalStreamErrorSendsSafeControlMessage`;
- `TestTerminalDisconnectCancelsExec`;
- `TestTerminalServerShutdownCancelsExec`;
- `TestTerminalSessionsAreIndependent` with two simultaneous connections;
- `TestResizeQueueCoalescesWithoutBlocking`.

- [ ] **Step 2: Run terminal streaming tests and verify failure**

Run: `go test ./internal/portal -run 'Test(SPDY|TerminalSession|TerminalRejects|TerminalUses|TerminalStream|TerminalDisconnect|TerminalServer|TerminalSessions|ResizeQueue)' -count=1`

Expected: FAIL because the executor factory and WebSocket session do not exist.

- [ ] **Step 3: Implement the fixed SPDY exec factory**

`SPDYExecutorFactory` stores a copied `*rest.Config` and `typedcorev1.CoreV1Interface`. Reject any request whose command is not exactly `[]string{"/bin/sh"}` or whose stream flags differ from stdin/stdout/TTY true. Build the request as follows:

```go
req := f.core.RESTClient().Post().
	Resource("pods").
	Name(execRequest.Target.PodName).
	Namespace(execRequest.Target.Namespace).
	SubResource("exec").
	VersionedParams(&corev1.PodExecOptions{
		Container: execRequest.Target.Container,
		Command: []string{"/bin/sh"},
		Stdin: true, Stdout: true, Stderr: false, TTY: true,
	}, scheme.ParameterCodec)
return remotecommand.NewSPDYExecutor(f.restConfig, http.MethodPost, req.URL())
```

Wrap construction failures with `fmt.Errorf("create Kubernetes exec stream: %w", err)`.

- [ ] **Step 4: Implement the bounded WebSocket protocol and cancellation**

Use `websocket.Upgrader` with a 4 KiB handshake buffer and a `sameOrigin` function that parses `Origin`, requires `http` or `https`, and compares `origin.Host` exactly to `r.Host`. Reject missing, malformed, or cross-origin requests before resolving the target; keep the same function as `Upgrader.CheckOrigin` as a defense-in-depth recheck. Resolve the target before calling `Upgrade`, then create the executor with:

```go
ExecRequest{Target: target, Command: []string{"/bin/sh"}, Stdin: true, Stdout: true, TTY: true}
```

Define `ClientControl` and `ServerControl` with explicit JSON tags. Set a 64 KiB read limit, a 60-second pong deadline, and a 30-second ping interval. Feed validated input through an `io.Pipe`; feed terminal sizes through a capacity-one queue implementing `remotecommand.TerminalSizeQueue`; and run:

```go
err := executor.StreamWithContext(sessionCtx, remotecommand.StreamOptions{
	Stdin: stdinReader,
	Stdout: &websocketOutput{writer: serializedWriter},
	Stderr: nil,
	Tty: true,
	TerminalSizeQueue: resizeQueue,
})
```

`websocketOutput.Write` copies the byte slice and emits a binary frame under one writer mutex. Input and resize are the only accepted client control types; require `1 <= cols,rows <= 65535`. The newest resize replaces any queued resize without blocking. All exit, error, and ping writes use the same writer mutex. Cancel the session and close both pipe ends on client disconnect, server context cancellation, or exec completion. Emit `{"type":"exit"}` for nil exec completion and `{"type":"error","message":"terminal session ended"}` for stream failure. Log only a fixed result category (`clean-exit`, `disconnect`, `cancelled`, `exec-error`) and resource coordinates; never pass terminal stream errors to the logger because remote errors can contain workload-controlled text.

Register `TerminalHandler` on the exact Go 1.22 ServeMux path from Task 4 before `/api/v1/`.

- [ ] **Step 5: Run terminal tests under the race detector**

Run: `go test -race ./internal/portal -run 'Test(SPDY|Terminal)' -count=1`

Expected: PASS with no race report.

- [ ] **Step 6: Commit the terminal bridge**

```bash
git add internal/portal/executor.go internal/portal/executor_test.go internal/portal/session.go internal/portal/session_test.go internal/portal/server.go internal/portal/server_test.go
git commit -m "feat: bridge browser terminals to Kubernetes exec"
```

---

### Task 7: Complete the Offline Dashboard UI

**Files:**
- Modify: `internal/portal/web/index.html`
- Modify: `internal/portal/web/app.css`
- Modify: `internal/portal/web/app.js`
- Create: `internal/portal/web/vendor/xterm.js`
- Create: `internal/portal/web/vendor/xterm.css`
- Create: `internal/portal/web/vendor/addon-fit.js`
- Create: `internal/portal/web/vendor/LICENSE.xterm`
- Create: `internal/portal/web/vendor/LICENSE.addon-fit`
- Modify: `internal/portal/server_test.go`

**Interfaces:**
- Consumes: the exact JSON model from Task 2 and terminal protocol from Task 6.
- Produces: browser functions `fetchInventory`, `renderDashboard`, `formatRemaining`, `copyValue`, `openTerminal`, `connectTerminal`, and `closeTerminal` under one private module scope.

- [ ] **Step 1: Extend asset contract tests before changing the UI**

Add `TestEmbeddedPortalAssets` asserting all eight paths (`/`, `/app.css`, `/app.js`, and the five vendored files) return 200 with non-empty bodies and correct content types. Assert `index.html` contains no `http://`, `https://`, inline `<script>`, inline `<style>`, or inline event attributes; it must reference `/vendor/xterm.css`, `/vendor/xterm.js`, `/vendor/addon-fit.js`, `/app.css`, and `/app.js`.

Add `TestPortalJavaScriptContract` that reads the embedded `app.js` and asserts it contains the API path, five-second poll interval, `visibilitychange`, the terminal WebSocket path template, all record field names, and input/resize control names. Assert it contains none of `.innerHTML`, `.outerHTML`, or `insertAdjacentHTML`. This source-level guard complements manual browser behavior and protects the CSP/XSS boundary.

- [ ] **Step 2: Run the asset tests and verify failure**

Run: `go test ./internal/portal -run 'Test(EmbeddedPortalAssets|PortalJavaScriptContract)' -count=1`

Expected: FAIL because vendor assets and complete interactions are absent.

- [ ] **Step 3: Vendor pinned xterm distributions and licenses**

Download only the pinned npm tarballs into a temporary directory outside the repository:

```bash
npm pack --pack-destination /private/tmp/agent-sandbox-portal-assets @xterm/xterm@5.5.0
npm pack --pack-destination /private/tmp/agent-sandbox-portal-assets @xterm/addon-fit@0.10.0
```

Extract `package/lib/xterm.js`, `package/css/xterm.css`, and the xterm license from the first tarball. Extract `package/lib/addon-fit.js` and the addon license from the second tarball. Add only those five files under `internal/portal/web/vendor`; do not add npm manifests, tarballs, caches, source maps, or `node_modules`. Verify the license files name the upstream xterm.js project and the Microsoft copyright before staging.

- [ ] **Step 4: Implement accessible inventory rendering and polling**

Use DOM creation plus `textContent`, never record-derived HTML strings. `fetchInventory` uses `fetch('/api/v1/sandboxes', {cache: 'no-store'})`, renders fixed error/partial-warning states, and stores the latest response. `renderDashboard` filters a lower-cased concatenation of namespace, name, claim, user, agent, images, runtime class, and Ready reason, then applies the selected `all|ready|unready|unknown` status filter.

Render these exact outputs:

- summary totals for all, Ready, non-Ready, and expiry within 15 minutes;
- identity as `namespace/name`, with claim beneath it when present;
- Ready badge plus reason and a title/expandable message;
- unassigned user/agent as `Unassigned`;
- first image visibly and remaining named images in a details list;
- empty runtime class as `Cluster default`;
- absolute expiry plus `formatRemaining(expiresAt, Date.now())`, or TTL `N seconds after finish`, or `No expiry`;
- copy buttons for Pod IP and Service FQDN records and safe `_blank` links with `rel="noopener noreferrer"` for router records;
- a disabled Terminal button with an explanatory label when `terminalEligible` is false.

Schedule refresh with `window.setInterval(fetchInventory, 5000)`. Pause by checking `document.hidden` and refresh immediately after `visibilitychange` makes the page visible. Update visible countdown text once per second from cached absolute timestamps without calling the API.

- [ ] **Step 5: Implement terminal drawer lifecycle**

Instantiate `Terminal`, load `FitAddon`, and call `fit()` after the drawer opens. Derive `ws:` or `wss:` from `window.location`, encode namespace/name/container with `encodeURIComponent`, and connect only to the same host. On terminal data send `JSON.stringify({type: 'input', data})`; on resize send `{type: 'resize', cols, rows}`. Feed binary `ArrayBuffer` output to xterm and parse text frames as server control messages.

`openTerminal(record)` selects `record.containers[0].name` and connects immediately. Populate the container selector from that record; changing it closes the old socket and reconnects explicitly. `closeTerminal()` closes the socket, disposes terminal handlers, clears the terminal, and hides the drawer. Inventory refreshes must not call either terminal function. Show connection, exit, and error states as text outside the terminal canvas.

- [ ] **Step 6: Run server and race tests**

Run: `go test -race ./internal/portal -count=1`

Expected: PASS with embedded assets served and no race report.

- [ ] **Step 7: Commit the complete offline UI**

```bash
git add internal/portal/web internal/portal/server_test.go
git commit -m "feat: add the offline Sandbox dashboard UI"
```

---

### Task 8: Wire the Binary, Build Target, and Operator Documentation

**Files:**
- Create: `cmd/agent-sandbox-portal/main.go`
- Create: `cmd/agent-sandbox-portal/main_test.go`
- Create: `cmd/agent-sandbox-portal/README.md`
- Modify: `Makefile:72-84`

**Interfaces:**
- Consumes: `resolvedConfig`, `Inventory`, `TerminalResolver`, `SPDYExecutorFactory`, `TerminalHandler`, and `NewServer` from Tasks 1-7.
- Produces: executable `bin/agent-sandbox-portal`, `run(context.Context, []string, io.Writer, io.Writer) error`, and Make target `build-portal`.

- [ ] **Step 1: Write failing command integration tests**

Add `TestPortalVersionFlag` asserting `run(ctx, []string{"--version"}, stdout, stderr)` returns nil and prints `agent-sandbox-portal, version`. Add `TestPortalRejectsPublicListenerBeforeLoadingKubeconfig` with `--listen-address=0.0.0.0:8080` and a nonexistent kubeconfig, asserting the loopback error wins. Add `TestPortalFlagParseError` asserting unknown flags return an error and usage on stderr. These tests must not open a network listener or read the developer's kubeconfig.

Add `TestMakefileBuildIncludesPortal` that reads `../../Makefile` from the command package test working directory and asserts `build: build-controller build-sandbox-router build-sandboxd build-portal` and the output path `bin/agent-sandbox-portal` are present.

- [ ] **Step 2: Run command tests and verify failure**

Run: `go test ./cmd/agent-sandbox-portal -run 'TestPortal|TestMakefile' -count=1`

Expected: FAIL because `run`, `main`, and `build-portal` do not exist.

- [ ] **Step 3: Construct clients and application components in `run`**

Import all client auth plugins with `_ "k8s.io/client-go/plugin/pkg/client/auth"`. Parse the private flag set, return version output before kubeconfig loading, resolve options, and create clients with contextual error wrapping:

```go
sandboxClientset, err := sandboxclientset.NewForConfig(cfg.restConfig)
if err != nil { return fmt.Errorf("create Sandbox client: %w", err) }
claimClientset, err := claimclientset.NewForConfig(cfg.restConfig)
if err != nil { return fmt.Errorf("create SandboxClaim client: %w", err) }
coreClientset, err := kubernetes.NewForConfig(cfg.restConfig)
if err != nil { return fmt.Errorf("create Kubernetes client: %w", err) }

inventory := portal.NewInventory(
	sandboxClientset.AgentsV1beta1(),
	claimClientset.ExtensionsV1beta1(),
	portal.InventoryOptions{Context: cfg.contextName, Namespace: cfg.namespace, AllNamespaces: cfg.allNamespaces, UserLabel: opts.userLabel, AgentLabel: opts.agentLabel, RouterURL: cfg.routerURL, RouterPathPrefix: opts.routerPathPrefix},
)
resolver := portal.NewTerminalResolver(sandboxClientset.AgentsV1beta1(), claimClientset.ExtensionsV1beta1(), coreClientset.CoreV1(), portal.TerminalScope{Namespace: cfg.namespace, AllNamespaces: cfg.allNamespaces}, time.Now)
factory := portal.NewSPDYExecutorFactory(cfg.restConfig, coreClientset.CoreV1())
terminal := portal.NewTerminalHandler(ctx, resolver, factory, log.WithName("terminal"))
handler, err := portal.NewServer(portal.ServerOptions{Inventory: inventory, Terminal: terminal, Log: log})
if err != nil { return fmt.Errorf("build portal server: %w", err) }
```

Set the rest config user agent to `agent-sandbox-portal/<git version>` without exposing credentials.

- [ ] **Step 4: Serve on the validated listener with graceful shutdown**

Call `net.Listen("tcp", opts.listenAddress)` only after validation. Configure `http.Server` with `ReadHeaderTimeout: 10*time.Second`, `IdleTimeout: 60*time.Second`, and no whole-response write timeout because WebSockets are long-lived. When the supplied context ends, call `Shutdown` with a 10-second child timeout. Treat `http.ErrServerClosed` as success and close active terminal contexts through the root context.

`main` creates a SIGINT/SIGTERM context, configures controller-runtime zap logging, calls `run`, logs one contextual error, and exits nonzero. Log the effective context, namespace scope, and local URL after the listener succeeds; do not log kubeconfig paths or REST config fields.

- [ ] **Step 5: Add the Make target and command-local README**

Update Makefile as follows:

```make
.PHONY: build
build: build-controller build-sandbox-router build-sandboxd build-portal

.PHONY: build-portal
build-portal:
	go build -ldflags "$(LD_FLAGS)" -o bin/agent-sandbox-portal ./cmd/agent-sandbox-portal
```

Document:

```text
make build-portal
./bin/agent-sandbox-portal
./bin/agent-sandbox-portal --namespace team-a
./bin/agent-sandbox-portal --all-namespaces
./bin/agent-sandbox-portal --router-url https://router.example --router-path-prefix /sandboxes
```

Explain that the URL is local, kubeconfig current context and namespace are used by default, `--kubeconfig` and `--context` are read-only overrides, and all-namespaces requires explicit RBAC. State the Kubernetes permissions precisely: `get,list` on `sandboxes.agents.x-k8s.io`; optional `get,list` on `sandboxclaims.extensions.agents.x-k8s.io`; `get` on backing Pods; and `create` on `pods/exec`. Explain that xterm assets are embedded and the shell is fixed to `/bin/sh`.

- [ ] **Step 6: Run command and build verification**

Run: `go test ./cmd/agent-sandbox-portal -count=1`

Expected: PASS.

Run: `make build-portal`

Expected: PASS and `bin/agent-sandbox-portal` exists.

Run: `./bin/agent-sandbox-portal --version`

Expected: exit 0 and output beginning `agent-sandbox-portal, version`.

- [ ] **Step 7: Commit binary integration and docs**

```bash
git add cmd/agent-sandbox-portal/main.go cmd/agent-sandbox-portal/main_test.go cmd/agent-sandbox-portal/README.md Makefile
git commit -m "feat: ship the local Sandbox portal binary"
```

---

### Task 9: Final Verification and Browser Smoke Test

**Files:**
- Modify only files required to fix failures introduced by Tasks 1-8.

**Interfaces:**
- Consumes: the complete portal binary and repository verification targets.
- Produces: reproducible verification evidence and a clean feature branch.

- [ ] **Step 1: Format and inspect the exact change set**

Run: `gofmt -w cmd/agent-sandbox-portal/*.go internal/portal/*.go`

Run: `git diff --check`

Expected: both exit 0.

Run: `git status --short`

Expected: only intentional portal, Makefile, documentation, and vendored asset changes are present; no binary, kubeconfig, npm cache, tarball, `node_modules`, `.venv`, `site/public`, or `site/resources` path is present.

- [ ] **Step 2: Run focused race, build, and lint checks**

Run: `go test -race ./internal/portal ./cmd/agent-sandbox-portal -count=1`

Expected: PASS with no race report.

Run: `make build`

Expected: PASS for manager, sandbox-router, sandboxd, and agent-sandbox-portal.

Run: `make lint-go`

Expected: PASS.

- [ ] **Step 3: Run repository-wide unit verification and compare with baseline**

Run: `make test-unit`

Expected on the current macOS baseline: all portal tests and all previously passing suites pass; the only permitted failure is `packages/sandboxd/pkg/pathutil TestSanitizePathAbsolutePathConfined`, matching the recorded pre-change failure. Restore any npm-generated `clients/typescript/agentic-sandbox-client/package-lock.json` platform metadata drift before continuing.

- [ ] **Step 4: Smoke-test the browser and live terminal when cluster prerequisites exist**

Run the binary against a kubeconfig containing at least one Ready Sandbox:

```bash
./bin/agent-sandbox-portal --kubeconfig bin/KUBECONFIG --namespace default
```

Verify in the browser that inventory refresh, filters, assignment labels, images, runtime class, countdown, copy actions, router links, first-container terminal, container reconnect, resize, and close cancellation behave as documented. In a second shell, verify the same credentials can perform the required reads and `pods/exec`; if no suitable cluster exists, record that the live smoke test was not run and retain the passing fake-client/WebSocket evidence.

- [ ] **Step 5: Commit only if verification required code corrections**

```bash
git add cmd/agent-sandbox-portal internal/portal Makefile
git commit -m "fix: harden Sandbox portal verification paths"
```

Skip this commit when Step 1 shows no corrections after the Task 8 commit.

- [ ] **Step 6: Review the branch and publish it**

Run: `git log --oneline --decorate origin/main..HEAD`

Run: `git diff --stat origin/main...HEAD`

Run: `git status --short --branch`

Expected: focused commits, the intended portal-only diff, and a clean worktree.

Run: `git push fork feat/sandbox-portal`

Expected: the fork branch advances successfully; opening a pull request remains a separate user-authorized handoff.
