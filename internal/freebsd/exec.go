package freebsd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sargunv/devices-cli/internal/remote"
)

// Exec runs the spec's program in the VM with the SSH host and returns its
// exit status. The VM mounts the owner directory at its own path, so the
// program and the synced directories must lie inside it, where they keep
// their paths and nothing is copied.
func Exec(ctx context.Context, host, owner string, spec remote.Spec) (int, error) {
	for _, p := range append([]string{spec.Program}, spec.Syncs...) {
		if p != owner && !strings.HasPrefix(p, owner+string(filepath.Separator)) {
			return 0, fmt.Errorf("%s is outside %s, the directory that the FreeBSD VM mounts; build or keep it inside that directory", p, owner)
		}
	}
	command := spec.Command(remote.Mapping{})
	command.Dir = ""
	if spec.Dir == owner || strings.HasPrefix(spec.Dir, owner+string(filepath.Separator)) {
		command.Dir = spec.Dir
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, command.Script())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		// ssh exits 255 when it fails itself, as a program may too.
		return exit.ExitCode(), nil
	default:
		return 0, fmt.Errorf("ssh %s: %w", host, err)
	}
}
