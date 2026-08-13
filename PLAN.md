# Cohotfs Agentic Workspaces Implementation Plan

## Context

Cohotfs is a greenfield, MIT-licensed Go project; the repository currently contains only `.gitignore`, `LICENSE`, and a two-line `README.md`. Build it as **“Cohotfs — isolated, host-integrated workspaces for agentic AI.”** A Linux or WSL2 user will create reproducible container workspaces, enter them through the host OpenSSH client, and explicitly grant narrowly scoped access to host toolchains, Chrome CDP, Git/SSH credentials, and selected AI-agent configuration.

The intended end state covers Docker Engine first, followed by native containerd and CRI/CRI-O through capability-oriented adapters. gVisor is optional defense in depth and must never silently fall back to the default OCI runtime. Host integration is off by default, and no runtime socket, home directory, private key, browser profile, or agent credential database is exposed to a workspace.

Before adding source, copy this approved execution spec verbatim to repository-root `PLAN.md`; use its ordered steps as the implementation roadmap and mark a step complete only after its verification passes.

## Settled architecture and contracts

### Processes and trust boundaries

- `cohotfs` is the per-user host application and default control plane. It runs as the invoking user, parses commands, reads configuration, owns workspace lifecycle and host integrations, invokes host `ssh`, and connects directly to the selected runtime endpoint using that user’s existing socket/TLS permissions. Docker v0.1 requires the user already be authorized for the Docker socket; Cohotfs never changes its ownership or group membership. `onboard` must warn that access to a rootful Docker socket is effectively root-equivalent even though Cohotfs itself remains a user process, and recommend Docker rootless mode where compatible.
- Every Cohotfs-created host artifact is isolated below one canonical local root: `~/.cohotfs`, resolved from the current UID’s home directory rather than a project or current working directory. The CLI rejects a symlinked root and creates it mode `0700`; production has no flag/environment override that relocates it. Configuration, durable state, caches, logs, SSH keys/known-hosts, browser profiles, build staging, toolchain upper/work/merged directories, broker endpoints, locks, and Unix sockets all stay below this root. Container/runtime objects and native Windows Chrome processes are unavoidable external resources represented by records under the root; Cohotfs creates no `/etc/cohotfs`, `/var/lib/cohotfs`, `/run/cohotfs`, or project-local execution state.
- `cohotfs host serve` is an optional per-user background process, started on demand for persistent Chrome/Git/CDP leases and crash reconciliation. It uses the same binary, UID, config, and local root; its control socket is `~/.cohotfs/run/host.sock` mode `0600` and requests are authorized with `SO_PEERCRED` requiring exactly the root owner UID. Ordinary create/start/stop/status and SSH work without it unless an enabled integration needs a persistent lease.
- Privilege is capability-specific, never an always-root control plane. Docker operates through the user-authorized endpoint. Native containerd/CRI-O require their sockets to be explicitly accessible to the user or report unavailable. Host cache COW first attempts unprivileged OverlayFS, then `fuse-overlayfs`; the resulting mount must be in the host app’s mount namespace so the local Docker daemon can resolve the merged bind source. If namespace propagation/daemon locality prevents that, use isolated caches or fail when `requireCow: true`. Cohotfs installs no setuid binary, root daemon, polkit rule, sudo wrapper, or system service in v0.1.
- The host app resolves external source/toolchain/browser/build paths with file-descriptor-relative checks (`openat2` `RESOLVE_BENEATH|RESOLVE_NO_MAGICLINKS|RESOLVE_NO_SYMLINKS`). Kernels/filesystems lacking enforceable beneath semantics fail closed for the affected host-path capability instead of using racy string canonicalization. It never executes shell strings.
- `cohotfs-agent` is a static, multi-call in-container binary. Its `serve` mode is PID 1, reaps children, starts restrictive `sshd`, publishes readiness, and provides only SSH/CDP/credential transport endpoints. It is not a general command-execution RPC and never receives a container-runtime socket.
- A future/native-Windows control plane is not part of this plan. WSL2 is controlled from the Linux per-user app; only `cohotfs-windows-bridge.exe`, launched through WSL interop, crosses into Windows to launch and tunnel to native Windows Chrome.

### Configuration and state

Use strict YAML with unknown-key rejection. Parse `apiVersion` and `kind` first; support only `apiVersion: cohotfs.io/v1alpha1` and `kind: Workspace` until an explicit migration is implemented. Precedence is command flags over `~/.cohotfs/projects/<source-hash>/workspace.yaml`, `~/.cohotfs/config.yaml` defaults, then built-ins. Host-local runtime endpoints, permitted external roots, Chrome paths, and credential variable mappings live in `~/.cohotfs/config.yaml`; project configuration cannot expand those grants. `<source-hash>` is the first 32 lowercase hex characters of SHA-256 over the canonical source path, and the project document stores and validates the full path and digest to reject collisions.

The local root layout is fixed; tests inject a temporary root through internal constructors rather than exposing a production relocation option:

```text
~/.cohotfs/
  bin/                              # optional local release binaries/Windows companion
  config.yaml                       # user host/runtime/integration policy
  projects/<source-hash>/workspace.yaml
  state/workspaces/<id>/            # workspace JSON, operation journal, grants
  state/images/                     # resolved image/build metadata
  workspaces/<id>/home/             # private container home backing state
  workspaces/<id>/toolchains/       # upper/work/merged and isolated caches
  workspaces/<id>/agent-seeds/      # filtered non-secret settings
  workspaces/<id>/system/           # container-root-only host key and system state
  ssh/id_ed25519                    # Cohotfs-only host client key
  ssh/known_hosts
  browser/<id>/                     # Linux profile; Windows profile is tunneled from this path
  cache/                            # disposable discovery/build cache
  logs/                             # redacted host/setup logs
  run/                              # host.sock, per-workspace sockets, PID/lease files
  tmp/                              # atomic-write and build staging
```

Directories containing keys, broker endpoints, state, browser profiles, or runtime sockets are mode `0700`; regular secret/key files are `0600`; public keys and non-secret manifests may be `0644`. Refuse startup when the root or a security-sensitive ancestor inside it is group/other-writable, belongs to another UID, or resolves through a symlink. Because pathname Unix sockets have a short platform limit, derive socket names from the 128-bit workspace ID and validate the fully expanded `~/.cohotfs/run/...` path before creation. If that path is too long, return typed unavailable before side effects and tell the operator to use a shorter home/root path; do not substitute a TCP transport. `run/` is cleaned by identity-checked reconciliation, not wholesale deletion.

`~/.cohotfs/config.yaml` uses a separate strict document; `auto` probes local endpoints only and never selects a remote daemon:

```yaml
apiVersion: cohotfs.io/v1alpha1
kind: HostConfig
runtime:
  preferred: docker
  docker:
    endpoint: auto                  # local Unix socket/context only in v0.1
    gvisorRuntime: ""               # exact Engine runtime alias when enabled
  containerd:
    endpoint: /run/containerd/containerd.sock
    namespace: cohotfs
    buildkitEndpoint: ""
    gvisorRuntime: io.containerd.runsc.v1
  crio:
    endpoint: /var/run/crio/crio.sock
    buildkitEndpoint: ""
    gvisorHandler: ""
browser:
  linuxExecutable: ""
  windowsExecutable: ""
toolchains:
  goRoot: auto
  rustToolchain: auto
credentials:
  agentEnvironment:
    omp: {}
    codex: {}
    claude: {}
```

Onboarding fills executable/toolchain candidates only after canonical-path and version checks. Each `agentEnvironment` map is `DESTINATION_NAME: HOST_SOURCE_NAME`; destination names must come from that agent descriptor’s fixed allowlist. Docker `auto` honors the current Docker context/`DOCKER_HOST` only when it resolves to a local Unix endpoint; TLS/TCP and SSH endpoints are reported unsupported in v0.1 because client-local source, socket, and `~/.cohotfs` bind mounts cannot be guaranteed. Containerd/CRI/BuildKit endpoints remain dormant until their milestone and must be local Unix sockets.

The machine-local project policy has this exact shape; omitted integration blocks retain fail-closed defaults:

```yaml
apiVersion: cohotfs.io/v1alpha1
kind: Workspace
metadata:
  name: api-service
spec:
  runtime:
    backend: docker                 # docker | containerd | crio
    isolation: standard             # standard | gvisor
  image:
    ref: ghcr.io/gosuda/cohotfs/workspace-base:<cohotfs-version>
    pullPolicy: always              # always | never; never resolves an already-loaded local image
    # build is mutually exclusive with ref:
    # build:
    #   context: .
    #   containerfile: Containerfile
    #   target: ""
    #   args: {}
  workspace:
    source: .
    target: /workspace
  setup:
    mode: manual                     # once | always | manual
    command: ["/bin/true"]
    timeout: 15m
  resources:
    enabled: false                    # false means pass no Cohotfs CPU/memory/PID/ulimit constraints
    cpu: 2
    memory: 4GiB
    memorySwap: 5GiB                  # total memory+swap when enabled; must be >= memory
    pids: 512
    nofile:
      soft: 1024
      hard: 4096
  integrations:
    hostToolchains:
      enabled: false
      persistence: workspace         # workspace | session
      requireCow: false
      go:
        enabled: false
        root: auto
        caches: cow                   # cow | isolated
      rust:
        enabled: false
        toolchain: auto
        caches: cow                   # cow | isolated
    browser:
      enabled: false
      platform: auto                 # linux | windows-wsl | auto
      executable: ""                 # explicit absolute path after onboarding
      retainProfile: false
    sshAgent:
      enabled: false
    gitCredentials:
      enabled: false
      allowedContexts: []             # exact scheme/host/optional port/path
    agents:
      omp:
        enabled: false
        config: seed
        import:
          enabled: true
          binary: true
          natives: true
          models: true
          config: true
          requireCow: false
      codex:
        enabled: false
        config: seed
      claude:
        enabled: false
        config: seed
```

`image.ref` and `image.build` are mutually exclusive. `image.pullPolicy` is `always` for release references; `never` performs an exact local Engine image inspection without registry access and fails unavailable when the image is absent. Source-build `dev` policies select `never` explicitly. Relative project paths resolve beneath the canonical source directory and may not escape it. Runtime endpoint paths, browser executable paths, host toolchain paths, credential providers, and secret values are forbidden in the project policy; their exact candidates belong in `~/.cohotfs/config.yaml` and must remain inside its permitted roots. Per-project machine-local policies live under `~/.cohotfs/projects/`, so `cohotfs init` does not edit `.gitignore`, `.git/info/exclude`, or any other repository file.

A workspace has a random immutable 128-bit base32 ID and an owner-unique display name matching `[a-z0-9][a-z0-9._-]{0,62}`. Persist schema version, owner UID/GID, canonical source, manifest digest, backend and opaque runtime IDs, negotiated capabilities, image digest, container UID/GID, mount manifest, SSH host-key fingerprint, setup digest/result, integration grants, status, and timestamps. Persist no token, private key, credential-helper output, CDP WebSocket URL, or agent auth database. Lifecycle states are `creating`, `starting`, `setup`, `ready`, `setup_failed`, `stopping`, `stopped`, `removing`, and `error`; every created backend resource is recorded before the next operation so reconciliation can clean partial failures.

### Runtime contract and rollout

Define a portable lifecycle only for operations all backends can truthfully provide:

```go
type Lifecycle interface {
    Probe(context.Context) (BackendInfo, error)
    Pull(context.Context, PullRequest) (ResolvedImage, error)
    Create(context.Context, WorkspaceSpec) (WorkspaceRef, error)
    Start(context.Context, WorkspaceRef) error
    ExecSync(context.Context, WorkspaceRef, ExecRequest) (ExecResult, error)
    Inspect(context.Context, WorkspaceRef) (WorkspaceStatus, error)
    Stop(context.Context, WorkspaceRef, time.Duration) error
    Delete(context.Context, WorkspaceRef) error
}

type Builder interface {
    Build(context.Context, BuildRequest) (ResolvedImage, <-chan BuildEvent, error)
}
```

`WorkspaceRef` is opaque and adapter-owned; CRI stores both pod-sandbox and container IDs. Optional interfaces/capability bits cover interactive runtime exec, managed networks, host-socket bind mounts, host-created overlay mounts, builder support, and runtime selection. A requested unsupported capability returns typed `ErrUnsupported` before creating resources. Do not add arbitrary backend option or annotation passthrough.

Release order is fixed:

1. Docker Engine end-to-end: configured local Unix endpoint, API negotiation, pull, BuildKit-backed Engine build, create/start/inspect/stop/delete, bounded and interactive runtime exec for internal diagnostics, bind mounts, directory-UDS SSH transport, and registered-runtime discovery.
2. Native containerd: dedicated `cohotfs` namespace, pull/unpack, snapshot/OCI/task lifecycle, explicit OCI runtime selection, CNI lifecycle, bounded then interactive exec, and separate BuildKit client. Builds output to a configured registry or trusted OCI import path.
3. CRI/CRI-O: one pod sandbox plus one workspace container, pull-only image service, sandbox networking, mounts, `ExecSync`, and exact cleanup order. Custom builds require BuildKit to push to a registry reachable by CRI-O; never claim CRI can build/import/tag/push.

For `isolation: gvisor`, Docker must find the user-configured alias in Engine `Runtimes`; native containerd uses configured `io.containerd.runsc.v1` only after a create probe; CRI uses a positively reported `runtime_handler`. Absence or probe failure is fatal for that workspace. CRI-O plus gVisor remains experimental and requires explicit user opt-in plus integration evidence; `standard` is never substituted.

### Image compatibility and container identity

- Docker v0.1 supports released `linux/amd64` Cohotfs workspace images. The default is `ghcr.io/gosuda/cohotfs/workspace-base:<cohotfs-version>@sha256:<resolved-digest>`; resolve tags before create and persist the digest. Add `linux/arm64` only when the same image and runtime matrix passes, rather than claiming it from a multi-platform build alone.
- The final stage of `image.build` must derive from the Cohotfs workspace base with matching `bootstrapAPI: v1alpha2`. Builder stages may use any image. Arbitrary prebuilt images, Alpine/musl, scratch/distroless images, read-only roots, and user-supplied `sshd` layouts are unsupported in v0.1 because copying a static Go agent does not supply OpenSSH’s libc/OpenSSL/runtime dependencies.
- The base contains `/usr/local/libexec/cohotfs-agent`, `/usr/sbin/sshd`, `/bin/sh`, and `/.cohotfs/base.json`, but no host key or credential. Before attaching source/integration mounts, create a disposable check container with only generated non-secret bootstrap metadata and run the image entrypoint as `cohotfs-agent check --bootstrap-api v1alpha2`; on success remove it and create the real workspace container. Incompatibility removes the check object and returns `image_incompatible`.
- Start the container bootstrap as `0:0`, but never expose SSH root login. The read-only bootstrap mount supplies requesting UID/GID and the authorized **public** key; it never contains either SSH private key. Before SSH starts, the agent validates or creates `agent` with that numeric UID/GID, `/home/agent`, and `/bin/sh`; any conflicting name/ID fails as `image_identity_conflict`. This preserves writable source ownership without chowning the host tree.
- Render the complete Cohotfs `sshd_config`; never inherit image defaults. Validate it with `sshd -t`, then supervise `sshd -D -e` as a PID-1 child. Required settings bind IPv4 loopback `2222`, select only the generated system host key and mounted authorized public key, enable public-key authentication and TTY, disable PAM/password/keyboard-interactive/root login/user environment/X11/remote or stream-local forwarding/gateway ports/tunnels/DNS, and allow only user `agent`. Local TCP forwarding is restricted by `PermitOpen` to `127.0.0.1:*`. Set `AllowAgentForwarding yes` only for an explicit workspace grant.

### SSH connectivity contract

`sshd` cannot listen on a pathname Unix socket. The host OpenSSH client therefore uses Cohotfs only as `ProxyCommand`; SSH remains end-to-end between host `ssh` and workspace `sshd`.

Primary transport for a local Linux runtime:

1. The host app creates `~/.cohotfs/run/workspaces/<workspace-id>/ssh/` mode `0700` and bind-mounts that directory at `/run/cohotfs/host/ssh`. It verifies that the Docker daemon is local and sees the same source path before selecting this transport.
2. `cohotfs-agent serve` runs `sshd` on container loopback port `2222`, listens on `/run/cohotfs/host/ssh/ssh.sock`, and copies each Unix stream bidirectionally to `127.0.0.1:2222`. The pathname socket is mode `0600`; abstract sockets and `/tmp` are forbidden.
3. `cohotfs ssh-proxy --workspace <id>` reads `~/.cohotfs/state`, revalidates lifecycle state plus runtime object ID/creation nonce and socket type/owner/mode, then copies stdin/stdout bytes without framing; diagnostics go only to stderr.

Docker v0.1 requires the shared-directory UDS transport. Docker’s host-port publication targets the container’s bridge address and therefore cannot reach `sshd` bound to container loopback; `loopback_publish` is not advertised. If a runtime/VM cannot share the verified pathname socket, planning returns typed `ErrUnsupported` before side effects. Do not add a reverse bridge, runtime-exec transport, unauthenticated routable listener, or wider `sshd` bind as fallback.

`cohotfs port-forward <port> [--workspace <name-or-id>]` uses authenticated
OpenSSH local forwarding over that transport. The host listener defaults to
`127.0.0.1:<local-port>`; the explicit `--host 0.0.0.0` option exposes it on
every IPv4 interface. The permitted container target remains fixed to
`127.0.0.1:<port>`. Datagram and a second application-specific shared Unix
socket are not part of the public contract; keeping forwarding at the SSH layer
lets a future runtime replace the underlying SSH byte transport without
changing application forwarding semantics.

Onboarding generates `~/.cohotfs/ssh/id_ed25519` with `ssh-keygen`, never mounts its private half, and supplies only its public key as `authorized_keys`. At first bootstrap, container root generates the per-workspace Ed25519 server key in `/var/lib/cohotfs/system/ssh/`, a bind backed by `~/.cohotfs/workspaces/<id>/system/`; it is mode `0600` and root-owned **inside the container**, so the login UID cannot read it. The agent reports only the public key/fingerprint over readiness and the host app pins it under alias `cohotfs/<workspace-id>` in `~/.cohotfs/ssh/known_hosts`. On rootful Docker the backing file may be host-root-owned; removal first runs an identity-matched root cleanup through the runtime, then removes the empty host directory. Never chown it to the host user merely to simplify cleanup. `StrictHostKeyChecking=yes`, password/root login remain disabled, and rotation requires explicit `workspace rotate-host-key` plus repinning.

### Host toolchain copy-on-write contract

The invariant is narrow and testable: **no write may reach a declared host toolchain root or host cache lower directory**. The project workspace remains a deliberate writable bind, so arbitrary agent commands may still edit source files.

- Discover only architecture-compatible native Linux Go/Rust installations. A Windows `go.exe`, Cargo, or rustup tree under `/mnt/c` is never mounted into a Linux container.
- Mount selected Go `GOROOT` and one resolved Rust toolchain directory read-only with private propagation, `nodev,nosuid`, and executable files allowed. Do not mount host rustup proxies or a whole Cargo/Rust home.
- The host app creates each cache view below `~/.cohotfs/workspaces/<id>/toolchains/` from a read-only host lower plus unique `upper`, empty same-filesystem `work`, and `merged` directories. It uses unprivileged kernel OverlayFS only after a mount probe, otherwise an installed `fuse-overlayfs` child whose PID/lifecycle is recorded under `~/.cohotfs/run/`; only `merged` is writable in the container. Persistent uppers are keyed by workspace and lower/toolchain fingerprint; session uppers are removed only after successful unmount. Never share an upper/work directory, use `volatile`/`metacopy`, chown a lower, place upper/work on DrvFS/NFS, or ask for sudo.
- If OverlayFS/xattr/`d_type`/UID mapping/runtime injection probes fail, use empty Cohotfs-managed isolated caches unless `requireCow: true`, in which case creation fails before the container starts. Never degrade to a writable host bind.
- Set managed `HOME`, XDG roots, and temp directories. For Go, set `GOROOT`, `GOTOOLCHAIN=local`, `GOMODCACHE`, `GOCACHE`, `GOPATH`, `GOBIN`, `GOENV`, and `GOTMPDIR` to mounted toolchain or Cohotfs state paths; reject `GODEBUG=installgoroot=all`. For Rust, execute the selected toolchain’s real `cargo`/`rustc`, set `RUSTC`, `RUSTDOC`, `CARGO_HOME`, `CARGO_TARGET_DIR`, `CARGO_INSTALL_ROOT`, and `TMPDIR`; COW only host Cargo `registry/` and `git/`, excluding credentials, config, binaries, and rustup state.

### Chrome, Git, SSH-agent, and AI-agent integration contracts

All integrations are explicit per-workspace grants and absent by default.

- **Linux Chrome:** the host app launches the configured canonical executable as the current user with a fresh `~/.cohotfs/browser/<id>/` profile, `--remote-debugging-port=0`, and no normal browser profile. It reads `DevToolsActivePort`, verifies `/json/version` over loopback, then exposes `~/.cohotfs/run/workspaces/<id>/cdp.sock`; no CDP TCP port is published. The optional host process retains the lease. Teardown kills only the recorded process group and deletes the profile unless `retainProfile: true`.
- **WSL native Windows Chrome:** ship `cohotfs-windows-bridge.exe` under `~/.cohotfs/bin/`. The Linux app creates the profile at Linux `~/.cohotfs/browser/<workspace-id>`, converts that exact path with `wslpath -w`, and passes the resulting `\\\\wsl.localhost\\<distro>\\...\\.cohotfs\\browser\\<id>` path to the companion as Chrome’s `--user-data-dir`. The companion rejects any path not round-tripping to the canonical Linux Cohotfs root, owns Chrome in a Windows Job Object, and returns readiness over stdio; each tunnel copies stdio to Windows-loopback Chrome with no routable listener. If Chrome cannot safely use the WSL-backed profile, Windows interop is disabled, or the companion is missing, browser integration is unavailable—do not create `%USERPROFILE%` state outside the requested root.
- **Container CDP:** mount `~/.cohotfs/run/workspaces/<id>/cdp.sock` when the runtime can see it. `cohotfs-agent` exposes only a container-loopback HTTP/WebSocket proxy, rewrites discovery WebSocket URLs to that endpoint, and exports `COHOTFS_CDP_URL`. It does not claim CDP is an origin/egress sandbox. Remote Docker/Desktop/gVisor combinations must pass the exact transfer probe or report unsupported; never widen Chrome to `0.0.0.0` or use `--remote-allow-origins=*` as fallback.
- **SSH agent:** when enabled, `cohotfs shell`, `cohotfs exec`, and `cohotfs agent run` add host OpenSSH agent forwarding (`-A`) and the server enables `AllowAgentForwarding` for that workspace. Do not bind-mount `SSH_AUTH_SOCK`, `.ssh`, or private keys. The grant warns that workspace code can request signatures while the session is open.
- **Git HTTPS credentials:** inject an ephemeral Git config that resets inherited helpers and selects `git-credential-cohotfs`. The helper sends only Git’s line protocol through the workspace broker endpoint under `~/.cohotfs/run/`. The per-user host process checks an exact scheme/host/port/path allowlist and executes configured host `git credential fill` as the same user; `credential.useHttpPath=true` is mandatory for path-scoped rules. `get` is supported; `store` and `erase` return unsupported. Secrets are returned only to the requesting Git process, never logged or persisted.
- **AI-agent settings:** descriptor-driven discovery supports OMP, Codex, and Claude independently. It stages only explicitly allowlisted non-secret settings into workspace-owned state and copies them into the container’s private writable home at bootstrap; it never bulk-mounts a host agent directory. Exclude OMP `agent.db`, Codex `auth.json`, Claude `~/.claude.json`, histories, sessions, caches, logs, browser profiles, and keyring data. Resolve OMP profile/XDG/`PI_CONFIG_DIR`, Codex `CODEX_HOME`, and documented Claude paths dynamically; missing candidates disable only that seed.
- **AI-agent credentials:** OAuth/session databases are not mounted or parsed. When an agent descriptor and `~/.cohotfs/config.yaml` define a mapping, `cohotfs agent run` creates a one-use 60-second in-memory broker lease from the allowlisted host environment variable to the fixed agent destination. The container wrapper fetches it over the workspace endpoint below `~/.cohotfs/run/` immediately before `exec`; the host process deletes it after first retrieval or expiry, and no argument, file, log, or durable state contains it. Agents requiring file-only OAuth authenticate separately into their isolated workspace home.

## Ordered implementation approach

### 1. Establish the Go project, API types, and deterministic CLI

- Initialize module `github.com/gosuda/cohotfs` with `go 1.26.0` and `toolchain go1.26.5` in `go.mod` (current release verified during planning); provide `Makefile` targets that call ordinary Go commands rather than hiding logic. Pin direct dependencies and commit `go.sum`.
- Create `cmd/cohotfs`, `cmd/cohotfs-agent`, and `cmd/cohotfs-windows-bridge`. `cohotfs host serve` is a hidden/internal-lifecycle subcommand of the same host binary, not a separately installed daemon executable. Keep each `main` limited to signal setup, dependency construction, and an `internal/cli`/service call. Windows bridge files use build tags; the other targets build for Linux.
- Use the standard library for the optional host process’s HTTP-over-Unix transport and structured JSON, `github.com/spf13/cobra` v1.10.2 for the command tree, `go.yaml.in/yaml/v3` v3.0.5 with `KnownFields(true)` for strict YAML, and `golang.org/x/sys` v0.47.0 for Linux fd/socket/mount primitives. Docker phase adds `github.com/moby/moby/client` v0.5.1. Containerd phase adds `github.com/containerd/containerd/v2` v2.3.3, `github.com/moby/buildkit` v0.32.2, `github.com/containerd/go-cni` v1.1.13, and `github.com/containernetworking/cni` v1.3.0. CRI phase adds `k8s.io/cri-api` v0.36.3. Do not introduce a plugin system, ORM, or generic dependency-injection framework.
- Implement `internal/hostroot` first: resolve the current UID’s home, create and validate the exact `~/.cohotfs` layout/modes, provide fd-relative path operations, and expose a test-only constructor for temporary roots. Then implement strict project/user config loading, field validation, canonical rendering, precedence, and redacted diagnostics.
- Define generated-free request/response structs under `internal/api` only for host-process integration leases; ordinary workspace commands call services in-process. Mutating operations carry an idempotency key in the journal, bound to operation plus body digest, so an interrupted CLI retry returns the terminal result and rejects a reused key with different input.
- Implement `internal/state` as versioned JSON files written by file `fsync`, atomic rename, then parent-directory `fsync` beneath `~/.cohotfs/state/`. Serialize one lifecycle mutation per workspace and one image/build mutation per image key with advisory locks under `~/.cohotfs/run/`; reads use immutable snapshots.
- Establish stable exit codes: `0` success, `2` CLI/config usage, `3` unavailable capability, `4` policy denial, `5` workspace state conflict, `6` runtime/integration failure, and `7` partial cleanup requiring `cohotfs recover`.

The command tree is exact:

```text
cohotfs init
cohotfs onboard [--non-interactive]
cohotfs doctor [--output text|json]
cohotfs config show|validate
cohotfs runtime list|capabilities
cohotfs workspace create|list
cohotfs workspace start|stop|restart|status|remove|recover|rotate-host-key [--workspace <name-or-id>]
cohotfs image pull|build
cohotfs setup validate
cohotfs setup run [--workspace <name-or-id>]
cohotfs shell [--workspace <name-or-id>]
cohotfs exec [--workspace <name-or-id>] -- <command...>
cohotfs port-forward <port> [--local-port <port>] [--host 127.0.0.1|0.0.0.0] [--workspace <name-or-id>]
cohotfs ssh-proxy --workspace <id>
cohotfs agent discover
cohotfs agent run <omp|codex|claude> [--workspace <name-or-id>] -- <args...>
cohotfs host status|stop
```

Workspace-targeting commands resolve an omitted `--workspace` by exact canonical
source equality with the current directory. No match or more than one match is
a typed state conflict; explicit `--workspace <name-or-id>` is required to
disambiguate. Positional workspace arguments are not supported.

Every discovery/status/list command supports `--output json` with stable field names. Mutating commands are human-readable only in the first release and support `--yes` solely for a prompt already represented in configuration; no flag bypasses host config grants.

### 2. Establish the local host lifecycle and recovery

- Implement direct in-process mode first: lock workspace state, reconcile its recorded runtime/mount/socket identities, compile a complete plan, perform the operation, and atomically journal each acquired external resource. A second CLI process receives a state-conflict error rather than racing.
- Add `cohotfs host serve` only for persistent Chrome, Git/CDP broker, and `fuse-overlayfs` leases. Spawn it with fixed argv and sanitized environment, wait for `~/.cohotfs/run/host.sock`, record PID plus Linux process start time, and reuse it only when UID, executable identity, protocol version, and root match. `host stop` refuses while leases remain unless `--yes`, then drains leases and performs identity-checked cleanup.
- On every CLI start, reconcile nonterminal workspaces and recorded child PIDs before mutation: compare runtime labels/nonce, PID start time, socket inode/type/owner, and `/proc/self/mountinfo`; unmount/remove only exact records and quarantine ambiguity. Never glob-delete under `run/`, kill by stale PID alone, or remove an unrecognized runtime object.
- Write redacted audit/events to `~/.cohotfs/logs/` with bounded rotation. Record operation, workspace, external resource type, and result; never record environment values, Git helper payloads, SSH bytes, CDP frames, build secret contents, or agent tokens.
- Release archives contain `cohotfs`, `cohotfs-agent`, and the Windows companion. `cohotfs onboard` copies version-matched helpers into `~/.cohotfs/bin/` atomically and records their SHA-256 values before runtime use. It never invokes sudo, installs a system unit, edits runtime configuration, or writes outside `~/.cohotfs`.

### 3. Build the image and bootstrap contract

- Add `images/workspace-base/Containerfile`, based on a release-pinned Debian stable-slim digest and dated Debian Snapshot. Install `openssh-server`, CA certificates, Git, and shell/core utilities; copy the static agent to `/usr/local/libexec/cohotfs-agent`; remove package-generated SSH host keys; write `/.cohotfs/base.json`; publish immutable digest-addressed `linux/amd64` releases to `ghcr.io/gosuda/cohotfs/workspace-base`. Record base digest, snapshot date, package versions, agent digest, SBOM, provenance, and resulting OCI digest.
- `cohotfs-agent serve` validates PID 1 and bootstrap API, creates/validates the runtime `agent` identity from requesting UID/GID, reaps children, renders and validates the fixed `sshd_config`, starts `sshd -D -e` on `127.0.0.1:2222`, starts enabled socket helpers, emits readiness over `/run/cohotfs/control/ready.sock`, forwards termination, and exits nonzero if a mandatory child dies.
- Enforce the settled base-image inheritance contract. `cohotfs image build` accepts arbitrary builder stages but requires the final stage to retain the matching Cohotfs base and marker. A prebuilt `image.ref` is accepted only when `cohotfs-agent check --bootstrap-api v1alpha2` passes before host source/integration mounts; Cohotfs never installs packages or copies an agent into an arbitrary pulled image at workspace start.
- Build contexts are canonical paths allowed by `~/.cohotfs/config.yaml`. Stream a generated tar that honors `.dockerignore`, rejects sockets/devices and paths escaping through symlinks, caps total bytes/file count, excludes `~/.cohotfs` and known secret files by default, and shows the inclusion plan before the first build. Build arguments are non-secret strings; BuildKit secret/SSH mounts are separate ephemeral grants and never serialized. Delegate cache keys/invalidation to BuildKit; Cohotfs stores only the resolved output digest.
- For containerd and CRI milestones, keep the same Containerfile/base contract but use a configured BuildKit endpoint. Push by immutable digest to an allowed registry; only native containerd may additionally import a trusted OCI archive. CRI always pulls from the registry.

### 4. Implement Docker workspace lifecycle end to end

- Implement the adapter contract with the official Moby client, endpoint/API negotiation, capability probe, image pull/build event streaming, registered runtime lookup, container lifecycle, bounded exec, and interactive diagnostic exec. The runtime endpoint stays in the host process and is never mounted into a workspace. Do not advertise managed-network or loopback-publication capabilities until their exact lifecycle and transfer paths have positive integration probes.
- Build a `WorkspacePlan` before side effects: resolve owner, source, image digest, image compatibility, container UID/GID, resource/security settings, every mount, network mode, SSH transport, integrations, setup, and required adapter capabilities. Render it redacted, persist under `~/.cohotfs/state/workspaces/<id>/`, then create resources transactionally.
- Default container security remains non-privileged, without runtime sockets/devices/host PID or IPC namespaces/added capabilities, with `no-new-privileges`, runtime default seccomp/AppArmor/SELinux, and Docker’s standard bridge network. **Resource constraints are disabled by default:** when `spec.resources.enabled: false`, pass no Cohotfs CPU, memory, swap, PID, or `nofile` limit fields and inherit only runtime/host defaults. When enabled, use the manifest’s recommended starting values (2 CPUs, 4 GiB memory, 5 GiB total memory+swap, 512 PIDs, `nofile` 1024 soft/4096 hard); validate positive CPU/memory/PIDs, `memorySwap >= memory`, and `soft <= hard`. Users may raise or lower these values without Cohotfs product caps, subject only to numeric overflow checks and backend/host feasibility; Cohotfs never silently clamps them.
- Label every runtime object with owner UID, workspace ID, manifest digest, and a random creation nonce. On inspect/stop/delete/recovery, require every identity value to match state. Cleanup is stop then remove; a missing matching object is idempotent success, while a conflicting object is quarantined and returns exit `7`.
- Wire the SSH directory transport, wait for both `cohotfs-agent` and SSH readiness, pin the generated host key, and make `cohotfs shell`, `exec`, `port-forward`, `scp` guidance, and `ssh-proxy` use the host OpenSSH process without stdout contamination. Fail typed unavailable before creation when the directory transport cannot be carried.

### 5. Add setup and custom provisioning

- `setup validate` canonicalizes the argv executable/script beneath the project, verifies files included in the workspace, computes an audit digest over the canonical setup block, resolved image digest, bootstrap API, and owner UID/GID, and reports command/user/timeout without execution. It rejects shell-string commands, missing/non-regular scripts, writable-by-other host scripts, and paths outside the source.
- `setup run` invokes `cohotfs-agent setup --timeout <duration> -- <argv...>` as `agent` in `/workspace`, with sanitized managed HOME/XDG/TMP environment. The wrapper creates a process group, caps captured combined output at 1 MiB with a truncation flag, sends TERM on timeout, waits 10 seconds, then KILLs the group. `once` means one successful automatic run per immutable workspace ID and never reruns merely because inputs changed; explicit `setup run` retries a failure and `setup run --force` reruns a success. `always` runs on every start before ready; `manual` runs only by command. Failure sets `setup_failed`, preserves the stopped container and bounded diagnostics, and exposes no shell/agent access until retry or removal.
- Image-level root provisioning belongs in the Containerfile, not `setup.command`. Setup executes untrusted repository code after declared mounts/integration grants are installed; onboarding must show those grants before the first run.

### 6. Add local COW host-toolchain imports

- Implement Go and Rust discovery as current-user probes. Emit canonical candidate path, executable version, OS/architecture/ABI, cache roots, and fingerprint; require explicit selection when multiple candidates exist and persist it only in `~/.cohotfs/config.yaml`.
- Add the user-space overlay manager with exact lower/upper/work/merged rules, mount-reference counting, PID/start-time records for `fuse-overlayfs`, reconciliation via mountinfo, and quarantine after failed unmount. Validate that lower sources are within user-config-permitted roots and that every writable backing path remains under `~/.cohotfs`.
- Compile the selected mounts/env into `WorkspacePlan`; make the container-side agent reject inherited environment overrides that point Go/Rust writes outside the managed paths. Preserve the writable source mount; promise immutability only for declared lower paths.
- Implement the isolated-cache fallback and `requireCow` behavior before adding persistent uppers. Then add per-workspace fingerprinted persistence, invalidating to a new upper when a lower fingerprint changes rather than mixing versions.

### 7. Add host bridge integrations independently

- Implement each integration as a separate lease in the optional per-user host process and a separately capability-gated container helper. Workspace start is atomic for every enabled integration: failure tears down newly acquired integration resources and leaves the workspace stopped. Existing workspaces use `workspace restart` after config changes; no hot grant mutation exists in v1alpha1.
- Chrome: implement Linux launch/discovery/process-group cleanup first, then the Unix HTTP/WebSocket proxy and container discovery rewriting, then the WSL Windows companion/Job Object/stdio tunnel. Never attach an existing profile/browser in v1alpha1.
- SSH agent: enable only per host-SSH invocation with `-A`; verify a live host `SSH_AUTH_SOCK` before launch, leave runtime mounts unchanged, and close forwarding with the SSH session.
- Git HTTPS: implement the container helper parser, user config context policy, same-user `git credential fill`, one-request lease, output scrubbing, and get-only behavior. Use fixture providers in tests; never invoke a developer’s real credential store in automated tests.
- Agent settings: implement source descriptors in order OMP, Codex, Claude. Stage included files under `~/.cohotfs/workspaces/<id>/agent-seeds/`, reject symlinks/non-regular files, filter secret-bearing candidates, copy as the container user into an empty private home, and never sync back.
- Agent environment credentials: accept only a user-configured source variable name and fixed per-agent destination allowlist (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or descriptor-declared equivalents). The CLI reads the value, hands it to its same-UID host process over `~/.cohotfs/run/host.sock`, and the container fetches it once over its broker endpoint. Lock/zero buffers best-effort; disable request/audit logging; refuse persistence, arbitrary destinations, and file-backed OAuth emulation.

### 8. Add native containerd, then CRI/CRI-O

- Implement containerd only after all Docker adapter contract tests pass. Use a dedicated namespace, pull/unpack/snapshot/spec/task lifecycle, register wait before task start, explicit OCI mounts/runtime, CNI setup/teardown, and task/process cleanup in the specified order. Capability-probe host sockets, overlays, gVisor, and interactive exec; do not copy Docker network concepts.
- Implement CRI as the constrained sandbox/container pair. Record each ID immediately; cleanup `StopContainer`, `RemoveContainer`, `StopPodSandbox`, then `RemovePodSandbox`. Use CRI ImageService pull only, handler discovery, sandbox CNI/port mappings, explicit mount features, and `ExecSync`; interactive runtime exec remains unsupported until a tested CRI streaming client exists.
- Validate the CRI adapter separately against current containerd CRI and CRI-O. Mark CRI-O+gVisor experimental in capability output and require an explicit local user-config opt-in plus a positively discovered handler; no code path silently switches handlers or security mode.

### 9. Complete onboarding and operator recovery

- `cohotfs onboard` creates/validates `~/.cohotfs`, checks current-user access to Docker and later runtime endpoints, gVisor aliases, OpenSSH, Linux/Windows Chrome, WSL interop, unprivileged OverlayFS/`fuse-overlayfs`, Go/Rust, live SSH agent, Git credential provider availability without requesting a secret, and OMP/Codex/Claude candidates. Interactive mode writes only selected non-secret values to `~/.cohotfs/config.yaml`; non-interactive mode reports without mutation.
- `cohotfs init` writes the complete machine-local project policy under `~/.cohotfs/projects/<source-hash>/workspace.yaml` only when absent, never changes the repository, and never enables credentials/browser/toolchains/agents. It validates the policy and prints the exact `workspace create` command.
- `doctor` performs no mutation. It emits pass/warn/fail plus remediation for `~/.cohotfs` ownership/modes/space, runtime socket access, filesystem/overlay support, build/network/mount support, WSL placement (`/mnt/*` warns; COW backing there fails), image compatibility, SSH transport, Chrome, credentials, agent discovery, stale state, and version skew.
- `workspace recover` operates on one workspace ID, shows exact matching/quarantined resources, and requires `--yes` to remove identity-matched resources. It never glob-deletes sockets/mounts/containers. `host stop` and binary removal refuse while recorded workspaces/mounts remain and point to recovery.

## Critical files and anchors

- `internal/hostroot/root.go` — canonical `~/.cohotfs` layout, permission/ownership checks, fd-relative safe operations, and test-root injection.
- `internal/workspace/service.go` — authoritative plan construction, lifecycle state machine, transactional resource journal, and reconciliation.
- `internal/runtime/runtime.go` — portable lifecycle types, optional capabilities, opaque references, and typed unsupported errors.
- `internal/mount/overlay_linux.go` — unprivileged OverlayFS/`fuse-overlayfs` validation, child lifecycle, and host-lower immutability boundary.
- `cmd/cohotfs-agent/main.go` — custom-image compatibility endpoint, PID 1 behavior, SSH relay, CDP proxy, and credential helper modes.

## Verification

Run ordinary unit/contract tests first from the repository root:

```console
go test ./...
go vet ./...
```

Use test-injected temporary Cohotfs roots and fake process/credential/runtime boundaries for deterministic unit tests. Required tests cover exact `~/.cohotfs` layout and mode enforcement, refusal of symlink/wrong-owner roots, strict config precedence, fd-relative path escape rejection, same-UID host-socket authorization, operation idempotency, redaction/no-secret serialization, lifecycle legality, atomic state recovery, malicious build contexts, setup semantics, SSH raw-stream integrity/host-key mismatch, helper parsing/context denial, agent seed exclusion, and concurrent CLI operations.

Build all shipped artifacts and verify platform boundaries:

```console
CGO_ENABLED=0 go build ./cmd/cohotfs ./cmd/cohotfs-agent
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/cohotfs-windows-bridge
```

Docker integration prerequisites: a local Linux Docker Engine already reachable by the invoking user, BuildKit enabled, OpenSSH client, and permission to create labeled test images/containers. Execute `go test -tags=integration ./integration/docker -run TestWorkspaceEndToEnd -v`; the suite creates its own temporary user home. It must create all host artifacts only under that home’s `.cohotfs`, build the image, create/start, run setup once, connect through host `ssh`, execute `cat setup-result` and observe `ok`, transfer random binary content through SSH/SCP/SFTP, stop/start without rerunning once-mode setup, force a second setup, then remove every labeled container/socket/mount/state record. Separate cases verify `always`, `manual`, a second concurrent mutation, exact resource policies, and typed rejection when no directory transport is available.

Resource-policy integration creates one workspace with the default manifest and inspects the backend request/container: Cohotfs must set no CPU quota/count, memory/swap, PIDs, or `nofile` constraints. Repeat with `enabled: true` and the recommended values, then with larger values (8 CPUs, 32 GiB memory, 64 GiB total memory+swap, 4096 PIDs, `nofile` 65536/65536); inspect must show the exact requested values or return a backend capability error before container creation—never clamp them silently.

SSH transport checks must transfer random binary data through `ssh-proxy` and compare its digest, exercise `ssh`, `scp`, and `sftp`, prove `StrictHostKeyChecking=yes`, reject a replaced host key/socket inode, and verify only stderr receives proxy diagnostics through the directory-UDS transport. The same suite must prove Docker does not advertise `loopback_publish` and that planning without a usable directory socket returns typed unavailable before container creation.

Toolchain integration creates synthetic host Go/Rust roots and cache lowers, records a recursive manifest of names/content/type/mode/UID/GID/xattrs/symlink targets/link counts/timestamps, then runs Go build/download/install/env-write attempts and Cargo fetch/build/install through the workspace. The lower manifest must be byte-for-byte unchanged; writes/whiteouts must exist only in unique uppers or isolated state. Repeat with two concurrent sessions, changed fingerprints, unsupported xattr filesystem, failed unmount recovery, and WSL DrvFS candidate; no case may produce a writable lower bind.

Linux Chrome integration uses a disposable Chrome for Testing binary and a local test origin. Assert a fresh profile, ephemeral loopback Chrome port, no published host/container-network CDP port, successful `COHOTFS_CDP_URL` navigation/screenshot, denial of direct peer/LAN access, and cleanup that leaves an unrelated Chrome process untouched. WSL verification launches native Windows Chrome through the companion under both NAT and mirrored modes, navigates from the container, then proves no routable Windows CDP listener/profile/process remains. WSL interop-disabled and Docker-Desktop socket-probe failures must return capability error `3`, not insecure fallback.

Credential verification uses generated SSH and fake HTTPS credentials. Disabled mode exposes no forwarding/helper/broker/secret state. SSH forwarding works for one `cohotfs exec` and ends with that session. Git returns a token only for one exact context; wrong scheme/host/port/path and `store`/`erase` receive none. Scan image history, workspace files, all of `~/.cohotfs` except the intentionally generated SSH private key fixture, process argv/environment diagnostics, and captured logs for the fixture token and require zero matches.

Agent integration fixtures cover default and overridden/profile/XDG locations for OMP, `CODEX_HOME` plus keyring/no-`auth.json` cases for Codex, and documented Claude settings. Assert only allowlisted settings appear in the private container home; OMP `agent.db`, Codex `auth.json`, Claude `.claude.json`, histories, and caches never appear. A one-use API-key lease succeeds once, fails on replay/expiry, and leaves no value in state/logs.

Containerd verification runs the same portable lifecycle/SSH/setup suite plus CNI teardown and snapshot cleanup. CRI verification runs it against both containerd CRI and CRI-O with `ExecSync`, then asserts image build and interactive runtime exec report `ErrUnsupported`; a BuildKit-to-registry workflow must make the built digest pullable. For every backend, request `isolation: gvisor` once with a configured handler and observe the selected runtime inside the workload; repeat without the handler and require pre-create failure with no standard-runtime container.

Final smoke proof starts from a clean supported Linux user whose Docker access is already configured: extract the release into a user-selected executable directory, run `onboard`, `init`, `workspace create`, `workspace start`, `shell`, one agent command, stop/remove, `host stop`, and remove the release. Repeat in WSL2 with a project on the Linux filesystem and native Windows Chrome. Snapshot the host before/after and require every Cohotfs-created regular file, directory, socket, log, key, profile, and mount backing path to be beneath `~/.cohotfs`; `doctor --output json` must report no stale external runtime object, mount, socket, Chrome process/profile, or secret-bearing state.

## Assumptions and contingencies

- The host app is local and per-user, with all host execution data under `~/.cohotfs`. Docker access must already be granted by the operator; Cohotfs neither installs nor requests privilege. containerd/CRI-O and host COW capabilities fail or use the specified unprivileged fallback when current-user access is insufficient.
- Docker Engine is the only backend required for the first public release. The source layout and adapter contracts include containerd and CRI-O from the start, but their milestones do not block Docker v0.1.
- Linux and WSL2 are first-class. The host app runs inside WSL2; native Windows supports only the Chrome companion. Native Windows/macOS control planes and Windows `ssh.exe` are not implied.
- Docker v0.1 requires a verified pathname Unix socket shared with the local runtime. If it cannot securely carry that socket, workspace creation fails typed unavailable; runtime exec, host-published TCP, wider `sshd` listeners, and reverse bridges are not substituted.
- If host cache OverlayFS is unavailable, isolated Cohotfs caches are mandatory unless `requireCow: true`; host paths are never made writable. If a host toolchain binary is ABI-incompatible, use the image toolchain and report the discovered reason.
- Git/agent brokering cannot hide an explicitly granted credential from the authorized process; it guarantees no image or persistent-state copy. File-only OAuth remains container-local. Direct credential-file mounts are never added.
