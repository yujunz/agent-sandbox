# Sandbox portal

`agent-sandbox-portal` is a workstation-local browser interface for Sandboxes
visible through your Kubernetes credentials. It listens only on a loopback
address and prints the local URL when it starts.

## Build and run

```shell
make build-portal
./bin/agent-sandbox-portal
./bin/agent-sandbox-portal --namespace team-a
./bin/agent-sandbox-portal --all-namespaces
./bin/agent-sandbox-portal --router-url https://router.example --router-path-prefix /sandboxes
```

## Configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `--listen-address` | `127.0.0.1:8080` | Local HTTP listener; only loopback addresses are accepted. |
| `--kubeconfig` | Standard loading rules | Read a specific kubeconfig file. |
| `--context` | Current context | Select a kubeconfig context. |
| `--namespace` | Context namespace or `default` | Restrict access to one namespace. |
| `--all-namespaces` | `false` | Explicitly request access across namespaces. |
| `--user-label` | `sandbox.users.io/user` | Select the label used for assigned users. |
| `--agent-label` | `sandbox.users.io/agent` | Select the label used for assigned agents. |
| `--router-url` | Empty | Set the sandbox-router base URL and enable router links. |
| `--router-path-prefix` | `/sandboxes` | Set the sandbox-router path prefix. |
| `--version` | `false` | Print build version information and exit. |

By default, the portal uses the current context and that context's namespace
from the standard kubeconfig loading rules. If the context has no namespace,
the portal uses `default`. The `--kubeconfig` and `--context` flags are read-only
overrides; the portal does not modify kubeconfig files or change the current
context. Use `--namespace` to select one namespace. Access to all namespaces
requires the explicit `--all-namespaces` flag and cluster-wide RBAC for the
permissions below.

`--router-url` enables links for TCP ports declared by Sandbox containers.
`--router-path-prefix` configures the path between that base URL and the
namespace, Sandbox, and port segments.

## Kubernetes access

The portal uses the selected kubeconfig identity and Kubernetes RBAC remains
the authorization boundary. That identity needs precisely:

- `get,list` on `sandboxes.agents.x-k8s.io`;
- optionally, `get,list` on
  `sandboxclaims.extensions.agents.x-k8s.io` for claim lifecycle enrichment and
  claim-owned terminal validation;
- `get` on the backing Pods; and
- `create` on the `pods/exec` subresource.

Grant these permissions with namespace-scoped Roles and RoleBindings for the
default or `--namespace` mode. The `--all-namespaces` mode requires equivalent
cluster-wide access, such as a ClusterRole and ClusterRoleBinding. Without the
optional SandboxClaim permissions, direct Sandbox inventory remains available,
but claim enrichment is reported as unavailable and terminals for claim-owned
Sandboxes are rejected because their lifecycle cannot be verified.

The portal never reads Secrets and does not expose arbitrary Pod exec. Terminal
targets are revalidated against the selected Sandbox and backing Pod, and the
command is fixed to `/bin/sh`. Terminal emulation uses embedded xterm.js assets,
so the browser UI does not load assets from a CDN.
