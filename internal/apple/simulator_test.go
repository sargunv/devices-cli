package apple

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sargunv/devices-cli/internal/state"
)

const runtimesJSON = `{"runtimes": [
  {"identifier": "rt.iOS-26-4", "version": "26.4.1", "platform": "iOS", "isAvailable": true,
   "supportedDeviceTypes": [
     {"identifier": "dt.iPad", "name": "iPad Pro", "productFamily": "iPad"},
     {"identifier": "dt.iPhone-17", "name": "iPhone 17", "productFamily": "iPhone"}]},
  {"identifier": "rt.iOS-27-0", "version": "27.0", "platform": "iOS", "isAvailable": true,
   "supportedDeviceTypes": [{"identifier": "dt.iPhone-18", "name": "iPhone 18", "productFamily": "iPhone"}]},
  {"identifier": "rt.iOS-28-0", "version": "28.0", "platform": "iOS", "isAvailable": false,
   "supportedDeviceTypes": []},
  {"identifier": "rt.tvOS-26-0", "version": "26.0", "platform": "tvOS", "isAvailable": true,
   "supportedDeviceTypes": [{"identifier": "dt.tv", "name": "Apple TV", "productFamily": "Apple TV"}]}
]}`

const devicesJSON = `{"devices": {"rt.iOS-26-4": [
  {"udid": "OURS", "name": "devices-cli wt", "state": "Booted"},
  {"udid": "THEIRS", "name": "iPhone 17", "state": "Shutdown"}
]}}`

// fakeXcrun puts an xcrun on PATH that answers simctl list and logs other
// calls.
func fakeXcrun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"runtimes.json": runtimesJSON, "devices.json": devicesJSON} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
dir=$(dirname "$0")
case "$2 $3" in
  "list runtimes") cat "$dir/runtimes.json" ;;
  "list devices") cat "$dir/devices.json" ;;
  *) echo "$@" >> "$dir/calls" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestNewRequest(t *testing.T) {
	fakeXcrun(t)
	ios := Platforms[0]
	for _, tc := range []struct {
		version, deviceType string
		runtime, want       string
	}{
		{"", "", "rt.iOS-27-0", "dt.iPhone-18"},
		{"26", "", "rt.iOS-26-4", "dt.iPhone-17"},
		{"26.4", "iPad Pro", "rt.iOS-26-4", "dt.iPad"},
		{"26.4.1", "dt.iPad", "rt.iOS-26-4", "dt.iPad"},
	} {
		req, err := NewRequest(t.Context(), ios, tc.version, tc.deviceType)
		if err != nil {
			t.Errorf("NewRequest(%q, %q): %v", tc.version, tc.deviceType, err)
			continue
		}
		if req.Runtime.Identifier != tc.runtime || req.DeviceType.Identifier != tc.want {
			t.Errorf("NewRequest(%q, %q) = %s %s, want %s %s", tc.version, tc.deviceType,
				req.Runtime.Identifier, req.DeviceType.Identifier, tc.runtime, tc.want)
		}
	}

	for _, tc := range []struct{ version, deviceType, message string }{
		{"2", "", "installed: 26.4.1, 27.0"},
		{"28", "", "xcodebuild -downloadPlatform iOS"},
		{"27", "iPad Pro", "supports: iPhone 18"},
	} {
		if _, err := NewRequest(t.Context(), ios, tc.version, tc.deviceType); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("NewRequest(%q, %q) = %v, want an error with %q", tc.version, tc.deviceType, err, tc.message)
		}
	}
}

func TestStopDeletesOnlyItsOwnSimulators(t *testing.T) {
	dir := fakeXcrun(t)

	if err := (Kind{}).Stop(t.Context(), state.Instance{ID: "OURS"}); err != nil {
		t.Fatal(err)
	}
	if err := (Kind{}).Stop(t.Context(), state.Instance{ID: "THEIRS"}); err == nil {
		t.Error("deleted a simulator that devices-cli did not create")
	}
	if err := (Kind{}).Stop(t.Context(), state.Instance{ID: "GONE"}); err != nil {
		t.Errorf("stopping a deleted simulator: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "simctl shutdown OURS\nsimctl delete OURS\n"; string(calls) != want {
		t.Errorf("simctl calls:\n%s\nwant:\n%s", calls, want)
	}
	if !(Kind{}).Alive(t.Context(), state.Instance{ID: "OURS"}) || (Kind{}).Alive(t.Context(), state.Instance{ID: "THEIRS"}) {
		t.Error("Alive does not follow the simulator's state")
	}
}
