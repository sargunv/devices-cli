//go:build integration

// Package integration boots real devices through the devices-cli binary. Run
// it with `mise run integration`. The Android test needs ANDROID_HOME and, on
// Linux, KVM; the Apple test runs on macOS with Xcode; the OpenHarmony test
// needs OHOS_EMULATOR_IMAGES and QEMU and hdc on PATH; the FreeBSD test runs
// on Linux with KVM and needs ANYVM, QEMU, and UEFI firmware.
package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	// xdgState is the XDG_STATE_HOME that devices-cli runs with.
	xdgState string
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
		t:        t,
		bin:      bin,
		xdgState: filepath.Join(dir, "state"),
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

// stateDir is devices-cli's state directory.
func (e *env) stateDir() string { return filepath.Join(e.xdgState, "devices-cli") }

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

	e.checkExec(a, true, true, shellProgram("/system/bin/sh"), request...)

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
	e.checkExec(a, false, false, e.simulatorProgram, "ios")

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
	e.checkExec(a, true, true, shellProgram("/system/bin/sh"), "ohos")

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

func TestHarmonyOSEmulators(t *testing.T) {
	emulator := os.Getenv("HARMONYOS_EMULATOR")
	if emulator == "" {
		emulator = "/Applications/DevEco-Studio.app/Contents/tools/emulator/Emulator"
	}
	if _, err := os.Stat(emulator); err != nil {
		t.Skip("HarmonyOS emulators need DevEco Studio's Emulator")
	}
	hdcPath := "/Applications/DevEco-Studio.app/Contents/sdk/default/openharmony/toolchains/hdc"
	hdc := func(key string, args ...string) string {
		return output(hdcPath, append([]string{"-t", key}, args...)...)
	}
	e := setup(t, "max_instances = 2\n")
	a, b := e.owner(), e.owner()

	keyA, keyB := e.getConcurrently(a, b, "harmonyos")

	// Each owner's instance has its own data.
	hdc(keyA, "shell", "echo owner-a > /data/local/tmp/marker")
	if got := hdc(keyA, "shell", "cat /data/local/tmp/marker"); got != "owner-a" {
		t.Errorf("a reads %q from its own marker", got)
	}
	if got := hdc(keyB, "shell", "cat /data/local/tmp/marker"); strings.Contains(got, "owner-a") {
		t.Error("b sees a's marker")
	}

	if id := e.get(a, "harmonyos"); id != keyA {
		t.Errorf("a got %s, then %s", keyA, id)
	}
	e.checkExec(a, true, true, shellProgram("/system/bin/sh"), "harmonyos")

	// Removing b's owner stops and deletes its instance at the next command;
	// release does a's.
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
		t.Errorf("a's emulator process %d still runs", pid)
	}
	instances, _ := filepath.Glob(filepath.Join(e.stateDir(), "harmonyos", "devices_cli_*"))
	if len(instances) != 0 {
		t.Errorf("instances left behind: %v", instances)
	}
}

func TestFreeBSDVMs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FreeBSD VMs run only on Linux")
	}
	e := setup(t, "max_instances = 2\n")
	a, b := e.owner(), e.owner()
	request := []string{"freebsd", "--release", "14.4"}
	ssh := func(host string, args ...string) string {
		// anyvm's SSH configuration adds each VM's host key with a warning.
		return output("ssh", append([]string{"-o", "BatchMode=yes", "-o", "LogLevel=ERROR", host}, args...)...)
	}

	hostA, hostB := e.getConcurrently(a, b, request...)

	if got := ssh(hostA, "uname", "-s"); got != "FreeBSD" {
		t.Errorf("ssh %s uname -s = %q", hostA, got)
	}
	// The owner directory is mounted at its own path, and only in its VM.
	if err := os.WriteFile(filepath.Join(a, "marker"), []byte("owner-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ssh(hostA, "cat", filepath.Join(a, "marker")); got != "owner-a" {
		t.Errorf("a's VM reads %q from the owner directory", got)
	}
	if got := ssh(hostB, "cat", filepath.Join(a, "marker")); strings.Contains(got, "owner-a") {
		t.Error("b's VM sees a's owner directory")
	}

	if id := e.get(a, request...); id != hostA {
		t.Errorf("a got %s, then %s", hostA, id)
	}
	e.checkExec(a, false, true, shellProgram("/bin/sh"), request...)

	// Removing b's owner shuts its VM down at the next command; release shuts
	// a's down.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if l := e.ls(); len(l.Instances) != 1 || l.Instances[0].ID != hostA {
		t.Errorf("after removing b: %+v; want only a's VM", l.Instances)
	}
	pid := e.ls().Instances[0].PID
	if _, _, err := e.run(a, "release"); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("pgrep", "-g", strconv.Itoa(pid)).Run(); err == nil {
		t.Errorf("a's VM processes in group %d still run", pid)
	}
	for _, host := range []string{hostA, hostB} {
		if got := output("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "true"); !strings.Contains(got, "ssh:") {
			t.Errorf("%s still answers SSH: %q", host, got)
		}
	}
}

// checkExec runs a program through exec on the owner's device, with a synced
// fixture directory that holds a symbolic link, and checks its output, its
// exit status, and that the paths into the fixtures reach their copy. On a
// device that sees the host's paths, the copy is the directory itself. When
// keepsDir is set, the program runs in the working directory's copy.
// program writes the program into a file and returns its path.
func (e *env) checkExec(owner string, copied, keepsDir bool, program func(dir string) string, request ...string) {
	e.t.Helper()
	fixtures := filepath.Join(owner, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "shared.txt"), []byte("linked\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Symlink("../shared.txt", filepath.Join(fixtures, "link.txt")); err != nil {
		e.t.Fatal(err)
	}
	prog := program(owner)

	args := []string{"exec"}
	args = append(args, request...)
	args = append(args, "--sync-env", "FIXTURES", "--", prog, filepath.Join(fixtures, "hello.txt"))
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = fixtures
	cmd.Env = slices.Concat(e.vars, []string{"FIXTURES=" + fixtures})
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	e.t.Logf("exec in %s:\nstdout:\n%s\nstderr:\n%s", fixtures, stdout.String(), stderr.String())
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		e.t.Fatalf("exec = %v; want the program's exit status 3", err)
	}
	lines := map[string]string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			lines[k] = v
		}
	}
	if lines["hello"] != "hello" || lines["link"] != "linked" {
		e.t.Errorf("the program read %q and %q from the fixtures; want hello and linked", lines["hello"], lines["link"])
	}
	if !strings.Contains(stderr.String(), "to stderr") {
		e.t.Error("the program's stderr did not come through")
	}
	got := lines["fixtures"]
	if copied == (got == fixtures) || lines["arg"] != got+"/hello.txt" || keepsDir && lines["pwd"] != got {
		e.t.Errorf("FIXTURES=%q, arg=%q, pwd=%q; want the fixtures' copy (copied: %v, working directory kept: %v)", got, lines["arg"], lines["pwd"], copied, keepsDir)
	}
}

// shellProgram writes a shell script for exec's check, run by the shell at
// interpreter.
func shellProgram(interpreter string) func(dir string) string {
	return func(dir string) string {
		path := filepath.Join(dir, "program.sh")
		script := "#!" + interpreter + `
echo "hello=$(cat "$FIXTURES/hello.txt")"
echo "link=$(cat "$FIXTURES/link.txt")"
echo "fixtures=$FIXTURES"
echo "arg=$1"
echo "pwd=$(pwd -P)"
echo "to stderr" >&2
exit 3
`
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			panic(err)
		}
		return path
	}
}

// simulatorProgram compiles exec's check program for the iOS simulator, which
// runs only programs built for it.
func (e *env) simulatorProgram(dir string) string {
	source := filepath.Join(dir, "program.c")
	program := filepath.Join(dir, "program")
	code := `#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static void put(const char *key, const char *dir, const char *name) {
	char path[4096], line[256] = "";
	snprintf(path, sizeof path, "%s/%s", dir, name);
	FILE *f = fopen(path, "r");
	if (f) {
		if (fgets(line, sizeof line, f)) line[strcspn(line, "\n")] = 0;
		fclose(f);
	}
	printf("%s=%s\n", key, line);
}

int main(int argc, char **argv) {
	const char *fixtures = getenv("FIXTURES");
	char cwd[4096];
	put("hello", fixtures, "hello.txt");
	put("link", fixtures, "link.txt");
	printf("fixtures=%s\narg=%s\npwd=%s\n", fixtures, argc > 1 ? argv[1] : "", getcwd(cwd, sizeof cwd));
	fprintf(stderr, "to stderr\n");
	return 3;
}
`
	if err := os.WriteFile(source, []byte(code), 0o644); err != nil {
		e.t.Fatal(err)
	}
	arch := "arm64"
	if runtime.GOARCH == "amd64" {
		arch = "x86_64"
	}
	out, err := exec.Command("xcrun", "--sdk", "iphonesimulator", "clang", "-arch", arch, "-mios-simulator-version-min=17.0", "-o", program, source).CombinedOutput()
	if err != nil {
		e.t.Fatalf("compile the simulator program: %v\n%s", err, out)
	}
	return program
}
