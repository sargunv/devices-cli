package wine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sargunv/devices-cli/internal/remote"
)

// The fake wine records its calls, makes the prefix's system32 directory on
// wineboot --init, keeps what regedit imports, and runs a "program" by
// printing its environment and exiting with the status it names.
const fakeWine = `#!/bin/sh
echo "wine $*" >> "$WINEPREFIX/../calls"
case "$1" in
  wineboot) if [ "$2" = --init ]; then mkdir -p "$WINEPREFIX/drive_c/windows/system32"; fi ;;
  regedit) cp "$WINEPREFIX/drive_c/devices-cli.reg" "$WINEPREFIX/../imported.reg" ;;
  *) echo "WINEPREFIX=$WINEPREFIX WINEDEBUG=$WINEDEBUG FOO=$FOO dir=$(pwd)"; exit "$2" ;;
esac
`

// The fake wineserver runs "persistently" until killed, exiting 2 when it
// already runs, as wineserver does.
const fakeWineserver = `#!/bin/sh
echo "wineserver $*" >> "$WINEPREFIX/../calls"
case "$1" in
  --persistent) [ -e "$WINEPREFIX/../server" ] && exit 2; touch "$WINEPREFIX/../server" ;;
  --kill) rm -f "$WINEPREFIX/../server" ;;
esac
`

func fakeTools(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for name, content := range map[string]string{"wine": fakeWine, "wineserver": fakeWineserver} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewRequestExpandsDirectories(t *testing.T) {
	fakeTools(t)
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "vkd3d", "d3d12.dll"))
	touch(t, filepath.Join(dir, "vkd3d", "d3d12core.dll"))
	touch(t, filepath.Join(dir, "vkd3d", "README"))
	touch(t, filepath.Join(dir, "libunwind.DLL"))

	req, err := NewRequest([]string{filepath.Join(dir, "vkd3d"), filepath.Join(dir, "libunwind.DLL"), filepath.Join(dir, "vkd3d", "d3d12.dll")})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "libunwind.DLL"),
		filepath.Join(dir, "vkd3d", "d3d12.dll"),
		filepath.Join(dir, "vkd3d", "d3d12core.dll"),
	}
	if strings.Join(req.DLLs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("DLLs = %q, want %q", req.DLLs, want)
	}

	other, err := NewRequest([]string{filepath.Join(dir, "vkd3d")})
	if err != nil {
		t.Fatal(err)
	}
	if req.Key() == other.Key() {
		t.Fatalf("requests for different DLLs share the key %q", req.Key())
	}

	// A DLL rebuilt in place changes the key.
	if err := os.WriteFile(filepath.Join(dir, "vkd3d", "d3d12.dll"), []byte("rebuilt"), 0o644); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := NewRequest([]string{filepath.Join(dir, "vkd3d")})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Key() == other.Key() {
		t.Fatalf("a rebuilt DLL keeps the key %q", other.Key())
	}
}

func TestNewRequestRejects(t *testing.T) {
	fakeTools(t)
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "a", "d3d12.dll"))
	touch(t, filepath.Join(dir, "b", "D3D12.dll"))
	touch(t, filepath.Join(dir, "b", "dxgi.dll"))
	touch(t, filepath.Join(dir, "c", "d3d12.dll"))
	touch(t, filepath.Join(dir, "empty", "README"))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}, "two DLLs called"},
		{[]string{filepath.Join(dir, "a"), filepath.Join(dir, "b", "dxgi.dll"), filepath.Join(dir, "c")}, "two DLLs called"},
		{[]string{filepath.Join(dir, "empty")}, "holds no .dll files"},
		{[]string{filepath.Join(dir, "empty", "README")}, "is not a .dll file"},
		{[]string{filepath.Join(dir, "missing.dll")}, "no such file"},
	} {
		if _, err := NewRequest(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("NewRequest(%q) = %v, want an error containing %q", tc.args, err, tc.want)
		}
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := NewRequest(nil); err == nil || !strings.Contains(err.Error(), "no wine on PATH") {
		t.Errorf("NewRequest without Wine = %v", err)
	}
}

func TestPrefixLifecycle(t *testing.T) {
	fakeTools(t)
	dlls := t.TempDir()
	touch(t, filepath.Join(dlls, "D3D12.dll"))
	touch(t, filepath.Join(dlls, "dxgi.dll"))
	req, err := NewRequest([]string{dlls})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	req.Root = filepath.Join(root, "wine")
	req.BootTimeout = 10 * time.Second
	ctx := context.Background()

	inst, err := req.Start(ctx, "/work/tree", nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(inst.ID) != req.Root || inst.Key != req.Key() {
		t.Fatalf("instance = %+v", inst)
	}
	if !(Kind{}).Alive(ctx, inst) {
		t.Fatal("a started prefix is not alive")
	}
	for range 2 {
		if err := req.WaitReady(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"d3d12.dll", "dxgi.dll"} {
		if _, err := os.Stat(filepath.Join(inst.ID, "drive_c", "windows", "system32", name)); err != nil {
			t.Errorf("system32 lacks %s: %v", name, err)
		}
	}
	reg, err := os.ReadFile(filepath.Join(req.Root, "imported.reg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`"d3d12"="native,builtin"`, `"dxgi"="native,builtin"`} {
		if !strings.Contains(string(reg), line+"\r\n") {
			t.Errorf("the registry import lacks %s:\n%s", line, reg)
		}
	}
	calls, err := os.ReadFile(filepath.Join(req.Root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	// The second get finds the prefix set up and its session running.
	want := "wine wineboot --init\nwine regedit /S C:\\devices-cli.reg\nwineserver --wait\n" +
		"wineserver --persistent\nwine wineboot\nwineserver --persistent\n"
	if string(calls) != want {
		t.Errorf("calls:\n%s\nwant:\n%s", calls, want)
	}

	var out bytes.Buffer
	t.Setenv("FOO", "bar")
	t.Setenv("WINEDEBUG", "")
	if err := os.Unsetenv("WINEDEBUG"); err != nil {
		t.Fatal(err)
	}
	// Resolved, as pwd prints it: macOS's temporary directory is a link.
	workDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, err := Exec(ctx, inst.ID, remote.Spec{Program: "/bin/app.exe", Args: []string{"3"}, Dir: workDir, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	if status != 3 {
		t.Errorf("status = %d, want 3", status)
	}
	if got, want := strings.TrimSpace(out.String()), "WINEPREFIX="+inst.ID+" WINEDEBUG=-all FOO=bar dir="+workDir; got != want {
		t.Errorf("program saw %q, want %q", got, want)
	}

	// Collection may run without Wine on PATH.
	t.Setenv("PATH", "/usr/bin:/bin")
	// Stop runs even after an interrupted get cancelled its context.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (Kind{}).Stop(cancelled, inst); err != nil {
		t.Fatal(err)
	}
	if (Kind{}).Alive(ctx, inst) {
		t.Error("a stopped prefix is still alive")
	}
	if _, err := os.Stat(filepath.Join(req.Root, "server")); err == nil {
		t.Error("Stop left the wineserver running")
	}
}
