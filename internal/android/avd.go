package android

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// definitionPrefix marks the AVDs that devices-cli owns; it never touches
// others.
const definitionPrefix = "devices-cli-"

// avdHome returns the directory the emulator reads AVDs from.
func avdHome() (string, error) {
	if dir := os.Getenv("ANDROID_AVD_HOME"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("ANDROID_USER_HOME"); dir != "" {
		return filepath.Join(dir, "avd"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".android", "avd"), nil
}

var cpuArch = map[string]string{
	"arm64-v8a":   "arm64",
	"armeabi-v7a": "arm",
	"x86_64":      "x86_64",
	"x86":         "x86",
}

func imagePackage(api, variant, abi string) []string {
	return []string{"system-images", "android-" + api, variant, abi}
}

// writeDefinition creates the AVD files for an image unless they exist. The
// hardware is a mid-size phone; anything else is a launch flag.
func writeDefinition(name, api, variant, abi string) error {
	home, err := avdHome()
	if err != nil {
		return err
	}
	ini := filepath.Join(home, name+".ini")
	dir := filepath.Join(home, name+".avd")
	config := filepath.Join(dir, "config.ini")
	if fileExists(ini) && fileExists(config) {
		return nil
	}

	playStore := "no"
	if strings.Contains(variant, "playstore") {
		playStore = "yes"
	}
	lines := []string{
		"avd.ini.encoding=UTF-8",
		"AvdId=" + name,
		"avd.ini.displayname=" + name,
		"abi.type=" + abi,
		"hw.cpu.arch=" + cpuArch[abi],
		"image.sysdir.1=" + filepath.Join(imagePackage(api, variant, abi)...) + "/",
		"tag.id=" + variant,
		"target=android-" + api,
		"PlayStore.enabled=" + playStore,
		"hw.ramSize=2048",
		"hw.lcd.width=1080",
		"hw.lcd.height=2400",
		"hw.lcd.density=420",
		"disk.dataPartition.size=6G",
		"hw.sdCard=no",
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(config, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	// The emulator lists an AVD by its .ini file, so that comes last.
	pointer := fmt.Sprintf("avd.ini.encoding=UTF-8\npath=%s\ntarget=android-%s\n", dir, api)
	return os.WriteFile(ini, []byte(pointer), 0o644)
}

// RemoveDefinition deletes an AVD that devices-cli created.
func (Kind) RemoveDefinition(name string) error {
	if !strings.HasPrefix(name, definitionPrefix) {
		return fmt.Errorf("refusing to remove AVD %s, which devices-cli did not create", name)
	}
	home, err := avdHome()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(home, name+".ini"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(filepath.Join(home, name+".avd"))
}

// primedMarker returns the file that records that the AVD has been primed.
func primedMarker(name string) (string, error) {
	home, err := avdHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, name+".avd", "devices-cli-primed"), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
