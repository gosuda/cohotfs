//go:build linux

package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
)

func TestValidateSetupContract(t *testing.T) {
	source := t.TempDir()
	script := filepath.Join(source, ".cohotfs", "setup.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := config.SetupSpec{Mode: "once", Command: []string{"/bin/sh", ".cohotfs/setup.sh"}, Timeout: time.Minute}
	validation, err := Validate(source, spec, "sha256:image", "v1alpha1", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Digest == "" || validation.User != "1000:1000" || validation.Script != script {
		t.Fatalf("validation = %#v", validation)
	}
	if err := os.Chmod(script, 0o757); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(source, spec, "sha256:image", "v1alpha1", 1000, 1000); err == nil {
		t.Fatal("accepted other-writable setup script")
	}
}

func TestValidateRejectsEscapingScript(t *testing.T) {
	source := t.TempDir()
	outside := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(outside, []byte("echo unsafe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "setup.sh")); err != nil {
		t.Fatal(err)
	}
	spec := config.SetupSpec{Mode: "once", Command: []string{"/bin/sh", "setup.sh"}, Timeout: time.Minute}
	if _, err := Validate(source, spec, "image", "v1alpha1", 1000, 1000); err == nil {
		t.Fatal("accepted escaping setup script")
	}
}

func TestShouldRunModes(t *testing.T) {
	success := state.SetupResult{Succeeded: true}
	failure := state.SetupResult{Succeeded: false}
	cases := []struct {
		mode                  string
		previous              state.SetupResult
		explicit, force, want bool
	}{
		{"once", success, false, false, false}, {"once", failure, false, false, true},
		{"once", success, true, true, true}, {"always", success, false, false, true},
		{"manual", failure, false, false, false}, {"manual", failure, true, false, true},
	}
	for _, test := range cases {
		got, err := ShouldRun(test.mode, test.previous, test.explicit, test.force)
		if err != nil || got != test.want {
			t.Fatalf("ShouldRun(%+v) = %v, %v", test, got, err)
		}
	}
}

type setupBackend struct {
	result   runtime.ExecResult
	err      error
	requests int
}

func (f *setupBackend) Probe(context.Context) (runtime.BackendInfo, error) {
	return runtime.BackendInfo{}, nil
}
func (f *setupBackend) Pull(context.Context, runtime.PullRequest) (runtime.ResolvedImage, error) {
	return runtime.ResolvedImage{}, nil
}
func (f *setupBackend) Create(context.Context, runtime.WorkspaceSpec) (runtime.WorkspaceRef, error) {
	return runtime.WorkspaceRef{}, nil
}
func (f *setupBackend) Start(context.Context, runtime.WorkspaceRef) error { return nil }
func (f *setupBackend) ExecSync(context.Context, runtime.WorkspaceRef, runtime.ExecRequest) (runtime.ExecResult, error) {
	f.requests++
	return f.result, f.err
}
func (f *setupBackend) Inspect(context.Context, runtime.WorkspaceRef) (runtime.WorkspaceStatus, error) {
	return runtime.WorkspaceStatus{}, nil
}
func (f *setupBackend) Stop(context.Context, runtime.WorkspaceRef, time.Duration) error { return nil }
func (f *setupBackend) Delete(context.Context, runtime.WorkspaceRef) error              { return nil }

func TestSetupFailureAndOnceSuccess(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, _ := state.NewStore(root)
	id, _ := state.NewWorkspaceID()
	if err := store.SaveWorkspace(state.Workspace{ID: id, Status: state.StatusStopped, RuntimeRef: runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": "container"}, Nonce: "nonce"}}); err != nil {
		t.Fatal(err)
	}
	backend := &setupBackend{result: runtime.ExecResult{ExitCode: 1, Output: []byte("failed")}}
	service := NewService(store, backend)
	spec := config.SetupSpec{Mode: "once", Command: []string{"/bin/sh", ".cohotfs/setup.sh"}, Timeout: time.Minute}
	validation := Validation{Digest: "digest", User: "1000:1000"}
	record, err := service.Run(context.Background(), id, spec, validation, false, false)
	if err == nil || record.Status != state.StatusSetupFailed || record.Setup.Output != "failed" {
		t.Fatalf("failure record=%#v err=%v", record, err)
	}
	backend.result = runtime.ExecResult{ExitCode: 0, Output: []byte("ok")}
	record, err = service.Run(context.Background(), id, spec, validation, true, false)
	if err != nil || record.Status != state.StatusReady || !record.Setup.Succeeded {
		t.Fatalf("retry record=%#v err=%v", record, err)
	}
	record, err = service.Run(context.Background(), id, spec, validation, false, false)
	if err != nil || backend.requests != 2 {
		t.Fatalf("once reran: requests=%d record=%#v err=%v", backend.requests, record, err)
	}
	record, err = service.Run(context.Background(), id, spec, validation, true, true)
	if err != nil || record.Status != state.StatusReady || backend.requests != 3 {
		t.Fatalf("forced ready rerun: requests=%d record=%#v err=%v", backend.requests, record, err)
	}
}
