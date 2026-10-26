// Package cli defines the devices-cli commands.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/apple"
	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/devices"
	"github.com/sargunv/devices-cli/internal/state"
)

// Execute runs the command line and returns the process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRoot(stdout, stderr)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "devices-cli: %v\n", err)
		return 1
	}
	return 0
}

type app struct {
	stdout, stderr io.Writer
}

func newRoot(stdout, stderr io.Writer) *cobra.Command {
	a := &app{stdout: stdout, stderr: stderr}
	root := &cobra.Command{
		Use:   "devices-cli",
		Short: "Give each worktree its own ephemeral test device",
		Long: `devices-cli gives each git worktree its own Android emulator or Apple
simulator, starts it on demand, and stops it when the worktree goes away.

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
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(a.getCommand(), a.assignCommand(), a.releaseCommand(), a.lsCommand(), a.gcCommand())
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
