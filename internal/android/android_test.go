package android

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sargunv/devices-cli/internal/state"
)

const fakeADB = `#!/bin/sh
# Answers the adb calls devices-cli makes, for any serial.
[ "$1" = -s ] && shift 2
case "$*" in
  "get-state") echo device ;;
  "shell getprop sys.boot_completed") echo 1 ;;
  "shell getprop ro.build.version.sdk") echo 36 ;;
  "shell getprop ro.build.version.sdk_full") echo 36.0 ;;
  "shell getprop ro.product.cpu.abilist") echo arm64-v8a,armeabi-v7a ;;
  *) exit 1 ;;
esac
`

// The emulator logs its arguments and runs until it is stopped.
const fakeEmulator = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/../launches"
trap 'exit 0' TERM
while :; do sleep 0.1; done
`

// The android CLI creates each package's directory, installs the fake
// emulator, and logs its arguments.
const fakeAndroidCLI = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/calls"
sdk=
for arg; do
  case "$arg" in
    --sdk=*) sdk=${arg#--sdk=} ;;
    sdk|install) ;;
    emulator) mkdir -p "$sdk/emulator" && cp "$(dirname "$0")/emulator.fake" "$sdk/emulator/emulator" ;;
    *) mkdir -p "$sdk/$arg" ;;
  esac
done
`

func writeScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeSDK returns an SDK with adb, the emulator, and the android CLI, but no
// system images, and points the AVD home at a temporary directory.
func fakeSDK(t *testing.T) SDK {
	t.Helper()
	root := t.TempDir()
	writeScript(t, filepath.Join(root, "platform-tools", "adb"), fakeADB)
	writeScript(t, filepath.Join(root, "emulator", "emulator"), fakeEmulator)
	writeScript(t, filepath.Join(root, "cmdline-tools", "latest", "bin", "android"), fakeAndroidCLI)
	writeScript(t, filepath.Join(root, "cmdline-tools", "latest", "bin", "emulator.fake"), fakeEmulator)
	t.Setenv("ANDROID_AVD_HOME", t.TempDir())
	return SDK{Root: root}
}

func newRequest(t *testing.T, sdk SDK) *Request {
	t.Helper()
	return &Request{
		SDK: sdk, API: "36", Variant: "default", ABI: "arm64-v8a",
		LogDir: t.TempDir(), BootTimeout: 10 * time.Second, Log: io.Discard,
	}
}

func TestPrepareInstallsPackagesAndPrimesTheAVD(t *testing.T) {
	sdk := fakeSDK(t)
	if err := os.RemoveAll(filepath.Join(sdk.Root, "emulator")); err != nil {
		t.Fatal(err)
	}
	req := newRequest(t, sdk)

	if err := req.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := req.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}

	launches, err := os.ReadFile(filepath.Join(sdk.Root, "launches"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(launches)), "\n"); len(lines) != 1 ||
		!strings.Contains(lines[0], "-avd "+req.Definition()+" -read-only -port "+strconv.Itoa(lastPort+2)) {
		t.Errorf("emulator launches:\n%s\nwant one read-only launch above the instances' ports", launches)
	}
	if !strings.Contains(string(launches), " -crash-report-mode disabled") {
		t.Errorf("emulator launches:\n%s\nwant crash reporting disabled", launches)
	}

	calls, err := os.ReadFile(filepath.Join(sdk.Root, "cmdline-tools", "latest", "bin", "calls"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--sdk=" + sdk.Root + " sdk install emulator system-images/android-36/default/arm64-v8a\n"
	if string(calls) != want {
		t.Errorf("android CLI calls:\n%s\nwant:\n%s", calls, want)
	}
	config, err := os.ReadFile(filepath.Join(os.Getenv("ANDROID_AVD_HOME"), req.Definition()+".avd", "config.ini"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"image.sysdir.1=system-images/android-36/default/arm64-v8a/", "hw.cpu.arch=arm64", "abi.type=arm64-v8a"} {
		if !strings.Contains(string(config), line+"\n") {
			t.Errorf("config.ini lacks %s:\n%s", line, config)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("ANDROID_AVD_HOME"), req.Definition()+".ini")); err != nil {
		t.Error(err)
	}
}

func TestEmulatorLifecycle(t *testing.T) {
	sdk := fakeSDK(t)
	req := newRequest(t, sdk)
	req.Args = []string{"-gpu", "swiftshader"}
	running := []state.Instance{{Port: firstPort}}
	if err := req.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}

	inst, err := req.Start(t.Context(), "/owner", running)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Port == firstPort || inst.ID != "emulator-"+strconv.Itoa(inst.Port) {
		t.Errorf("started %s on port %d next to an instance on %d", inst.ID, inst.Port, firstPort)
	}
	if !alive(inst) {
		t.Fatal("started emulator is not alive")
	}
	if err := req.WaitReady(t.Context(), inst); err != nil {
		t.Fatal(err)
	}

	other := inst
	other.Port += 2
	if alive(other) {
		t.Error("an instance with another port matches the emulator's process")
	}

	if err := (Kind{}).Stop(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	if alive(inst) {
		t.Error("emulator is alive after Stop")
	}
	if err := (Kind{}).Stop(t.Context(), inst); err != nil {
		t.Errorf("stopping a stopped emulator: %v", err)
	}
}

func TestWaitReadyReportsAnEmulatorThatExits(t *testing.T) {
	sdk := fakeSDK(t)
	writeScript(t, filepath.Join(sdk.Root, "emulator", "emulator"), "#!/bin/sh\necho 'PANIC: no image'\n")
	writeScript(t, filepath.Join(sdk.Root, "platform-tools", "adb"), "#!/bin/sh\necho 0\n")
	req := newRequest(t, sdk)

	inst, err := req.Start(t.Context(), "/owner", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = req.WaitReady(t.Context(), inst)
	if err == nil || !strings.Contains(err.Error(), "PANIC: no image") {
		t.Errorf("got %v, want an error with the log's end", err)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		api, variant, abi string
		args              []string
		ok                bool
	}{
		{"36", "default", "arm64-v8a", nil, true},
		{"36.1", "google_apis_playstore", "x86_64", []string{"-gpu", "host"}, true},
		{"", "default", "arm64-v8a", nil, false},
		{"36/../x", "default", "arm64-v8a", nil, false},
		{"36", "Default", "arm64-v8a", nil, false},
		{"36", "default", "mips", nil, false},
		{"36", "default", "arm64-v8a", []string{"-port", "5554"}, false},
		{"36", "default", "arm64-v8a", []string{"@other"}, false},
	} {
		req := Request{API: tc.api, Variant: tc.variant, ABI: tc.abi, Args: tc.args}
		if err := req.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v", tc, err)
		}
	}
}

func TestMatchPin(t *testing.T) {
	sdk := fakeSDK(t)
	for _, tc := range []struct {
		api, abi string
		ok       bool
	}{
		{"36", "arm64-v8a", true},
		{"36", "armeabi-v7a", true},
		{"36.0", "arm64-v8a", true},
		{"36.1", "arm64-v8a", false},
		{"35", "arm64-v8a", false},
		{"36", "x86_64", false},
	} {
		req := Request{SDK: sdk, API: tc.api, ABI: tc.abi}
		if err := req.MatchPin(t.Context(), "phone"); (err == nil) != tc.ok {
			t.Errorf("MatchPin for API %s %s = %v", tc.api, tc.abi, err)
		}
	}
}

func TestRemoveDefinition(t *testing.T) {
	sdk := fakeSDK(t)
	req := newRequest(t, sdk)
	if err := req.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("ANDROID_AVD_HOME")

	if err := (Kind{}).RemoveDefinition(req.Definition()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Errorf("AVD home still holds %v", entries)
	}
	if err := (Kind{}).RemoveDefinition("pixel_9"); err == nil {
		t.Error("removed an AVD that devices-cli did not create")
	}
}

// Profile and window mode must not reuse a device with different hardware or UI.
func TestProfileAndWindowIdentity(t *testing.T) {
	sdk := fakeSDK(t)
	req := newRequest(t, sdk)
	baseKey, baseDefinition := req.Key(), req.Definition()
	if baseKey != baseDefinition {
		t.Fatal("default request must reuse the existing headless key")
	}
	req.Args = []string{"-gpu", "host"}
	if want := baseDefinition + " -gpu host"; req.Key() != want {
		t.Fatalf("headless key = %q, want existing key %q", req.Key(), want)
	}
	req.Args = nil
	req.Headful = true
	if req.Key() == baseKey || req.Definition() != baseDefinition {
		t.Fatal("window mode must change instance identity, not the shared AVD")
	}
	req.DeviceProfile = "wearos_small_round"
	if req.Definition() == baseDefinition {
		t.Fatal("profile must change the AVD")
	}
	wearDefinition := req.Definition()
	req.DeviceProfile = "tv_1080p"
	if req.Definition() == wearDefinition {
		t.Fatal("TV reused the watch AVD")
	}
	for _, headful := range []bool{false, true} {
		req.Headful = headful
		inst, err := req.Start(t.Context(), "/owner", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := req.WaitReady(t.Context(), inst); err != nil {
			t.Fatal(err)
		}
		if err := (Kind{}).Stop(t.Context(), inst); err != nil {
			t.Fatal(err)
		}
		launches, err := os.ReadFile(filepath.Join(sdk.Root, "launches"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(launches)), "\n")
		if strings.Contains(lines[len(lines)-1], "-no-window") == headful {
			t.Fatalf("headful=%v: %s", headful, lines[len(lines)-1])
		}
	}
}

func TestHardwareProfilesUseAvdmanager(t *testing.T) {
	sdk := fakeSDK(t)
	writeScript(t, filepath.Join(sdk.Root, "cmdline-tools", "latest", "bin", "avdmanager"), `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/profiles"
`)
	for _, profile := range []string{"wearos_small_round", "tv_1080p"} {
		req := newRequest(t, sdk)
		req.DeviceProfile = profile
		if err := req.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := req.writeDefinition(t.Context()); err != nil {
			t.Fatal(err)
		}
		calls, err := os.ReadFile(filepath.Join(sdk.Root, "cmdline-tools", "latest", "bin", "profiles"))
		if err != nil {
			t.Fatal(err)
		}
		want := "create avd --force --name " + req.Definition() + " --package system-images;android-36;default;arm64-v8a --device " + profile
		if !strings.Contains(string(calls), want+"\n") {
			t.Fatalf("missing %s in %s", want, calls)
		}
	}
}
