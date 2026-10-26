// Package android runs Android emulators from machine-wide AVD definitions and
// matches pinned physical Android devices.
package android

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SDK is an Android SDK installation.
type SDK struct {
	Root string
}

// FindSDK returns the SDK that ANDROID_HOME or ANDROID_SDK_ROOT names.
func FindSDK() (SDK, error) {
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if root := os.Getenv(env); root != "" {
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				return SDK{}, fmt.Errorf("%s is %s, which is not a directory", env, root)
			}
			return SDK{Root: root}, nil
		}
	}
	return SDK{}, errors.New("no Android SDK: set ANDROID_HOME to the SDK directory (a project usually sets it in its mise environment)")
}

func (s SDK) adbPath() string      { return filepath.Join(s.Root, "platform-tools", "adb") }
func (s SDK) emulatorPath() string { return filepath.Join(s.Root, "emulator", "emulator") }

func (s SDK) has(pkg []string) bool {
	_, err := os.Stat(filepath.Join(append([]string{s.Root}, pkg...)...))
	return err == nil
}

// install installs SDK packages, each given as its path segments under the
// SDK root. It prefers Google's android CLI, which needs no JDK, and falls back
// to sdkmanager.
func (s SDK) install(ctx context.Context, log io.Writer, pkgs [][]string) error {
	var cmd *exec.Cmd
	if android := s.findTool("android"); android != "" {
		args := []string{"--sdk=" + s.Root, "sdk", "install"}
		for _, pkg := range pkgs {
			args = append(args, strings.Join(pkg, "/"))
		}
		cmd = exec.CommandContext(ctx, android, args...)
	} else if sdkmanager := s.findTool("sdkmanager"); sdkmanager != "" {
		args := []string{"--sdk_root=" + s.Root, "--install"}
		for _, pkg := range pkgs {
			args = append(args, strings.Join(pkg, ";"))
		}
		cmd = exec.CommandContext(ctx, sdkmanager, args...)
		// Accepts each license prompt.
		cmd.Stdin = strings.NewReader(strings.Repeat("y\n", 20))
	} else {
		return fmt.Errorf("cannot install Android SDK packages into %s: put Google's android CLI on PATH "+
			"(for example `mise use http:android-cli`) or install the SDK command-line tools", s.Root)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	return nil
}

// findTool returns the path of an SDK command-line tool: from the SDK's
// command-line tools if it has them, otherwise from PATH.
func (s SDK) findTool(name string) string {
	candidates := []string{filepath.Join(s.Root, "cmdline-tools", "latest", "bin", name)}
	matches, _ := filepath.Glob(filepath.Join(s.Root, "cmdline-tools", "*", "bin", name))
	candidates = append(candidates, matches...)
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	path, _ := exec.LookPath(name)
	return path
}

// adb runs an adb command and returns its trimmed output. A call to a device
// that is still booting can block indefinitely, so each call has a deadline.
func (s SDK) adb(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.adbPath(), args...)
	// The adb server crashes in mDNS discovery on some hosts, and devices-cli
	// never needs it.
	cmd.Env = append(os.Environ(), "ADB_MDNS=0")
	// An adb client that starts the server can leave it holding the output
	// pipe; the client's own exit is what matters.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	return strings.TrimSpace(strings.ReplaceAll(string(out), "\r", "")), err
}
