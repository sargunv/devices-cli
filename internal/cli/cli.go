// Package cli defines the devices-cli commands.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/apple"
	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/devices"
	"github.com/sargunv/devices-cli/internal/freebsd"
	"github.com/sargunv/devices-cli/internal/harmonyos"
	"github.com/sargunv/devices-cli/internal/ohos"
	"github.com/sargunv/devices-cli/internal/state"
	"github.com/sargunv/devices-cli/internal/wine"
)

// Execute runs the command line and returns the process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := &app{stdin: os.Stdin, stdout: stdout, stderr: stderr}
	root := a.root()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "devices-cli: %v\n", err)
		return 1
	}
	return a.exitCode
}

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// exitCode is the process's exit status after a command that succeeds:
	// for exec, the program's.
	exitCode int
	// emulatorArgs are the flags after -- of "get android".
	emulatorArgs []string
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "devices-cli",
		Short: "Give each worktree its own ephemeral test device",
		Long: `devices-cli gives each git worktree its own test device (an Android
emulator, an Apple simulator, an OpenHarmony or HarmonyOS emulator, or a FreeBSD virtual
machine), starts it on demand, and stops it when the worktree goes away.

A project asks for a device and gets its ID on stdout:

  ANDROID_SERIAL=${ANDROID_SERIAL:-$(devices-cli get android --api 36)}

Every command first stops instances whose worktree is gone or that have been
idle too long, so stale devices go away without a scheduler.

Settings live in $XDG_CONFIG_HOME/devices-cli/config.toml and state in
$XDG_STATE_HOME/devices-cli. See https://github.com/sargunv/devices-cli.`,
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.AddCommand(a.getCommand(), a.execCommand(), a.assignCommand(), a.releaseCommand(), a.lsCommand(), a.gcCommand())
	return root
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

func (a *app) manager() (*devices.Manager, error) {
	configPath, err := config.Path()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	kinds := map[string]devices.Kind{android.KindName: android.Kind{}}
	for _, platform := range apple.Platforms {
		kinds[platform.Kind] = apple.Kind{}
	}
	kinds[ohos.KindName] = ohos.Kind{}
	kinds[freebsd.KindName] = freebsd.Kind{VMDir: filepath.Join(stateDir, "vms")}
	// Collection runs without DevEco Studio too, and then signals the process.
	emulator, _ := harmonyos.FindEmulator()
	kinds[harmonyos.KindName] = harmonyos.Kind{Root: filepath.Join(stateDir, "harmonyos"), Emulator: emulator}
	kinds[wine.KindName] = wine.Kind{}
	return &devices.Manager{
		Store:      state.Store{Dir: stateDir},
		Config:     cfg,
		ConfigPath: configPath,
		Kinds:      kinds,
		Log:        a.stderr,
		Poll:       5 * time.Second,
		Now:        time.Now,
	}, nil
}

func logDir() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return dir + string(os.PathSeparator) + "logs", nil
}
