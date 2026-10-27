package freebsd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sargunv/devices-cli/internal/process"
	"github.com/sargunv/devices-cli/internal/state"
)

// The fake anyvm leaves a QEMU stand-in that names the data directory, as
// QEMU's firmware variables file does, and exits.
const fakeAnyVM = `#!/bin/sh
echo "$@"
while [ $# -gt 0 ]; do
  case "$1" in
    --data-dir) dir=$2; shift ;;
  esac
  shift
done
# Like anyvm's subprocesses, QEMU does not inherit the launch lock.
sh -c 'trap "exit 0" TERM; while :; do sleep 0.1; done' fake-qemu "$dir/freebsd-VARS.fd" 3>&- &
echo "The VM is still running in background."
`

// The fake ssh answers every host, reports FAKE_MOUNT as the guest's one
// mount point, and stops the QEMU stand-in on shutdown.
const fakeSSH = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/ssh-calls"
case "$*" in
  *"stat -f %d '$FAKE_MOUNT' "*) printf '2\n1\n' ;;
  *"stat -f %d "*) printf '1\n1\n' ;;
  *"shutdown -p now") pkill -f fake-qemu ;;
esac
`

func writeScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestVMLifecycle(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, filepath.Join(bin, "anyvm"), fakeAnyVM)
	writeScript(t, filepath.Join(bin, "ssh"), fakeSSH)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	vmDir := t.TempDir()
	req := &Request{
		Release: "14.4", AnyVM: []string{filepath.Join(bin, "anyvm")},
		VMDir: vmDir, CacheDir: t.TempDir(), LogDir: t.TempDir(), BootTimeout: 10 * time.Second,
	}
	// A space in the owner's path survives the trip to the guest's shell.
	owner := filepath.Join(t.TempDir(), "my project")
	if err := os.Mkdir(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_MOUNT", owner)
	if err := req.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}

	inst, err := req.Start(t.Context(), owner, []state.Instance{{Kind: KindName, Port: firstPort}})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Port != firstPort+2 || inst.ID != "devices-cli-freebsd-"+strconv.Itoa(firstPort+2) {
		t.Errorf("started %s on port %d next to a VM on %d", inst.ID, inst.Port, firstPort)
	}
	if err := req.WaitReady(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	if !alive(inst) {
		t.Fatal("VM is not alive after anyvm exits")
	}
	log, err := os.ReadFile(inst.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"--release 14.4", "--sync nfs -v " + owner + ":" + owner, "--snapshot", "--ssh-name " + inst.ID, "--serial " + strconv.Itoa(inst.Port+1)} {
		if !strings.Contains(string(log), arg) {
			t.Errorf("anyvm arguments lack %q:\n%s", arg, log)
		}
	}

	// anyvm's SSH alias for the port goes with the VM.
	conf := filepath.Join(home, ".ssh", "config.d", strconv.Itoa(inst.Port)+".conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("\nHost "+strconv.Itoa(inst.Port)+" "+inst.ID+"\n  Port 60002\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (Kind{VMDir: vmDir}).Stop(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	if alive(inst) {
		t.Error("VM is alive after Stop")
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "ssh-calls"))
	if !strings.Contains(string(calls), inst.ID+" shutdown -p now") {
		t.Errorf("ssh calls:\n%s\nwant a shutdown", calls)
	}
	for _, path := range []string{conf, dataDir(vmDir, inst.Port)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s remains after Stop", path)
		}
	}
}

func TestRemoveSSHConfigKeepsOtherAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	conf := filepath.Join(home, ".ssh", "config.d", "60000.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("\nHost 60000 someone-else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeSSHConfig(state.Instance{ID: sshAlias(60000), Port: 60000}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conf); err != nil {
		t.Errorf("removed another VM's SSH alias: %v", err)
	}
}

func TestNewRequestRejectsBadReleases(t *testing.T) {
	for _, release := range []string{"", "14", "14.4/..", "latest"} {
		if _, err := NewRequest(release); err == nil || !strings.Contains(err.Error(), "--release") {
			t.Errorf("NewRequest(%q) = %v, want a --release error", release, err)
		}
	}
}

func TestWaitReadyRequiresTheMount(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, filepath.Join(bin, "anyvm"), fakeAnyVM)
	writeScript(t, filepath.Join(bin, "ssh"), fakeSSH)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_MOUNT", "/somewhere/else")
	vmDir := t.TempDir()
	req := &Request{
		Release: "14.4", AnyVM: []string{filepath.Join(bin, "anyvm")},
		VMDir: vmDir, CacheDir: t.TempDir(), LogDir: t.TempDir(), BootTimeout: 10 * time.Second,
	}
	inst, err := req.Start(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = (Kind{VMDir: vmDir}).Stop(t.Context(), inst) }()

	if err := req.WaitReady(t.Context(), inst); err == nil || !strings.Contains(err.Error(), "did not mount") {
		t.Errorf("got %v, want an error that the owner is not mounted", err)
	}
}

func TestLaunchLockLastsUntilAnyVMExits(t *testing.T) {
	bin := t.TempDir()
	// anyvm takes a moment, then leaves only the QEMU stand-in.
	writeScript(t, filepath.Join(bin, "anyvm"), "#!/bin/sh\nsleep 1\n"+fakeAnyVM[len("#!/bin/sh\n"):])
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	vmDir := t.TempDir()
	newReq := func() *Request {
		return &Request{
			Release: "14.4", AnyVM: []string{filepath.Join(bin, "anyvm")},
			VMDir: vmDir, CacheDir: t.TempDir(), LogDir: t.TempDir(), BootTimeout: 10 * time.Second,
		}
	}
	first := newReq()
	if err := first.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	inst, err := first.Start(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.StopGroup(inst.PID, 5*time.Second) }()

	start := time.Now()
	if err := newReq().Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Errorf("a second launch took the lock after %s, while anyvm still ran", waited)
	}
}
