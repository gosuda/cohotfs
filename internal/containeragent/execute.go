package containeragent

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gosuda/cohotfs/internal/toolchain"
)

var Version = "dev"

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if os.Getenv("COHOTFS_MANAGED_TOOLCHAINS") == "1" {
		if err := toolchain.ValidateManagedEnvironment(os.Environ()); err != nil {
			return fmt.Errorf("invalid managed toolchain environment: %w", err)
		}
	}
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, Version)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: cohotfs-agent <serve|ready|cleanup-system|check|setup|ssh-relay|cdp-proxy|git-credential|agent-run>")
	}
	switch args[0] {
	case "check":
		flags := flag.NewFlagSet("check", flag.ContinueOnError)
		flags.SetOutput(stderr)
		bootstrapAPI := flags.String("bootstrap-api", "", "required bootstrap API")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *bootstrapAPI == "" || flags.NArg() != 0 {
			return fmt.Errorf("check requires --bootstrap-api")
		}
		marker, err := CheckImage("/", *bootstrapAPI)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(marker)
	case "ready":
		if len(args) != 1 {
			return fmt.Errorf("ready accepts no arguments")
		}
		ready, err := ProbeReadiness(ctx, defaultReadySocket)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(ready)
	case "cleanup-system":
		flags := flag.NewFlagSet("cleanup-system", flag.ContinueOnError)
		flags.SetOutput(stderr)
		fingerprint := flags.String("fingerprint", "", "expected SSH host-key fingerprint")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *fingerprint == "" {
			return fmt.Errorf("cleanup-system requires --fingerprint")
		}
		bootstrap, err := LoadBootstrap(defaultBootstrapPath)
		if err != nil {
			return err
		}
		return CleanupSystemStorage(bootstrap, *fingerprint)
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(stderr)
		bootstrap := flags.String("bootstrap", defaultBootstrapPath, "bootstrap metadata path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("serve accepts no positional arguments")
		}
		return Serve(ctx, *bootstrap)
	case "ssh-relay":
		flags := flag.NewFlagSet("ssh-relay", flag.ContinueOnError)
		flags.SetOutput(stderr)
		socket := flags.String("socket", defaultSSHRelaySocket, "pathname Unix socket")
		target := flags.String("target", "127.0.0.1:2222", "container-loopback target")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("ssh-relay accepts no positional arguments")
		}
		bootstrap, err := LoadBootstrap(defaultBootstrapPath)
		if err != nil {
			return err
		}
		return RunSSHRelay(ctx, *socket, *target, bootstrap.OwnerUID, bootstrap.OwnerGID)
	case "setup":
		flags := flag.NewFlagSet("setup", flag.ContinueOnError)
		flags.SetOutput(stderr)
		timeout := flags.Duration("timeout", 0, "setup timeout")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		argv := flags.Args()
		if len(argv) != 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		result, err := RunSetup(ctx, *timeout, argv)
		if encodeErr := json.NewEncoder(stdout).Encode(result); encodeErr != nil {
			return encodeErr
		}
		return err
	case "git-credential":
		flags := flag.NewFlagSet("git-credential", flag.ContinueOnError)
		flags.SetOutput(stderr)
		socket := flags.String("socket", "/run/cohotfs/host/git.sock", "credential broker socket")
		workspaceID := flags.String("workspace", "", "workspace ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *workspaceID == "" {
			return fmt.Errorf("git-credential requires --workspace and get")
		}
		return RunGitCredential(ctx, flags.Arg(0), *socket, *workspaceID, os.Stdin, stdout)
	case "cdp-proxy":
		flags := flag.NewFlagSet("cdp-proxy", flag.ContinueOnError)
		flags.SetOutput(stderr)
		listen := flags.String("listen", "127.0.0.1:9222", "container-loopback listen address")
		upstream := flags.String("upstream-unix", "/run/cohotfs/host/cdp.sock", "host CDP Unix socket")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("cdp-proxy accepts no positional arguments")
		}
		return RunCDPProxy(ctx, *listen, *upstream)
	case "agent-run":
		flags := flag.NewFlagSet("agent-run", flag.ContinueOnError)
		flags.SetOutput(stderr)
		socket := flags.String("socket", defaultSecretSocket, "agent secret broker socket")
		workspaceID := flags.String("workspace", "", "workspace ID")
		var leaseIDs []string
		flags.Func("lease", "one-use secret lease ID; repeatable", func(value string) error {
			if value == "" {
				return fmt.Errorf("lease ID is empty")
			}
			leaseIDs = append(leaseIDs, value)
			return nil
		})
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		argv := flags.Args()
		if len(argv) != 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		return RunAgent(ctx, *socket, *workspaceID, leaseIDs, argv, os.Stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown cohotfs-agent mode %q", args[0])
	}
}
