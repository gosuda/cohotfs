//go:build linux

package ompimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
)

const testWorkspaceID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCompileBuildsWritableReflinkMountsForSelectedOMPState(t *testing.T) {
	root := openTestRoot(t)
	defer root.Close()
	sources := ompFixture(t)

	spec := enabledImportSpec()
	spec.Import.RequireCOW = false
	plan, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mounts) != 3 {
		t.Fatalf("mounts = %#v", plan.Mounts)
	}
	homeOMP := filepath.Join(root.Path(), "workspaces", testWorkspaceID, "home", ".omp")
	if info, err := os.Stat(homeOMP); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("workspace OMP parent = %#v, %v", info, err)
	}
	binaryMount := requiredMount(t, plan, containerBinaryDir)
	nativeMount := requiredMount(t, plan, containerNative)
	agentMount := requiredMount(t, plan, containerAgent)
	for _, mount := range []runtime.Mount{binaryMount, nativeMount, agentMount} {
		if mount.ReadOnly || !strings.HasPrefix(mount.Source, root.Path()+string(filepath.Separator)) {
			t.Fatalf("OMP mount is not a writable workspace-owned path: %#v", mount)
		}
	}
	for _, relative := range []string{"config.yml", "models.yml"} {
		if _, err := os.Stat(filepath.Join(agentMount.Source, relative)); err != nil {
			t.Fatalf("selected OMP state %s is unavailable: %v", relative, err)
		}
	}
	for _, excluded := range []string{"agent.db", "agent.db-wal", "agent.db-shm", "agent.db-journal", "history.db"} {
		if _, err := os.Stat(filepath.Join(agentMount.Source, excluded)); !os.IsNotExist(err) {
			t.Fatalf("secret or unselected OMP state %s was imported: %v", excluded, err)
		}
	}
	original, err := os.Stat(sources.Binary)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := os.Stat(filepath.Join(binaryMount.Source, "omp"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(original, cloned) {
		t.Fatal("OMP binary clone reuses the host inode")
	}
	if !contains(plan.Environment, "PI_CODING_AGENT_DIR="+containerAgent) || !contains(plan.Environment, "PATH="+containerBinaryDir+":/usr/local/bin:/usr/bin:/bin") {
		t.Fatalf("OMP environment = %#v", plan.Environment)
	}
}

func TestCompileClonesCompleteAgentDirectoryWhenOAuthEnabled(t *testing.T) {
	root := openTestRoot(t)
	defer root.Close()
	sources := ompFixture(t)
	nested := filepath.Join(sources.Agent, "sessions", "current")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "session.json"), []byte("session fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := config.OMPAgentSpec{
		Enabled: true,
		Import: config.OMPImportSpec{
			Enabled: true, OAuthDB: true, RequireCOW: false,
		},
	}

	plan, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	agent := requiredMount(t, plan, containerAgent).Source
	for _, name := range []string{
		"agent.db", "agent.db-wal", "agent.db-shm", "agent.db-journal",
		"config.yml", "models.yml", "history.db", filepath.Join("sessions", "current", "session.json"),
	} {
		sourcePath := filepath.Join(sources.Agent, name)
		snapshotPath := filepath.Join(agent, name)
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := os.ReadFile(snapshotPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(snapshot) != string(source) {
			t.Fatalf("OMP agent file %s changed during clone", name)
		}
		sourceInfo, err := os.Stat(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		snapshotInfo, err := os.Stat(snapshotPath)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(sourceInfo, snapshotInfo) {
			t.Fatalf("OMP agent file %s reuses the host inode", name)
		}
	}
	if err := os.WriteFile(filepath.Join(agent, "agent.db"), []byte("workspace-update"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostDatabase, err := os.ReadFile(filepath.Join(sources.Agent, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(hostDatabase) != "sqlite fixture" {
		t.Fatalf("host OAuth database changed: %q", hostDatabase)
	}
}

func TestWritableOMPSnapshotsKeepHostFilesUnchanged(t *testing.T) {
	root := openTestRoot(t)
	defer root.Close()
	sources := ompFixture(t)
	spec := enabledImportSpec()
	spec.Import.RequireCOW = false
	plan, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(requiredMount(t, plan, containerBinaryDir).Source, "omp")
	agent := requiredMount(t, plan, containerAgent).Source
	for path, replacement := range map[string]string{
		binary:                                "changed binary",
		filepath.Join(agent, "config.yml"):    "model: workspace\n",
		filepath.Join(agent, "models.yml"):    "models: workspace\n",
		filepath.Join(agent, "workspace.tmp"): "workspace state",
	} {
		if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, expected := range map[string]string{
		sources.Binary: "binary",
		filepath.Join(sources.Agent, "config.yml"): "model: fixture\n",
		filepath.Join(sources.Agent, "models.yml"): "models: {}\n",
		filepath.Join(sources.Agent, "agent.db"):   "sqlite fixture",
	} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != expected {
			t.Fatalf("host file %s = %q, %v", path, raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sources.Agent, "workspace.tmp")); !os.IsNotExist(err) {
		t.Fatalf("workspace state reached host state: %v", err)
	}
}

func TestCompileRejectsUnsafeSelectedOMPFile(t *testing.T) {
	root := openTestRoot(t)
	defer root.Close()
	sources := ompFixture(t)
	outside := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(outside, []byte("unsafe: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sources.Agent, "config.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sources.Agent, "config.yml")); err != nil {
		t.Fatal(err)
	}
	spec := enabledImportSpec()
	spec.Import.RequireCOW = false
	if _, err := Compile(root, testWorkspaceID, spec, sources); err == nil {
		t.Fatal("accepted symlinked OMP configuration")
	}
}

func TestCompileChangesSnapshotWhenHostStateChanges(t *testing.T) {
	root := openTestRoot(t)
	defer root.Close()
	sources := ompFixture(t)
	spec := enabledImportSpec()
	spec.Import.RequireCOW = false
	first, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(sources.Agent, "config.yml")
	if err := os.WriteFile(configPath, []byte("model: updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(configPath, future, future); err != nil {
		t.Fatal(err)
	}
	second, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	if requiredMount(t, first, containerAgent).Source == requiredMount(t, second, containerAgent).Source {
		t.Fatal("changed OMP state reused the previous COW snapshot")
	}
}

func TestCompileInvalidatesFullAgentSnapshotWhenContentChangesWithoutMetadata(t *testing.T) {
	root := openTestRoot(t)
	defer root.Close()
	sources := ompFixture(t)
	spec := config.OMPAgentSpec{
		Enabled: true,
		Import:  config.OMPImportSpec{Enabled: true, OAuthDB: true},
	}
	first, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(sources.Agent, "history.db")
	info, err := os.Stat(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	changed[0] ^= 0xff
	if err := os.WriteFile(historyPath, changed, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(historyPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changedInfo, err := os.Stat(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	if changedInfo.Size() != info.Size() || !changedInfo.ModTime().Equal(info.ModTime()) {
		t.Fatalf("test mutation changed metadata: before=%#v after=%#v", info, changedInfo)
	}
	second, err := Compile(root, testWorkspaceID, spec, sources)
	if err != nil {
		t.Fatal(err)
	}
	firstAgent := requiredMount(t, first, containerAgent).Source
	secondAgent := requiredMount(t, second, containerAgent).Source
	if firstAgent == secondAgent {
		t.Fatal("content change with stable size and mtime reused the previous full agent snapshot")
	}
	snapshot, err := os.ReadFile(filepath.Join(secondAgent, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot) != string(changed) {
		t.Fatal("new full agent snapshot does not contain changed content")
	}
}

func TestDiscoverUsesExplicitAgentDirectoryAndPrivateNatives(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "omp")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	natives := filepath.Join(home, ".omp", "natives")
	agent := filepath.Join(home, "custom-agent")
	for _, directory := range []string{natives, agent} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	sources, err := Discover([]string{"HOME=" + home, "PI_CODING_AGENT_DIR=" + agent})
	if err != nil {
		t.Fatal(err)
	}
	if sources.Binary != executable || sources.Natives != natives || sources.Agent != agent {
		t.Fatalf("discovered sources = %#v", sources)
	}
}

func TestDiscoverResolvesCustomConfigRootProfileBeforeAgentOverride(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	configRoot := filepath.Join(t.TempDir(), "omp-config")
	profileAgent := filepath.Join(configRoot, "profiles", "work", "agent")
	natives := filepath.Join(configRoot, "natives")
	decoy := filepath.Join(home, "ignored-agent")
	for _, directory := range []string{bin, profileAgent, natives, decoy} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(bin, "omp")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	sources, err := Discover([]string{
		"HOME=" + home,
		"PI_CONFIG_DIR=" + configRoot,
		"OMP_PROFILE=work",
		"PI_CODING_AGENT_DIR=" + decoy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sources.Binary != executable || sources.Natives != natives || sources.Agent != profileAgent {
		t.Fatalf("profile sources = %#v", sources)
	}
}

func enabledImportSpec() config.OMPAgentSpec {
	return config.OMPAgentSpec{
		Enabled: true,
		Config:  "seed",
		Import: config.OMPImportSpec{
			Enabled: true, Binary: true, Natives: true, Models: true, Config: true, RequireCOW: true,
		},
	}
}

func ompFixture(t *testing.T) Sources {
	t.Helper()
	binaryDirectory := filepath.Join(t.TempDir(), "bin")
	natives := t.TempDir()
	agent := t.TempDir()
	if err := os.Mkdir(binaryDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binaryDirectory, "omp")
	if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"config.yml":       "model: fixture\n",
		"models.yml":       "models: {}\n",
		"agent.db":         "sqlite fixture",
		"agent.db-wal":     "private WAL",
		"agent.db-shm":     "private SHM",
		"agent.db-journal": "private journal",
		"history.db":       "private history",
	} {
		if err := os.WriteFile(filepath.Join(agent, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(natives, "module.node"), []byte("native"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Sources{Binary: binary, Natives: natives, Agent: agent}
}

func openTestRoot(t *testing.T) *hostroot.Root {
	t.Helper()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func requiredMount(t *testing.T, plan Plan, target string) runtime.Mount {
	t.Helper()
	for _, mount := range plan.Mounts {
		if mount.Target == target {
			return mount
		}
	}
	t.Fatalf("missing mount target %s in %#v", target, plan.Mounts)
	return runtime.Mount{}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
