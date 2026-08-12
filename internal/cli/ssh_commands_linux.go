//go:build linux

package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/hostservice"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func buildShellCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "shell", Args: noWorkspacePositionalArgs}
	workspaceName := addWorkspaceFlag(command)
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		return withSSHWorkspace(deps, cmd, *workspaceName, func(record state.Workspace, root *hostroot.Root) error {
			return runOpenSSH(cmd.Context(), cmd, root, record, true, nil)
		})
	}
	return command
}

func buildExecCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "exec -- <command...>", Args: cobra.MinimumNArgs(1)}
	workspaceName := addWorkspaceFlag(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		argv := args
		if len(argv) != 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		if len(argv) == 0 {
			return apperr.New(apperr.ExitUsage, "usage", "exec requires a command")
		}
		return withSSHWorkspace(deps, cmd, *workspaceName, func(record state.Workspace, root *hostroot.Root) error {
			return runOpenSSH(cmd.Context(), cmd, root, record, false, argv)
		})
	}
	return command
}

func buildAgentCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "agent"}
	discover := &cobra.Command{
		Use:  "discover",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, _ := cmd.Flags().GetString("output")
			type candidate struct {
				Agent      string `json:"agent"`
				Executable string `json:"executable,omitempty"`
				Available  bool   `json:"available"`
			}
			candidates := make([]candidate, 0, 3)
			for _, agent := range []string{"omp", "codex", "claude"} {
				executable := discoverExecutable(agent)
				candidates = append(candidates, candidate{Agent: agent, Executable: executable, Available: executable != ""})
			}
			if format == "json" {
				return writeJSON(cmd, candidates)
			}
			if format != "text" {
				return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
			}
			for _, candidate := range candidates {
				status := "unavailable"
				if candidate.Available {
					status = "available"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", candidate.Agent, status, candidate.Executable)
			}
			return nil
		},
	}
	discover.Flags().String("output", "text", "output format: text or json")
	run := &cobra.Command{Use: "run <omp|codex|claude> -- <args...>", Args: cobra.MinimumNArgs(1)}
	workspaceName := addWorkspaceFlag(run)
	run.RunE = func(cmd *cobra.Command, args []string) error {
		agent := args[0]
		if agent != "omp" && agent != "codex" && agent != "claude" {
			return apperr.New(apperr.ExitUsage, "agent", "unknown agent %q", agent)
		}
		agentArgs := args[1:]
		if len(agentArgs) != 0 && agentArgs[0] == "--" {
			agentArgs = agentArgs[1:]
		}
		return withSSHWorkspace(deps, cmd, *workspaceName, func(record state.Workspace, root *hostroot.Root) error {
			if !record.IntegrationGrants["agent:"+agent] {
				return apperr.New(apperr.ExitPolicy, "agent", "%s integration is not granted", agent)
			}
			hostConfigPath, _ := root.HostPath("config.yaml")
			hostConfig, err := config.LoadHost(hostConfigPath)
			if err != nil {
				return err
			}
			client, err := hostservice.EnsureRunning(cmd.Context(), root)
			if err != nil {
				return err
			}
			leases, err := issueAgentEnvironment(cmd.Context(), client, record.ID, agent, hostConfig.Credentials.AgentEnvironment[agent])
			if err != nil {
				return err
			}
			remote := []string{"/usr/local/libexec/cohotfs-agent", "agent-run", "--workspace", record.ID}
			for _, leaseID := range leases {
				remote = append(remote, "--lease", leaseID)
			}
			remote = append(remote, "--", agent)
			remote = append(remote, agentArgs...)
			return runOpenSSH(cmd.Context(), cmd, root, record, false, remote)
		})
	}
	command.AddCommand(discover, run)
	return command
}

func issueAgentEnvironment(ctx context.Context, client *hostservice.Client, workspaceID, agent string, mappings map[string]string) ([]string, error) {
	destinations := make([]string, 0, len(mappings))
	for destination := range mappings {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	leases := make([]string, 0, len(mappings))
	for _, destination := range destinations {
		source := mappings[destination]
		value, present := os.LookupEnv(source)
		if !present || value == "" {
			return nil, fmt.Errorf("configured source environment variable %s is unavailable", source)
		}
		secret := []byte(value)
		response, err := client.IssueAgentSecret(ctx, api.AgentSecretIssueRequest{
			WorkspaceID: workspaceID, Agent: agent, Destination: destination, SourceVariable: source, Value: secret,
		})
		for index := range secret {
			secret[index] = 0
		}
		if err != nil {
			return nil, err
		}
		leases = append(leases, response.LeaseID)
	}
	return leases, nil
}

func withSSHWorkspace(deps Dependencies, cmd *cobra.Command, name string, fn func(state.Workspace, *hostroot.Root) error) error {
	return withRoot(deps, func(root *hostroot.Root) error {
		store, err := state.NewStore(root)
		if err != nil {
			return err
		}
		record, err := resolveWorkspaceSelection(store, name)
		if err != nil {
			return err
		}
		if record.Status != state.StatusReady {
			return apperr.New(apperr.ExitStateConflict, "state_conflict", "workspace %s is not ready", record.ID)
		}
		return fn(record, root)
	})
}

func openSSHBaseArguments(root *hostroot.Root, record state.Workspace) (string, []string, error) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return "", nil, apperr.Wrap(apperr.ExitUnavailable, "ssh", err, "OpenSSH client is unavailable")
	}
	privateKey, _ := root.HostPath("ssh/id_ed25519")
	if err := validatePrivateKey(privateKey, root.UID()); err != nil {
		return "", nil, err
	}
	knownHosts, err := validateKnownHosts(record)
	if err != nil {
		return "", nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", nil, err
	}
	proxy := shellQuote(executable) + " ssh-proxy --workspace " + shellQuote(record.ID)
	arguments := []string{"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "ProxyCommand=" + proxy, "-i", privateKey}
	return sshPath, arguments, nil
}

func runOpenSSH(ctx context.Context, cmd *cobra.Command, root *hostroot.Root, record state.Workspace, terminal bool, remote []string) error {
	sshPath, arguments, err := openSSHBaseArguments(root, record)
	if err != nil {
		return err
	}
	if record.IntegrationGrants["sshAgent"] {
		if err := validateSSHAgent(os.Getenv("SSH_AUTH_SOCK"), root.UID()); err != nil {
			return apperr.Wrap(apperr.ExitPolicy, "ssh_agent", err, "SSH agent forwarding grant is unusable: %v", err)
		}
		arguments = append(arguments, "-A")
	}
	if terminal {
		arguments = append(arguments, "-t")
	} else {
		arguments = append(arguments, "-T")
	}
	arguments = append(arguments, "agent@cohotfs-"+record.ID)
	if len(remote) != 0 {
		quoted := make([]string, len(remote))
		for index, item := range remote {
			quoted[index] = shellQuote(item)
		}
		arguments = append(arguments, strings.Join(quoted, " "))
	}
	ssh := exec.CommandContext(ctx, sshPath, arguments...)
	ssh.Stdin = cmd.InOrStdin()
	ssh.Stdout = cmd.OutOrStdout()
	ssh.Stderr = cmd.ErrOrStderr()
	return ssh.Run()
}

func ensureSSHClientKey(root *hostroot.Root) error {
	if err := root.EnsureDir("run/locks", 0o700); err != nil {
		return err
	}
	lock, err := root.OpenFile("run/locks/ssh-key.lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	privatePath, err := root.HostPath("ssh/id_ed25519")
	if err != nil {
		return err
	}
	publicPath, err := root.HostPath("ssh/id_ed25519.pub")
	if err != nil {
		return err
	}
	privateData, err := root.ReadFile("ssh/id_ed25519")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		if _, publicErr := os.Lstat(publicPath); publicErr == nil {
			return fmt.Errorf("SSH public key exists without its private key")
		} else if !errors.Is(publicErr, os.ErrNotExist) {
			return publicErr
		}
		public, private, generateErr := ed25519.GenerateKey(rand.Reader)
		if generateErr != nil {
			return generateErr
		}
		block, marshalErr := ssh.MarshalPrivateKey(private, "cohotfs")
		if marshalErr != nil {
			return marshalErr
		}
		privateData = pem.EncodeToMemory(block)
		publicKey, publicErr := ssh.NewPublicKey(public)
		if publicErr != nil {
			return publicErr
		}
		if err := root.AtomicWrite("ssh/id_ed25519", privateData, 0o600); err != nil {
			return err
		}
		if err := root.AtomicWrite("ssh/id_ed25519.pub", ssh.MarshalAuthorizedKey(publicKey), 0o644); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := validatePrivateKey(privatePath, root.UID()); err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(privateData)
	if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("SSH client private key is not a valid Ed25519 key")
	}

	publicData, err := root.ReadFile("ssh/id_ed25519.pub")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		if err := root.AtomicWrite("ssh/id_ed25519.pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
			return err
		}
		publicData, err = root.ReadFile("ssh/id_ed25519.pub")
	}
	if err != nil {
		return err
	}
	if err := validatePublicKey(publicPath, root.UID()); err != nil {
		return err
	}
	publicKey, _, _, rest, err := ssh.ParseAuthorizedKey(publicData)
	if err != nil || len(bytes.TrimSpace(rest)) != 0 || publicKey.Type() != ssh.KeyAlgoED25519 || !bytes.Equal(publicKey.Marshal(), signer.PublicKey().Marshal()) {
		return fmt.Errorf("SSH client public key does not match its private key")
	}
	return nil
}

func validatePublicKey(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	permissions := info.Mode().Perm()
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || permissions&0o400 == 0 || permissions&0o133 != 0 || !ok || int(stat.Uid) != uid {
		return fmt.Errorf("SSH client public key identity or mode is invalid")
	}
	return nil
}

func validatePrivateKey(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(stat.Uid) != uid {
		return fmt.Errorf("Cohotfs SSH private key identity is unsafe")
	}
	return nil
}

func validateKnownHosts(record state.Workspace) (string, error) {
	for _, resource := range record.Resources {
		if resource.Type != "ssh_known_hosts" || resource.ReleasedAt != nil {
			continue
		}
		data, err := os.ReadFile(resource.Identity["path"])
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		_, _, key, _, _, parseErr := ssh.ParseKnownHosts(data)
		if hex.EncodeToString(sum[:]) != resource.Identity["sha256"] || parseErr != nil || ssh.FingerprintSHA256(key) != record.SSHHostFingerprint {
			return "", fmt.Errorf("workspace known-host entry identity mismatch")
		}
		return resource.Identity["path"], nil
	}
	return "", errors.New("workspace known-host entry is unavailable")
}

func validateSSHAgent(path string, uid int) error {
	if path == "" {
		return fmt.Errorf("SSH_AUTH_SOCK is unset")
	}
	if _, err := proc.ReadSocket(path, uid); err != nil {
		return err
	}
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
