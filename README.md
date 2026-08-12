# Cohotfs

**Give your coding agent a room of its own—not the keys to the house.**

Cohotfs creates reproducible container workspaces for agentic AI. Your project is
mounted where the agent can work on it, while access to host toolchains, browsers,
credentials, and agent configuration is granted one integration at a time.

Think of it as a furnished sublet for code: the workspace gets a writable desk,
borrowed tools come with receipts, and the valuables stay at home.

> [!IMPORTANT]
> Cohotfs is currently Docker-first and Linux-first. The implemented runtime is a
> **local Docker Engine over a Unix socket**, the workspace platform is
> **linux/amd64**, and the host application runs on **Linux or WSL2**. Native
> containerd and CRI/CRI-O adapters are planned, not currently shipped.

## Why Cohotfs?

An isolated container is useful until an agent needs to compile with your local
toolchain, open Chrome, sign a Git operation, or reuse a small piece of its normal
configuration. The tempting fix is to mount `$HOME`, the Docker socket, browser
profiles, or credential files into the container. That works right up to the point
where it works much too well.

Cohotfs keeps the host as the control plane:

- host integration is off by default;
- the container never receives a Docker/runtime socket;
- host SSH private keys, browser profiles, and agent authentication databases are
  not mounted;
- every persistent Cohotfs artifact lives below a user-owned `~/.cohotfs`;
- requested capabilities are checked before resources are created;
- interrupted operations are journaled and recoverable; and
- `isolation: gvisor` fails closed when the configured runtime is unavailable. It
  never quietly becomes an ordinary container.

The project directory is intentionally writable. Cohotfs protects the host around
the workspace; it does not pretend that an agent cannot edit the source tree you
asked it to edit.

## How it works

For each operation, the host-side `cohotfs` process resolves strict configuration,
probes the selected runtime, and compiles a complete redacted workspace plan before
making changes. It then:

1. resolves or builds a compatible Cohotfs base image by immutable digest;
2. creates an unprivileged container with the requested source, limits, and
   explicitly granted integrations;
3. starts `cohotfs-agent` as PID 1 inside the container;
4. creates an `agent` account with the invoking host user's numeric UID/GID;
5. generates a per-workspace SSH host key and starts a restrictive `sshd` on
   container loopback;
6. pins that host key under a workspace-specific alias; and
7. enters the workspace using the host OpenSSH client.

SSH stays end-to-end between the host OpenSSH client and the workspace `sshd`.
Cohotfs is only the byte-clean `ProxyCommand` transport:

```text
host ssh
   │
   ▼
cohotfs ssh-proxy
   │  validates workspace state, runtime nonce, socket owner/type/mode/inode
   ▼
~/.cohotfs/run/workspaces/<id>/ssh/ssh.sock
   │
   ▼
cohotfs-agent relay ──► sshd on 127.0.0.1:2222
```

Optional integrations use short-lived, identity-checked leases from the per-user
`cohotfs host serve` process. Secrets move only when an authorized operation asks
for them; they are not copied into images or durable workspace state.

## Supported today

| Area | Support |
| --- | --- |
| Host | Linux and WSL2 |
| Runtime | Local Docker Engine through a pathname Unix socket |
| Workspace platform | `linux/amd64` |
| Isolation | Standard OCI isolation; optional fail-closed Docker gVisor runtime |
| Images | Pull by reference, resolve by digest, BuildKit-backed Docker builds, Cohotfs base compatibility probe |
| Lifecycle | Create, start, stop, restart, inspect, list, remove, recover, and rotate SSH host keys |
| Interactive access | Host OpenSSH shell and command execution through an identity-checked Unix socket |
| Setup | `once`, `always`, and `manual` repository setup; explicit retry/force; bounded output and timeout handling |
| Resources | Optional CPU, memory, total memory+swap, PID, and `nofile` limits |
| Host toolchains | Native Linux Go/Rust discovery; read-only toolchain roots; COW or isolated managed caches |
| Browser | Fresh-profile Linux Chrome CDP; native Windows Chrome from WSL through the companion bridge |
| Credentials | Explicit SSH-agent forwarding and allowlisted, read-only Git HTTPS credential brokering |
| AI agents | Allowlisted non-secret settings for OMP, Codex, and Claude; one-use environment-secret leases |
| Operations | Durable state, idempotent mutation journals, identity-checked cleanup, quarantine, and recovery |
| Machine output | Stable JSON output for discovery, status, and list commands |

### Deliberate non-features

- No remote Docker endpoint over TCP, TLS, or SSH.
- No native containerd or CRI/CRI-O backend yet.
- No silent runtime-exec or routable TCP fallback for workspace SSH.
- No runtime socket, host home, `.ssh` directory, private key, normal browser
  profile, or agent credential database mounted into a workspace.
- No arbitrary prebuilt image. The final image must retain the matching Cohotfs
  bootstrap contract and its OpenSSH runtime dependencies.
- No generic runtime annotation/option escape hatch.

Docker socket access is itself highly privileged—root-equivalent for a typical
rootful daemon. Cohotfs uses the invoking user's existing access and never changes
socket ownership or group membership. Prefer rootless Docker where it fits.

## Getting started

### Requirements

- Linux or WSL2 on an `amd64` host
- a local Docker Engine reachable by the current user through a Unix socket
- the host OpenSSH client and `ssh-keygen`
- a compatible Cohotfs workspace-base image

Build the host binary from source:

```console
go build -o "$HOME/.local/bin/cohotfs" ./cmd/cohotfs
```

A source build identifies itself as `dev`; use a matching locally built
workspace-base image or set `spec.image.ref` to a published version-compatible
image. Release binaries resolve their matching image tag automatically.

Prepare the per-user state and inspect the machine:

```console
cohotfs onboard
cohotfs doctor
```

Initialize a repository:

```console
cd ~/src/my-project
cohotfs init
```

This creates `.cohotfs/workspace.yaml` and a machine-local override below
`~/.cohotfs/projects/`. Review the generated image, setup, resource, and
integration policy. The default setup command points to
`.cohotfs/setup.sh`; add that repository-owned script before the first create, or
change the command to another real script in the repository.

Then enter:

```console
cohotfs
```

The bare command fixes the current directory at `/workspace`, creates or reuses
its workspace, starts it when needed, and opens an interactive OpenSSH session.
It refuses to mount your home directory by default. The exceptional
`cohotfs --allow-home` path still masks `~/.cohotfs` inside the workspace.

Useful explicit commands:

```console
cohotfs workspace create
cohotfs workspace list
cohotfs workspace status <workspace>
cohotfs shell <workspace>
cohotfs exec <workspace> -- go test ./...
cohotfs setup run <workspace>
cohotfs workspace stop <workspace>
cohotfs workspace recover <workspace>        # preview identity-matched cleanup
cohotfs workspace recover <workspace> --yes  # apply it
cohotfs workspace remove <workspace> --yes
```

Inspect runtime negotiation without guessing:

```console
cohotfs runtime list --output json
cohotfs runtime capabilities --output json
cohotfs config show
```

## Configuration

Project policy lives in `.cohotfs/workspace.yaml`. Host-local grants—runtime
endpoints, permitted external roots, browser executables, selected toolchains, and
credential environment mappings—live in `~/.cohotfs/config.yaml` and cannot be
expanded by a repository.

Precedence is:

```text
command flags
  > ~/.cohotfs/projects/<source-hash>/override.yaml
  > .cohotfs/workspace.yaml
  > ~/.cohotfs/config.yaml defaults
  > built-ins
```

Both documents are strict YAML: unknown keys are errors, not creative suggestions.
Run `cohotfs init` to generate the complete workspace schema, then enable only the
integrations the project needs.

All host-side state stays under:

```text
~/.cohotfs/
├── config.yaml
├── projects/                 # machine-local project overrides
├── state/                    # workspaces, plans, and operation journals
├── workspaces/               # private homes, system state, and toolchain views
├── ssh/                      # Cohotfs client key and pinned workspace host keys
├── browser/                  # isolated Chrome profiles
├── run/                      # identity-checked sockets, leases, and locks
├── logs/                     # redacted audit/setup logs
└── tmp/
```

## Development

```console
make build
make test
make vet
make windows-bridge
```

The live Docker suite creates real containers and images, exercises SSH/SCP/SFTP,
setup modes, resource policies, host-key rejection, concurrent mutation, and
cleanup:

```console
go test -tags=integration ./integration/docker \
  -run TestWorkspaceEndToEnd -v -count=1 -timeout=20m
```

## License

MIT. See [LICENSE](LICENSE).
