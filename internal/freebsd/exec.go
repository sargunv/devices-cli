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
// their paths and nothing is copied. The owner path has its symbolic links
// resolved, as the VM mounts it, so paths reached through a link are resolved
// too.
func Exec(ctx context.Context, host, owner string, spec remote.Spec) (int, error) {
	inside := func(p string) bool { return p == owner || strings.HasPrefix(p, owner+string(filepath.Separator)) }
	mapping := remote.Mapping{}
	for _, p := range append([]string{spec.Program}, spec.Syncs...) {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return 0, err
		}
		if !inside(resolved) {
			return 0, fmt.Errorf("%s is outside %s, the directory that the FreeBSD VM mounts; build or keep it inside that directory", p, owner)
		}
		mapping[p] = resolved
	}
	command := spec.Command(mapping)
	command.Dir = ""
	if dir, err := filepath.EvalSymlinks(spec.Dir); err == nil && inside(dir) {
		command.Dir = dir
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
