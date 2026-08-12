//go:build linux

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/state"
)

func TestParsePortAcceptsOnlyTCPPortRange(t *testing.T) {
	for _, value := range []string{"", "0", "65536", "-1", "http", " 3000"} {
		if _, err := parsePort(value, "container port"); err == nil || apperr.Code(err) != apperr.ExitUsage {
			t.Fatalf("parsePort(%q) error = %v", value, err)
		}
	}
	for _, value := range []string{"1", "3000", "65535"} {
		if port, err := parsePort(value, "container port"); err != nil || port <= 0 {
			t.Fatalf("parsePort(%q) = %d, %v", value, port, err)
		}
	}
}

func TestParseBindHostAllowsOnlyRequestedExposureModes(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "0.0.0.0"} {
		if host, err := parseBindHost(value); err != nil || host != value {
			t.Fatalf("parseBindHost(%q) = %q, %v", value, host, err)
		}
	}
	for _, value := range []string{"", "localhost", "::", "192.0.2.1"} {
		if _, err := parseBindHost(value); err == nil || apperr.Code(err) != apperr.ExitUsage {
			t.Fatalf("parseBindHost(%q) error = %v", value, err)
		}
	}
}

func TestPortForwardArgumentsBindRequestedHostAndTargetLoopback(t *testing.T) {
	base := []string{"-F", "/dev/null"}
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		got := portForwardArguments(base, host, "workspace", 8080, 3000)
		want := []string{
			"-F", "/dev/null", "-T", "-N", "-o", "ExitOnForwardFailure=yes",
			"-L", host + ":8080:127.0.0.1:3000", "agent@cohotfs-workspace",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("port-forward %s arguments = %#v, want %#v", host, got, want)
		}
	}
	if !reflect.DeepEqual(base, []string{"-F", "/dev/null"}) {
		t.Fatalf("port-forward mutated base arguments: %#v", base)
	}
}

func TestPortForwardRejectsWorkspaceWithoutCurrentCapability(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	root, err := hostroot.OpenForTest(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []state.Workspace{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "old-api", BootstrapAPI: "v1alpha1", TCPForwarding: true, Status: state.StatusReady},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "no-capability", BootstrapAPI: containeragent.BootstrapAPI, Status: state.StatusReady},
	} {
		if err := store.SaveWorkspace(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	for _, workspace := range []string{"old-api", "no-capability"} {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"port-forward", "3000", "--workspace", workspace}, &stdout, &stderr, testDependencies(rootPath))
		if code != apperr.ExitStateConflict || !strings.Contains(stderr.String(), "remove and recreate") {
			t.Fatalf("port-forward %s code=%d stdout=%q stderr=%q", workspace, code, stdout.String(), stderr.String())
		}
	}
}
