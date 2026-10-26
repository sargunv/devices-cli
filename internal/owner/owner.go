// Package owner resolves the directory that owns a device.
package owner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolve returns the owner path: explicit when it is set, otherwise the top
// level of the git worktree that contains the working directory. The path is
// absolute with symlinks resolved, so one directory always has one owner.
func Resolve(explicit string) (string, error) {
	path := explicit
	if path == "" {
		out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return "", errors.New("the working directory is not in a git worktree; pass --owner <path> to name the owner")
		}
		path = strings.TrimSpace(string(out))
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("owner %s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("owner %s is not a directory", resolved)
	}
	return resolved, nil
}

// Exists reports whether the owner path still exists. Only a path that is
// definitely gone counts as missing.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
