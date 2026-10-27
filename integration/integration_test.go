//go:build integration

// Package integration boots real devices through the devices-cli binary. Run
// it with `mise run integration`. The Android test needs ANDROID_HOME and, on
// Linux, KVM; the Apple test runs on macOS with Xcode; the OpenHarmony test
// needs OHOS_EMULATOR_IMAGES and QEMU and hdc on PATH.
package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type env struct {
	t    *testing.T
	bin  string
	vars []string
}

func setup(t *testing.T, config string) *env {
	t.Helper()
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
	id, _, err := e.run(dir, append([]string{"get"}, args...)...)
	if err != nil {
		e.t.Fatalf("get in %s: %v", dir, err)
	}
	return id
}

// getConcurrently runs two gets for one owner and one for another at once and
// checks that the first owner got one device and the second another.
func (e *env) getConcurrently(a, b string, args ...string) (string, string) {
	ids := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, call := range []struct{ name, dir string }{{"a1", a}, {"a2", a}, {"b", b}} {
		// t.Fatal must not run in another goroutine, so failures wait for all.
		wg.Go(func() {
			id, _, err := e.run(call.dir, append([]string{"get"}, args...)...)
			if err != nil {
				e.t.Errorf("get in %s: %v", call.dir, err)
			}
			mu.Lock()
			ids[call.name] = id
			mu.Unlock()
		})
	}
	wg.Wait()
	if e.t.Failed() || ids["a1"] != ids["a2"] || ids["a1"] == ids["b"] {
		e.t.Fatalf("got %v; want one device for a and another for b", ids)
	}
	return ids["a1"], ids["b"]
}

// output runs a command and returns its trimmed combined output.
func output(name string, args ...string) string {
	cmd := exec.Command(name, args...)
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
	sdk := os.Getenv("ANDROID_HOME")
	if sdk == "" {
		t.Fatal("ANDROID_HOME must name an Android SDK")
	}
	adb := func(args ...string) string { return output(filepath.Join(sdk, "platform-tools", "adb"), args...) }
	e := setup(t, "max_instances = 2\n")
	a, b := e.owner(), e.owner()
	request := []string{"android", "--api", api()}

	serialA, serialB := e.getConcurrently(a, b, request...)

	// Each owner's instance has its own data.
	adb("-s", serialA, "shell", "echo owner-a > /data/local/tmp/marker")
	if got := adb("-s", serialA, "shell", "cat /data/local/tmp/marker"); got != "owner-a" {
		t.Errorf("a reads %q from its own marker", got)
	}
	if got := adb("-s", serialB, "shell", "cat /data/local/tmp/marker"); strings.Contains(got, "owner-a") {
		t.Error("b sees a's marker")
	}

	// A running instance comes back at once.
	start := time.Now()
	if id := e.get(a, request...); id != serialA {
		t.Errorf("a got %s, then %s", serialA, id)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("reusing an instance took %s", elapsed)
	}

	// Both slots are taken.
	if _, stderr, err := e.run(e.owner(), append([]string{"get", "--wait", "0"}, request...)...); err == nil || !strings.Contains(stderr, "max_instances") {
		t.Errorf("a third owner got a device past the cap: %v", err)
	}

	// An instance whose owner is gone stops at the next command.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if l := e.ls(); len(l.Instances) != 1 || l.Instances[0].ID != serialA {
		t.Errorf("after removing b: %+v; want only a's instance", l.Instances)
	}
	if got := adb("-s", serialB, "get-state"); got == "device" {
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

func TestAppleSimulators(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple simulators run only on macOS")
	}
	// Two owners' simulators run at once, past the default cap of a small CI
	// runner.
	e := setup(t, "max_instances = 2\n")
	a, b := e.owner(), e.owner()

	udidA, udidB := e.getConcurrently(a, b, "ios")

	// Each owner's simulator has its own data.
	output("xcrun", "simctl", "spawn", udidA, "defaults", "write", "dev.devices-cli.test", "marker", "owner-a")
	if got := output("xcrun", "simctl", "spawn", udidA, "defaults", "read", "dev.devices-cli.test", "marker"); got != "owner-a" {
		t.Errorf("a reads %q from its own marker", got)
	}
	if got := output("xcrun", "simctl", "spawn", udidB, "defaults", "read", "dev.devices-cli.test", "marker"); strings.Contains(got, "owner-a") {
		t.Error("b sees a's marker")
	}

	if id := e.get(a, "ios"); id != udidA {
		t.Errorf("a got %s, then %s", udidA, id)
	}

	// A simulator whose owner is gone is deleted at the next command, and a
	// released one is deleted too.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if l := e.ls(); len(l.Instances) != 1 || l.Instances[0].ID != udidA {
		t.Errorf("after removing b: %+v; want only a's simulator", l.Instances)
	}
	if _, _, err := e.run(a, "release"); err != nil {
		t.Fatal(err)
	}
	devices := output("xcrun", "simctl", "list", "devices")
	for _, udid := range []string{udidA, udidB} {
		if strings.Contains(devices, udid) {
			t.Errorf("simulator %s still exists", udid)
		}
	}
}

func TestOHOSEmulators(t *testing.T) {
	images := os.Getenv("OHOS_EMULATOR_IMAGES")
	if images == "" {
		t.Fatal("OHOS_EMULATOR_IMAGES must name the Oniro emulator image directory")
	}
	before, err := os.Stat(filepath.Join(images, "userdata.img"))
	if err != nil {
		t.Fatal(err)
	}
	hdc := func(key string, args ...string) string { return output("hdc", append([]string{"-t", key}, args...)...) }
	e := setup(t, "max_instances = 2\n")
	a, b := e.owner(), e.owner()

	keyA, keyB := e.getConcurrently(a, b, "ohos")

	// Each owner's instance has its own data, and none writes the image.
	hdc(keyA, "shell", "echo owner-a > /data/local/tmp/marker")
	if got := hdc(keyA, "shell", "cat /data/local/tmp/marker"); got != "owner-a" {
		t.Errorf("a reads %q from its own marker", got)
	}
	if got := hdc(keyB, "shell", "cat /data/local/tmp/marker"); strings.Contains(got, "owner-a") {
		t.Error("b sees a's marker")
	}

	if id := e.get(a, "ohos"); id != keyA {
		t.Errorf("a got %s, then %s", keyA, id)
	}

	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if l := e.ls(); len(l.Instances) != 1 || l.Instances[0].ID != keyA {
		t.Errorf("after removing b: %+v; want only a's instance", l.Instances)
	}
	pid := e.ls().Instances[0].PID
	if _, _, err := e.run(a, "release"); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err == nil {
		t.Errorf("a's QEMU process %d still runs", pid)
	}
	after, err := os.Stat(filepath.Join(images, "userdata.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an instance wrote the shared userdata image")
	}
}
