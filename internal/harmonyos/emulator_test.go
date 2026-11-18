package harmonyos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeEmulator writes an Emulator whose -imageList prints list.
func fakeEmulator(t *testing.T, list string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Emulator")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'EOF'\n"+list+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPickImageTakesTheNewestDownloadedMatch(t *testing.T) {
	emulator := fakeEmulator(t, `[
		{"deviceType": "phone", "osVersion": "HarmonyOS 6.0.1(21)", "downloaded": "true"},
		{"deviceType": "phone", "osVersion": "HarmonyOS 6.0.10(23)", "downloaded": "true"},
		{"deviceType": "phone", "osVersion": "HarmonyOS 6.0.2(22)", "downloaded": "true"},
		{"deviceType": "phone", "osVersion": "HarmonyOS 7.0.0(24)", "downloaded": "false"},
		{"deviceType": "tablet", "osVersion": "HarmonyOS 8.0.0(26)", "downloaded": "true"}
	]`)
	cases := map[string]string{"": "HarmonyOS 6.0.10(23)", "6.0.2": "HarmonyOS 6.0.2(22)", "(21)": "HarmonyOS 6.0.1(21)"}
	for want, version := range cases {
		got, err := pickImage(context.Background(), emulator, "phone", want)
		if err != nil || got != version {
			t.Errorf("pickImage(%q) = %q, %v; want %q", want, got, err, version)
		}
	}
	if _, err := pickImage(context.Background(), emulator, "phone", "7.0"); err == nil {
		t.Error("an image that is not downloaded was picked")
	}
}
