//go:build integration

// Package integration boots real devices through the devices-cli binary. Run
// it with `mise run integration`; it needs ANDROID_HOME and, on Linux, KVM.
package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type env struct {
	t       *testing.T
	bin     string
	vars    []string
	adbPath string
}

func setup(t *testing.T, config string) *env {
	t.Helper()
	sdk := os.Getenv("ANDROID_HOME")
	if sdk == "" {
		t.Fatal("ANDROID_HOME must name an Android SDK")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "devices-cli")
	if out, err := exec.Command("go", "build", "-o", bin, "..").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	configHome := filepath.Join(dir, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "devices-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "devices-cli", "config.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &env{
		t:   t,
		bin: bin,
		vars: append(os.Environ(),
			"XDG_STATE_HOME="+filepath.Join(dir, "state"),
			"XDG_CONFIG_HOME="+configHome,
		),
		adbPath: filepath.Join(sdk, "platform-tools", "adb"),
	}
	t.Cleanup(func() {
		st := e.ls()
		for _, in := range st.Instances {
			_, _, _ = e.run(e.t.TempDir(), "release", "--owner", in.Owner)
		}
	})
	return e
}

// owner returns a new git worktree.
func (e *env) owner() string {
	dir := e.t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		e.t.Fatalf("git init: %v\n%s", err, out)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return resolved
}

// run runs devices-cli in dir and returns its stdout, stderr, and error.
func (e *env) run(dir string, args ...string) (string, string, error) {
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = dir
	cmd.Env = e.vars
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	e.t.Logf("%s in %s:\n%s", strings.Join(args, " "), dir, stderr.String())
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

func (e *env) get(dir string, args ...string) string {
	id, _, err := e.run(dir, append([]string{"get", "android", "--api", api()}, args...)...)
	if err != nil {
		e.t.Fatalf("get in %s: %v", dir, err)
	}
	return id
}

func (e *env) adb(args ...string) string {
	cmd := exec.Command(e.adbPath, args...)
	cmd.Env = append(os.Environ(), "ADB_MDNS=0")
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

type instance struct {
	Owner string `json:"owner"`
	ID    string `json:"id"`
	PID   int    `json:"pid"`
}

type listing struct {
	Instances []instance `json:"instances"`
}

func (e *env) ls() listing {
	out, _, err := e.run(e.t.TempDir(), "ls", "--json")
	if err != nil {
		e.t.Fatal(err)
	}
	var l listing
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

func api() string {
	if v := os.Getenv("DEVICES_CLI_TEST_API"); v != "" {
		return v
	}
	return "36"
}

func TestAndroidEmulators(t *testing.T) {
	e := setup(t, "max_instances = 2\n")
	a, b := e.owner(), e.owner()

	// Two gets for one owner and one for another, all at once.
	ids := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, call := range []struct{ name, dir string }{{"a1", a}, {"a2", a}, {"b", b}} {
		wg.Go(func() {
			id := e.get(call.dir)
			mu.Lock()
			ids[call.name] = id
			mu.Unlock()
		})
	}
	wg.Wait()
	if ids["a1"] != ids["a2"] || ids["a1"] == ids["b"] {
		t.Fatalf("got %v; want one instance for a and another for b", ids)
	}
	serialA, serialB := ids["a1"], ids["b"]

	// Each owner's instance has its own data.
	e.adb("-s", serialA, "shell", "echo owner-a > /data/local/tmp/marker")
	if got := e.adb("-s", serialA, "shell", "cat /data/local/tmp/marker"); got != "owner-a" {
		t.Errorf("a reads %q from its own marker", got)
	}
	if got := e.adb("-s", serialB, "shell", "cat /data/local/tmp/marker"); strings.Contains(got, "owner-a") {
		t.Error("b sees a's marker")
	}

	// A running instance comes back at once.
	start := time.Now()
	if id := e.get(a); id != serialA {
		t.Errorf("a got %s, then %s", serialA, id)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("reusing an instance took %s", elapsed)
	}

	// Both slots are taken.
	if _, stderr, err := e.run(e.owner(), "get", "android", "--api", api(), "--wait", "0"); err == nil || !strings.Contains(stderr, "max_instances") {
		t.Errorf("a third owner got a device past the cap: %v", err)
	}

	// An instance whose owner is gone stops at the next command.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if l := e.ls(); len(l.Instances) != 1 || l.Instances[0].ID != serialA {
		t.Errorf("after removing b: %+v; want only a's instance", l.Instances)
	}
	if got := e.adb("-s", serialB, "get-state"); got == "device" {
		t.Errorf("b's emulator %s still runs", serialB)
	}

	pid := e.ls().Instances[0].PID
	if _, _, err := e.run(a, "release"); err != nil {
		t.Fatal(err)
	}
	if l := e.ls(); len(l.Instances) != 0 {
		t.Errorf("after release: %+v", l.Instances)
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err == nil {
		t.Errorf("a's emulator process %d still runs", pid)
	}
}
