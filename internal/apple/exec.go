package apple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/sargunv/devices-cli/internal/remote"
)

// Exec runs the spec's program in the simulator with the UDID and returns its
// exit status. A simulator shares the host's file system, so the program, its
// arguments and its working directory keep their host paths, and nothing is
// copied.
func Exec(ctx context.Context, udid string, spec remote.Spec) (int, error) {
	// spawn runs the host's /bin/sh, since a simulator has none, and the shell
	// starts the program in the working directory. A simulator program that a
	// shell starts needs dyld to load the simulator runtime's libraries, which
	// spawn gives in SIMULATOR_ROOT. spawn passes each SIMCTL_CHILD_<name>
	// variable to the shell as <name>.
	command := spec.Command(remote.Mapping{})
	command.Env = nil
	script := "export DYLD_ROOT_PATH=\"$SIMULATOR_ROOT\"; cd " + remote.ShellQuote(spec.Dir) + " && " + command.Script()
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", udid, "/bin/sh", "-c", script)
	cmd.Env = os.Environ()
	for name, value := range spec.Env {
		cmd.Env = append(cmd.Env, "SIMCTL_CHILD_"+name+"="+value)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), nil
	default:
		return 0, fmt.Errorf("simctl spawn %s: %w; check that Xcode's command line tools work (xcrun simctl list)", udid, err)
	}
}
