package cli

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/apple"
	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/devices"
	"github.com/sargunv/devices-cli/internal/freebsd"
	"github.com/sargunv/devices-cli/internal/ohos"
	"github.com/sargunv/devices-cli/internal/remote"
	"github.com/sargunv/devices-cli/internal/state"
)

type execFlags struct {
	sync    []string
	syncEnv []string
	env     []string
}

func (a *app) execCommand() *cobra.Command {
	var flags getFlags
	var ef execFlags
	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Run a host-built program on this worktree's device",
		Long: `Run a program built on the host for the device on this worktree's device,
starting the device as get does, and exit with the program's exit status.

The program's output goes to stdout and stderr; devices-cli's own messages go
to stderr. --env forwards host environment variables, by name or glob. --sync
copies a host directory to the device, and --sync-env copies the directory
that a variable names and forwards the variable. A forwarded value, an
argument, or the working directory that is a synced path, or lies inside one,
becomes the path of its copy. Copies are named for their contents, so an
unchanged program or directory is not copied again. Symbolic links are copied
as what they point to, and version control directories and directories with a
CACHEDIR.TAG, such as Cargo's target directory, are left out.

Apple simulators and FreeBSD VMs see the host's paths, so nothing is copied
there; the program and the synced directories of a FreeBSD VM must lie in the
worktree, which the VM mounts. A simulator starts the program in its own data
directory, not the working directory. On OpenHarmony, the program's stderr
arrives after it exits.

exec works as Cargo's target runner, which appends the program and its
arguments to the command. End the runner with --, so that the program's
flags are not taken for devices-cli's.`,
		Example: `  devices-cli exec ios -- ./target/aarch64-apple-ios-sim/debug/app
  CARGO_TARGET_AARCH64_LINUX_ANDROID_RUNNER='devices-cli exec android --api 36 --sync-env CARGO_MANIFEST_DIR --env RUST_* --' \
    cargo test --target aarch64-linux-android`,
	}
	flags.bind(cmd)
	cmd.PersistentFlags().StringArrayVar(&ef.sync, "sync", nil, "host directory to copy to the device (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&ef.syncEnv, "sync-env", nil, "variable that names a host directory to copy to the device; the variable is forwarded (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&ef.env, "env", nil, "host variable to forward, by name or glob such as RUST_* (repeatable)")
	for _, kind := range a.kindCommands() {
		sub := &cobra.Command{
			Use:   kind.name + kind.usage + " -- <program> [arguments]",
			Short: "Run a program on " + kind.device,
			Args: func(cmd *cobra.Command, args []string) error {
				if cmd.ArgsLenAtDash() != 0 || len(args) == 0 {
					return errors.New("name the program after --, as in: devices-cli exec " + cmd.Name() + " -- <program> [arguments]")
				}
				return nil
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				spec, err := ef.spec(args)
				if err != nil {
					return err
				}
				spec.Stdin, spec.Stdout, spec.Stderr = a.stdin, a.stdout, a.stderr
				req, err := kind.request(cmd.Context(), &flags)
				if err != nil {
					return err
				}
				status, err := a.exec(cmd, &flags, req, spec)
				if err != nil {
					return err
				}
				a.exitCode = status
				return nil
			},
		}
		kind.bind(sub)
		cmd.AddCommand(sub)
	}
	return cmd
}

// spec makes the program's spec from the flags and the arguments after --.
func (ef execFlags) spec(args []string) (remote.Spec, error) {
	dir, err := os.Getwd()
	if err != nil {
		return remote.Spec{}, err
	}
	program, err := filepath.Abs(args[0])
	if err != nil {
		return remote.Spec{}, err
	}
	if info, err := os.Stat(program); err != nil || !info.Mode().IsRegular() {
		return remote.Spec{}, fmt.Errorf("%s is not a file; name the host-built program to run", args[0])
	}
	spec := remote.Spec{Program: program, Args: args[1:], Env: map[string]string{}, Dir: dir}
	for _, pattern := range ef.env {
		if _, err := path.Match(pattern, ""); err != nil {
			return remote.Spec{}, fmt.Errorf("--env %q is not a valid glob: %w", pattern, err)
		}
		for _, kv := range os.Environ() {
			name, value, _ := strings.Cut(kv, "=")
			if ok, _ := path.Match(pattern, name); ok {
				spec.Env[name] = value
			}
		}
	}
	syncs := ef.sync
	for _, name := range ef.syncEnv {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			return remote.Spec{}, fmt.Errorf("--sync-env %s: %s is not set; set it to the directory to copy", name, name)
		}
		abs, err := filepath.Abs(value)
		if err != nil {
			return remote.Spec{}, err
		}
		spec.Env[name] = abs
		syncs = append(syncs, abs)
	}
	for _, d := range syncs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return remote.Spec{}, err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return remote.Spec{}, fmt.Errorf("%s is not a directory; name a directory to sync", d)
		}
		spec.Syncs = append(spec.Syncs, abs)
	}
	return spec, nil
}

// exec gets the device for the request and runs the spec on it.
func (a *app) exec(cmd *cobra.Command, flags *getFlags, req devices.Request, spec remote.Spec) (int, error) {
	ctx := cmd.Context()
	ownerPath, id, err := a.device(cmd, flags, req)
	if err != nil {
		return 0, err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return 0, err
	}
	lock := state.Store{Dir: stateDir}.Lock
	switch r := req.(type) {
	case *android.Request:
		return r.SDK.Exec(ctx, id, ownerPath, spec, lock)
	case *apple.Request:
		return apple.Exec(ctx, id, spec)
	case *ohos.Request:
		return ohos.Exec(ctx, r.HDC, id, ownerPath, spec, lock)
	case *freebsd.Request:
		return freebsd.Exec(ctx, id, ownerPath, spec)
	default:
		return 0, fmt.Errorf("exec does not support %s devices", req.Kind())
	}
}
