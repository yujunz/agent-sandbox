# Sandbox Portal Design

**Status:** Approved
**Date:** 2026-09-08

## Summary

Add `agent-sandbox-portal`, a workstation-local Go binary that presents a thin browser UI over the Kubernetes API. The portal lists Sandboxes visible to the operator's kubeconfig and shows readiness, assigned user and agent, container images, runtime class, expiration, connection targets, and an embedded terminal.

The process binds only to loopback. It uses the operator's existing Kubernetes credentials for reads and `pods/exec`, so Kubernetes RBAC remains the authorization boundary. The design does not change the Sandbox APIs, controllers, or sandbox-router.

## Goals

- Give an operator a compact inventory of active and retained Sandboxes.
- Show readiness and its reason without requiring `kubectl` JSONPath queries.
- Show the configured user and agent identity from configurable labels.
- Show container images, runtime class, lifecycle expiration, and remaining time.
- Surface copyable Pod IP and Service FQDN values.
- Generate clickable sandbox-router links for declared ports when a router base URL is configured.
- Open an interactive `/bin/sh` terminal in a Ready Sandbox with one click.
- Preserve the operator's kubeconfig context and Kubernetes RBAC decisions.
- Work offline after the binary is built.

## Non-goals

- Creating, editing, suspending, resuming, or deleting Sandboxes.
- Replacing the Kubernetes API, sandbox-router, or the language SDKs.
- Providing a multi-user hosted dashboard, authentication proxy, or Ingress.
- Adding API fields for ownership or connection metadata.
- Supporting arbitrary Pod exec, arbitrary commands, file transfer, logs, metrics dashboards, warm-pool management, or template editing.
- Shipping an in-cluster Deployment, ServiceAccount, Role, or Service.

## User flow

1. The operator selects a kubeconfig context as they would for `kubectl`.
2. They run `agent-sandbox-portal` and open its loopback URL.
3. The portal lists Sandboxes in the context namespace by default. An explicit flag enables all namespaces.
4. The operator filters or scans the table, copies connection targets, or opens a declared router port.
5. Selecting **Terminal** opens an embedded terminal for the Sandbox's first container. If the Pod has multiple containers, the terminal drawer lets the operator reconnect to another container.
6. Closing the drawer or browser cancels the corresponding Kubernetes exec stream.

## Architecture

### Command

`cmd/agent-sandbox-portal` owns process concerns:

- command-line flag parsing and validation;
- kubeconfig loading and context/namespace selection;
- construction of typed Sandbox, SandboxClaim, and core Kubernetes clients;
- loopback HTTP listener setup;
- structured logging, signal handling, and graceful shutdown;
- version output through the existing `internal/version` package.

The binary is added to `make build` through a dedicated `build-portal` target.

### Portal package

`internal/portal` contains the application behavior:

- `inventory.go` lists resources and converts them into stable UI records;
- `server.go` serves health, inventory, terminal, and embedded asset endpoints;
- `terminal.go` validates terminal targets and bridges WebSocket traffic to Kubernetes remote exec;
- focused `_test.go` files cover each behavior;
- `web/` contains the embedded HTML, CSS, JavaScript, and vendored terminal assets.

The package accepts client and executor interfaces so unit tests can use the repository's generated fake clients without a cluster.

### Browser application

The browser application is framework-free HTML, CSS, and JavaScript embedded with `go:embed`. A pinned xterm.js distribution and its fit addon are vendored with their upstream license files. Vendoring is justified by the need for correct terminal emulation while keeping the operator tool offline-capable and avoiding a Node build or CDN dependency.

## Configuration

The initial command surface is:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--listen-address` | `127.0.0.1:8080` | Portal HTTP listener. Validation accepts loopback hosts only. |
| `--kubeconfig` | standard client-go loading rules | Select a kubeconfig file without changing it. |
| `--context` | kubeconfig current context | Override the selected context. |
| `--namespace` | selected context namespace, then `default` | Restrict inventory and terminal access to one namespace. |
| `--all-namespaces` | `false` | Explicitly request cluster-wide inventory and terminal selection. Mutually exclusive with an explicitly supplied `--namespace`. |
| `--user-label` | `sandbox.users.io/user` | Label key used for the assigned user. |
| `--agent-label` | `sandbox.users.io/agent` | Label key used for the assigned agent. |
| `--router-url` | empty | Optional external sandbox-router base URL used to generate port links. |
| `--router-path-prefix` | `/sandboxes` | Router path-routing prefix appended to `--router-url`. |
| `--version` | `false` | Print build version and exit. |

`--listen-address` rejects unspecified, non-loopback, and Unix-socket targets. `localhost`, IPv4 loopback addresses, and IPv6 loopback are accepted. `--router-url`, when set, must be an absolute `http` or `https` URL without credentials, query parameters, or fragments.

## Inventory model

`GET /api/v1/sandboxes` lists Sandbox resources in the configured namespace scope. It also lists SandboxClaims to enrich claim-owned Sandboxes with claim lifecycle data.

The response has a generation timestamp, the effective namespace scope, optional warnings, and records sorted by namespace and name. Each record contains:

- namespace, Sandbox name, and optional owning claim name;
- creation timestamp and desired operating mode;
- Ready status, reason, message, and transition time;
- assigned user and agent;
- containers with name, image, and declared TCP ports;
- runtime class;
- absolute expiration and remaining lifecycle basis;
- Pod IPs and Service FQDN;
- generated connection links;
- whether terminal access is currently eligible.

### Readiness

Readiness comes from the Sandbox `Ready` condition. A missing condition is represented as `Unknown`; the portal does not infer readiness from Pod IP presence. The condition reason and message remain available in the UI for pending, failed, suspended, and expired states.

### User and agent assignment

The configured label keys are resolved in this order:

1. `Sandbox.spec.podTemplate.metadata.labels`, which includes metadata propagated from a SandboxClaim;
2. `Sandbox.metadata.labels` as a fallback.

Missing values are represented as unassigned. The portal does not mutate labels or interpret arbitrary annotations as identity.

### Images and runtime class

The record includes every regular container from `Sandbox.spec.podTemplate.spec.containers`. The first container is the primary display value; additional containers remain visible without discarding their names or images. The runtime class comes directly from `Sandbox.spec.podTemplate.spec.runtimeClassName`; an unset value is displayed as the cluster default.

### Expiration and TTL

For a claim-owned Sandbox, the claim lifecycle is authoritative because `SandboxClaim.spec.lifecycle.shutdownTime` is deliberately not propagated to the Sandbox. For a direct Sandbox, `Sandbox.spec.shutdownTime` is used.

The API returns absolute timestamps rather than a server-formatted countdown. The browser computes the live remaining duration from its current time and the response generation time, preventing an API request every second.

When a claim has `ttlSecondsAfterFinished`, the record includes that retention value. If its mirrored `Finished=True` condition exists, the portal also computes the retention deadline from the condition's `lastTransitionTime`. Before completion, the UI displays the policy as “after finish” rather than inventing an absolute deadline.

Claims are matched to Sandboxes by a controller owner reference to `SandboxClaim`. The claim status Sandbox name is a secondary lookup for compatibility with objects whose ownership metadata is incomplete; it is used only when exactly one claim identifies that Sandbox. Owner-reference matches take precedence, and ambiguous status-only matches produce a warning instead of guessing. A direct Sandbox remains usable if no claim matches.

### Connections

Every record exposes Pod IPs and Service FQDN as copyable values. When `--router-url` is configured, each declared container port becomes a browser link with this shape:

```text
<router-url><router-path-prefix>/<namespace>/<sandbox-name>/<port>/
```

Only declared ports produce links. Port names are labels; TCP is assumed when protocol is omitted, and non-TCP ports are not linked. URL construction escapes path segments and preserves any base path already present in `--router-url`.

## HTTP surface

The server exposes only:

- `GET /` and embedded static assets;
- `GET /healthz` for local process health;
- `GET /api/v1/sandboxes` for inventory;
- WebSocket upgrade at `GET /api/v1/namespaces/{namespace}/sandboxes/{name}/terminal?container={container}`.

Unknown paths and unsupported methods return JSON errors for API routes and ordinary `404` responses elsewhere. Inventory responses use `Cache-Control: no-store`.

All responses set a restrictive Content Security Policy, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and `X-Frame-Options: DENY`. The policy permits WebSockets only back to the portal's own origin and scripts/styles only from embedded assets.

## Terminal design

### Target validation

Terminal validation happens before the WebSocket upgrade:

1. Verify the request Origin matches the portal's own loopback origin.
2. Verify the namespace is inside the configured scope.
3. Re-read the named Sandbox from the Kubernetes API.
4. Require no deletion timestamp, no elapsed shutdown time, and `Ready=True`.
5. Resolve the backing Pod from the legacy non-empty pod-name annotation, falling back to the Sandbox name.
6. Read the Pod and require that it is controlled by the Sandbox UID, Running, and Ready.
7. Resolve the requested container against the live Pod; default to its first regular container.

The endpoint never accepts a Pod name or arbitrary command from the browser. It always executes the fixed command `/bin/sh` with `stdin`, `stdout`, and TTY enabled. Images without `/bin/sh` receive a clear terminal error rather than an automatic command substitution.

### Streaming protocol

The server uses `remotecommand.NewSPDYExecutor` and `StreamWithContext` to connect to the Kubernetes `pods/exec` subresource. An internal executor interface keeps the HTTP handler testable.

Browser-to-server WebSocket text messages are small JSON control frames:

- `{"type":"input","data":"..."}` writes terminal input;
- `{"type":"resize","cols":120,"rows":40}` updates the remote TTY size.

Server-to-browser binary messages carry raw terminal output bytes for xterm.js. Server-to-browser JSON control frames report session errors and clean exit. The implementation limits control-message size, serializes WebSocket writes, sends ping frames, and closes the exec context promptly on browser disconnect or process shutdown.

TTY mode intentionally merges stderr into the terminal output stream, matching an interactive shell. Error responses sent before upgrade contain no kubeconfig data, bearer tokens, or raw API response bodies.

## UI design

The page uses a single compact dashboard view:

- header with effective context/namespace, last refresh time, search, readiness filter, and manual refresh;
- summary counts for total, Ready, pending/unready, and expiring soon;
- responsive Sandbox table with columns for identity, readiness, assignment, image/runtime, expiry, connections, and actions;
- accessible badges and text labels that do not rely on color alone;
- empty, loading, partial-data, and API-error states;
- terminal drawer with Sandbox/container identity, reconnect control, and close action.

The UI refreshes inventory every five seconds while visible and pauses polling when the document is hidden. An active terminal remains connected across inventory refreshes. The **Terminal** action immediately uses the first container; the drawer's container selector allows an explicit reconnect to another container.

## Failure behavior

- Invalid flags or an unusable kubeconfig fail startup with contextual errors.
- Kubernetes inventory failures return `503` and remain retryable from the UI.
- If the SandboxClaim CRD is absent, core Sandbox inventory remains available without claim enrichment.
- If claim listing is forbidden or temporarily fails, Sandbox inventory is returned with a warning; expiry falls back to Sandbox lifecycle data when present.
- A resource that changes between inventory and terminal selection is rejected by the terminal endpoint after its fresh reads.
- Kubernetes `Forbidden`, `NotFound`, and readiness failures are mapped to concise operator-facing messages and appropriate HTTP status codes.
- Router link configuration errors fail startup; individual malformed port declarations are omitted rather than producing unsafe links.
- Terminal stream failures close only that terminal session and do not affect inventory polling or other sessions.

## Security model

The portal intentionally inherits the operator's Kubernetes authority. If their credentials cannot list a resource or create `pods/exec`, the portal cannot bypass that decision.

Loopback binding prevents network peers from reaching the HTTP server, and same-origin validation prevents a normal remote web page from opening a credentialed terminal WebSocket through the operator's browser. The portal does not offer an opt-out or a public listen mode. Local processes running as the same workstation user are outside this boundary and generally already have access to that user's kubeconfig.

The terminal endpoint narrows an operator's general Kubernetes exec capability to the Sandbox selected in the UI. Fresh resource reads, namespace enforcement, Sandbox-to-Pod ownership checks, and a fixed shell command prevent the endpoint from becoming a generic Pod exec proxy.

No Kubernetes credentials, Secret values, environment variables, terminal input, or terminal output are logged. Logs contain only resource coordinates, lifecycle events, and contextual errors.

## Testing strategy

Implementation follows test-driven development. Each behavior starts with a focused failing test.

### Inventory tests

- Ready, unready, missing-condition, suspended, expired, and deleting records.
- Direct Sandbox and claim-owned expiration precedence.
- Finished TTL deadline and “after finish” policy behavior.
- User/agent label precedence and missing values.
- Multiple images, runtime class, TCP port links, and URL escaping.
- Missing SandboxClaim CRD and partial claim-list authorization failures.
- Namespace and all-namespace list options.

### HTTP tests

- Static asset delivery, content types, security headers, and no-store API responses.
- Method and route rejection.
- Kubernetes error-to-status mapping without credential leakage.
- Stable JSON schema and deterministic record ordering.

### Terminal tests

- Origin and namespace rejection.
- Missing, deleting, expired, or non-Ready Sandbox rejection.
- Pod ownership, phase, readiness, and container validation.
- Default-container selection and fixed `/bin/sh` execution request.
- Input, output, resize, clean exit, failure, disconnect, and cancellation behavior through a fake executor.
- Concurrent sessions and serialized WebSocket writes under the race detector.

### Configuration and command tests

- Kubeconfig context and namespace resolution.
- `--namespace` and `--all-namespaces` exclusivity.
- Loopback-only listener validation, including IPv4 and IPv6.
- Router URL and label-key validation.

### Verification

- `go test -race ./internal/portal ./cmd/agent-sandbox-portal`
- `make build`
- `make lint-go`
- `make test-unit`
- manual browser smoke test against fake records, followed by a live-cluster smoke test when a suitable kubeconfig and Ready Sandbox are available

## Documentation

Add a command-local README under `cmd/agent-sandbox-portal` covering build, flags, kubeconfig behavior, router links, terminal RBAC, and example invocation. This keeps the first implementation scoped and avoids changing the mounted public documentation until maintainers decide where the planned dashboard belongs in the site navigation.

## Compatibility

The feature is additive. It introduces a new binary and internal package without changing existing APIs, controller behavior, labels, annotations, generated clients, manifests, or images. Existing Sandbox and SandboxClaim objects require no migration.

## Acceptance criteria

- The binary refuses non-loopback listeners and loads the selected kubeconfig context.
- The default view is restricted to the context namespace; all namespaces require an explicit flag.
- The inventory shows every requested field with correct direct/claim lifecycle semantics.
- Router links are generated only when configured and only for declared TCP ports.
- A Ready Sandbox opens an interactive `/bin/sh` terminal in one click.
- Invalid or stale terminal targets cannot reach arbitrary Pods or containers.
- The application works without a CDN or separate frontend build.
- Focused tests pass under the race detector, and repository-wide verification introduces no new failures beyond an explicitly documented baseline failure.
