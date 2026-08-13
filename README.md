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
  not mounted directly; an OMP credential database requires an explicit private
  snapshot grant;
- every host-side Cohotfs file and socket lives below a user-owned `~/.cohotfs`;
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

1. resolves a compatible Cohotfs base image by immutable digest;
2. creates a non-privileged container with the requested source, limits, and
   explicitly granted integrations;
3. starts `cohotfs-agent` as PID 1 and container root;
4. creates an `agent` account whose numeric UID/GID match the invoking host user;
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
| Images | Pull/resolve `image.ref` by digest, explicit BuildKit-backed `cohotfs image build`, local-only `pullPolicy: never`, Cohotfs base compatibility probe |
| Lifecycle | Create, start, stop, restart, inspect, list, remove, recover, and rotate SSH host keys |
| Interactive access | Host OpenSSH shell, command execution, and loopback-only local port forwarding through an identity-checked Unix socket |
| Setup | `once`, `always`, and `manual` repository setup; mapped host UID/GID; explicit retry/force; bounded output and timeout handling |
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
  profile, or directly shared agent credential database mounted into a workspace.
- No arbitrary prebuilt image. The final image must retain the matching Cohotfs
  bootstrap contract and its OpenSSH runtime dependencies.
- Workspace creation does not build `spec.image.build` inline yet. Run
  `cohotfs image build`, then use its local tag as `image.ref` with
  `pullPolicy: never`.
- No generic runtime annotation/option escape hatch.

Docker socket access is itself highly privileged—root-equivalent for a typical
rootful daemon. Cohotfs uses the invoking user's existing access and never changes
socket ownership or group membership. Prefer rootless Docker where it fits.

## Getting started

### Requirements

- Linux or WSL2 on an `amd64` host
- a local Docker Engine reachable by the current user through a Unix socket
- the Docker CLI for source image builds
- the host OpenSSH client and `ssh-keygen`
- Go 1.26.5 when building from source

From the Cohotfs repository root, build the host binary and the matching local
workspace image:

```console
install -d "$HOME/.local/bin"
go build -o "$HOME/.local/bin/cohotfs" ./cmd/cohotfs
export PATH="$HOME/.local/bin:$PATH"
./images/workspace-base/build-local-image.sh
```

The helper builds the static in-container agent, creates a temporary Docker build
context, and tags the result as
`ghcr.io/gosuda/cohotfs/workspace-base:dev`. A `dev` host binary writes that
exact reference with `pullPolicy: never`, so workspace creation resolves the
already-loaded Docker image without contacting GHCR. Pass one optional image tag
to the helper only when you set the same `spec.image.ref` and retain
`pullPolicy: never`. Release manifests default to `pullPolicy: always`.

Prepare the per-user state and inspect the machine:

```console
cohotfs onboard
cohotfs doctor
```

`onboard` records each unambiguous native toolchain's canonical root in the host
configuration and grants that exact path through `permittedRoots`. Rerun it after
moving or replacing a toolchain. An enabled host toolchain whose root is not
granted fails before container creation instead of leaving its command unavailable.

Initialize trusted policy for the current project:

```console
cd ~/src/my-project
cohotfs init
cohotfs config project show
cohotfs config project edit
```

`init` writes the complete project policy only to
`~/.cohotfs/projects/<source-hash>/workspace.yaml`; it does not create or modify
repository files. The terminal editor handles integration toggles. Use
`cohotfs config project edit --edit` with `$VISUAL` or `$EDITOR` to edit the full
strict YAML document, including setup argv and resource limits. The generated
policy uses manual no-op setup. If setup should run repository code, put it at a
path such as `scripts/cohotfs-setup.sh` and explicitly set
`spec.setup.command`; repository `.cohotfs` and `.omp` paths are masked inside
the workspace and cannot host setup scripts.

Bootstrap PID 1 remains container root, but setup commands, SSH shells, and
`cohotfs exec` run with the host-mapped `agent` UID/GID. Setup is still trusted
repository code: it can modify the writable project mount by design.
When host toolchains are enabled, setup inherits their validated managed paths
and caches; unrelated container or agent-secret variables remain excluded.

Then enter:

```console
cohotfs
```

The bare command fixes the current directory at `/workspace`, creates or reuses
its workspace, starts it when needed, and opens an interactive OpenSSH session.
It refuses to mount your home directory by default. The exceptional
`cohotfs --allow-home` path still masks `~/.cohotfs` inside the workspace.

Useful current-directory commands:

```console
cohotfs workspace create
cohotfs workspace list
cohotfs workspace status
cohotfs shell
cohotfs exec -- go test ./...
cohotfs port-forward 3000
cohotfs setup run
cohotfs workspace stop
cohotfs workspace recover        # preview identity-matched cleanup
cohotfs workspace recover --yes  # apply it
cohotfs workspace remove --yes
```

Workspace-targeting commands select the workspace whose canonical source is the
current directory. Use `--workspace <name-or-id>` to target another workspace,
for example `cohotfs shell --workspace api` or
`cohotfs workspace stop --workspace api`.

`port-forward` listens on host `127.0.0.1` and forwards to the same
container-loopback port through the authenticated workspace SSH transport. Keep
the command running while using `http://127.0.0.1:3000`; stop it with `Ctrl-C`.
Use `--local-port 8080` to map host port 8080 to container port 3000.
`--host 0.0.0.0` explicitly exposes the host port on every IPv4 interface; use
it only when other machines must connect. The container application may stay
bound to `127.0.0.1`; no Docker port is published. Workspaces created with an
older bootstrap API are rejected with a remove/recreate error instead of
attempting an unsupported forward.

Inspect runtime negotiation without guessing:

```console
cohotfs runtime list --output json
cohotfs runtime capabilities --output json
cohotfs config show
cohotfs config project show
```

## Configuration

Global host capabilities and defaults live in `~/.cohotfs/config.yaml`. The
complete trusted policy for a project lives in
`~/.cohotfs/projects/<source-hash>/workspace.yaml`, bound to that project's
canonical source identity. Repository files never participate in policy
resolution, so a checkout cannot grant itself host integrations.

An initialized project's validated document is authoritative. In an
uninitialized directory, Cohotfs resolves host defaults over built-ins. Host
runtime endpoints, permitted external roots, browser executables, toolchain
sources, and credential environment mappings remain machine-local in
`~/.cohotfs/config.yaml`.

Both documents are strict YAML: unknown keys are errors. Run `cohotfs init` to
write the complete project schema, then use `cohotfs config project edit` to
enable only the integrations that project needs.

OMP is opt-in. When enabled, its selected binary, native modules, model catalog,
and non-secret configuration are cloned into workspace-owned writable snapshots;
container writes cannot modify the host copies. Set `requireCow: true` to require
reflink support instead of allowing a private copy-once fallback. Set
`agents.omp.import.oauthDB: true` only when the workspace may receive credentials.
Cohotfs directly byte-copies `agent.db` and any existing `-wal`, `-shm`, or
`-journal` sidecars into a private, writable snapshot; these files do not use
reflinks even when `requireCow: true`. Host and workspace credential updates do
not sync. Stop OMP or otherwise quiesce its database before workspace creation
when a point-in-time-consistent copy is required.

All host-side state stays under:

```text
~/.cohotfs/
├── config.yaml
├── projects/                 # source-bound trusted project policies
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
./images/workspace-base/build-local-image.sh
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
