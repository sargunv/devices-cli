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
// exit status. A simulator shares the host's file system, so the program and
// its arguments keep their host paths, and nothing is copied. simctl spawn
// starts the program in the simulator's data directory: a simulator has no
// shell to change to the host's working directory first.
func Exec(ctx context.Context, udid string, spec remote.Spec) (int, error) {
	args := append([]string{"simctl", "spawn", udid, spec.Program}, spec.Args...)
	cmd := exec.CommandContext(ctx, "xcrun", args...)
	// spawn passes each SIMCTL_CHILD_<name> variable to the program as <name>.
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
