package ohos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sargunv/devices-cli/internal/process"
)

// The fake QEMU runs until it is stopped.
const fakeQEMU = `#!/bin/sh
trap 'exit 0' TERM
while :; do sleep 0.1; done
`

// The fake hdc boots in stages as the Oniro image does: the shell answers
// first, then the boot completes, then user 100 activates, and the screen
// stays locked until a swipe. FAKE_BOOT_STALLS keeps the boot from completing.
// Every call goes to a log beside it.
const fakeHDC = `#!/bin/sh
dir=$(dirname "$0")
echo "$@" >> "$dir/hdc-calls"
count() { n=$(cat "$dir/$1" 2>/dev/null || echo 0); n=$((n + 1)); echo $n > "$dir/$1"; echo $n; }
case "$*" in
  "tconn "*) echo "Connect OK" ;;
  *"shell echo ready") echo ready ;;
  *"shell param get bootevent.boot.completed")
    if [ -n "$FAKE_BOOT_STALLS" ] || [ "$(count boot)" -lt 2 ]; then
      echo 'Get parameter "bootevent.boot.completed" fail! errNum is:106!'
    else
      echo 'true '
    fi ;;
  *"shell acm dump -i 100")
    if [ "$(count acm)" -lt 2 ]; then status=inactive; else status=active; fi
    printf 'ID: 100\n\tName: u**********\n\tStatus: %s\n\tCreate Completed: true\n' "$status" ;;
  *"shell hidumper -s ScreenlockService -a -all")
    if [ -e "$dir/swiped" ]; then locked=false; else locked=true; fi
    printf ' * screenState  \t\tfalse\t\tscreen on / off status\n * screenLocked  \t\t%s\t\t100\n' "$locked" ;;
  *"shell uinput -T -m "*) touch "$dir/swiped" ;;
  *) echo "unexpected hdc call: $*" >&2; exit 1 ;;
esac
`

func setupFakes(t *testing.T) (*Request, string) {
	t.Helper()
	bin := t.TempDir()
	for name, content := range map[string]string{"hdc": fakeHDC, "qemu-system-x86_64": fakeQEMU} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	interval := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = interval })
	req := &Request{
		Images: t.TempDir(), QEMU: filepath.Join(bin, "qemu-system-x86_64"), HDC: filepath.Join(bin, "hdc"),
		LogDir: t.TempDir(), BootTimeout: 10 * time.Second,
	}
	return req, bin
}

func TestWaitReadyWaitsUntilAnAppCanLaunch(t *testing.T) {
	req, bin := setupFakes(t)
	inst, err := req.Start(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.StopGroup(inst.PID, 5*time.Second) }()

	if err := req.WaitReady(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bin, "swiped")); err != nil {
		t.Fatal("WaitReady returned without unlocking the screen")
	}
	calls, err := os.ReadFile(filepath.Join(bin, "hdc-calls"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	// The last check confirms the swipe unlocked the screen.
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, "shell hidumper -s ScreenlockService -a -all") {
		t.Errorf("last hdc call is %q, want a screen lock check", last)
	}
	swipe := "-t " + inst.ID + " shell uinput -T -m 180 648 180 72 300"
	if !strings.Contains(string(calls), swipe+"\n") {
		t.Errorf("hdc calls:\n%s\nwant %q", calls, swipe)
	}
	// Checks run in order, so the screen is unlocked only once the user is
	// active, which the fake reports from its second account dump.
	acm := 0
	for i, line := range lines {
		if strings.HasSuffix(line, "shell acm dump -i 100") {
			acm++
		}
		if strings.Contains(line, "shell uinput") && acm < 2 {
			t.Errorf("hdc call %d swipes before the user is active:\n%s", i, calls)
		}
	}
}

func TestWaitReadyNamesTheStageThatDidNotFinish(t *testing.T) {
	req, _ := setupFakes(t)
	t.Setenv("FAKE_BOOT_STALLS", "1")
	req.BootTimeout = 200 * time.Millisecond
	inst, err := req.Start(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.StopGroup(inst.PID, 5*time.Second) }()

	err = req.WaitReady(t.Context(), inst)
	if err == nil || !strings.Contains(err.Error(), "bootevent.boot.completed") || !strings.Contains(err.Error(), "--boot-timeout") {
		t.Errorf("got %v, want an error that names the boot event and the timeout flag", err)
	}
}

func TestAccountActive(t *testing.T) {
	for dump, want := range map[string]bool{
		"ID: 100\n\tName: u**********\n\tType: admin\n\tStatus: active\n\tisForeground: 1\n": true,
		"ID: 100\n\tStatus: inactive\n\tVerified: false\n":                                   false,
		"error: failed to dump state.":                                                       false,
	} {
		if got := accountActive(dump); got != want {
			t.Errorf("accountActive(%q) = %v, want %v", dump, got, want)
		}
	}
}

func TestScreenLocked(t *testing.T) {
	// The service's dump also has a deviceLocked row, which stays true after
	// the screen unlocks.
	header := "----ScreenlockService----\n Screenlock system state\tValue\t\tDescription\n" +
		" * screenState  \t\tfalse\t\tscreen on / off status\n * deviceLocked  \t\ttrue\t\t100\n"
	for dump, want := range map[string][2]bool{
		header + " * screenLocked  \t\ttrue\t\t100\n":  {true, true},
		header + " * screenLocked  \t\tfalse\t\t100\n": {false, true},
		header: {false, false},
		"":     {false, false},
	} {
		locked, ok := screenLocked(dump)
		if [2]bool{locked, ok} != want {
			t.Errorf("screenLocked(%q) = %v, %v; want %v", dump, locked, ok, want)
		}
	}
}
