# Sandbox portal

`agent-sandbox-portal` is a standalone, workstation-local process and browser
interface for Sandboxes visible through your Kubernetes credentials. It is not
deployed as a Kubernetes Pod or Service. It listens only on a loopback address
and prints the local URL when it starts.

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
context. Use `--namespace` to select one namespace. Cluster-wide inventory
requires the explicit `--all-namespaces` flag and the separate inventory RBAC
described below.

`--router-url` enables links for TCP ports declared by Sandbox containers.
`--router-path-prefix` configures the path between that base URL and the
namespace, Sandbox, and port segments.

## Kubernetes access

The portal uses the selected kubeconfig identity and Kubernetes RBAC remains
the authorization boundary.

For the default single-namespace mode or an explicit `--namespace`, bind a Role
in that namespace which grants precisely:

- `get,list` on `sandboxes.agents.x-k8s.io`;
- optionally, `get,list` on
  `sandboxclaims.extensions.agents.x-k8s.io` for claim lifecycle enrichment and
  claim-owned terminal validation;
- `get` on the backing Pods; and
- `create` on the `pods/exec` subresource.

For `--all-namespaces`, split inventory discovery from terminal authority:

- Use a ClusterRole and ClusterRoleBinding granting cluster-wide `list` on
  `sandboxes.agents.x-k8s.io` and, optionally, cluster-wide `list` on
  `sandboxclaims.extensions.agents.x-k8s.io`. These permissions populate the
  inventory only.
- In every namespace where terminal access is intended, use a Role and
  RoleBinding granting `get` on Sandboxes, optional `get` on SandboxClaims,
  `get` on Pods, and `create` on `pods/exec`.

An identity may therefore see a Sandbox in the cluster-wide inventory but be
unable to open its terminal. If any required namespace-scoped permission is
denied, the terminal request for that namespace is rejected. Without the
optional SandboxClaim permissions, direct Sandbox inventory remains available,
but claim enrichment is reported as unavailable and terminals for claim-owned
Sandboxes are rejected because their lifecycle cannot be verified.

The portal never reads Secrets and does not expose arbitrary Pod exec. Terminal
targets are revalidated against the selected Sandbox and backing Pod, and the
command is fixed to `/bin/sh`. Terminal emulation uses embedded xterm.js assets,
so the browser UI does not load assets from a CDN.

## Running on a remote host

The local URL is relative to the host running `agent-sandbox-portal`. To use a
browser on a different workstation, create a host-level tunnel to the remote
loopback listener, for example:

```shell
ssh -L 8080:127.0.0.1:8080 remote-host
```

Then open `http://127.0.0.1:8080` locally. `kubectl port-forward` does not apply:
the portal is a standalone host process, not an in-cluster Pod or Service.
