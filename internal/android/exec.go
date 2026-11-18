package android

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sargunv/devices-cli/internal/remote"
)

// execRoot holds what exec copies to a device, one directory per owner.
const execRoot = "/data/local/tmp/devices-cli"

// Exec runs the spec's program on the device with the serial and returns its
// exit status. The program and the synced directories are copied under a
// directory of the owner's (see remote.Plan), where an unchanged copy is
// reused.
func (s SDK) Exec(ctx context.Context, serial, owner string, spec remote.Spec, lock remote.Locker) (int, error) {
	sh := adbShell{sdk: s, serial: serial}
	sendFile := func(ctx context.Context, host, dir string) error {
		dest := path.Join(dir, filepath.Base(host))
		var errOut bytes.Buffer
		status, err := s.adbStreams(ctx, nil, io.Discard, &errOut, "-s", serial, "push", host, dest)
		if err == nil && status != 0 {
			err = fmt.Errorf("adb push %s: %s", host, strings.TrimSpace(errOut.String()))
		}
		if err != nil {
			return err
		}
		_, err = sh.Run(ctx, nil, "chmod 755 "+remote.ShellQuote(dest))
		return err
	}
	sendTree := func(ctx context.Context, tree *remote.Tree, dir string) error {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(tree.WriteTar(pw)) }()
		_, err := sh.Run(ctx, pr, "tar -xf - -C "+remote.ShellQuote(dir))
		pr.CloseWithError(err)
		return err
	}
	copies, mapping, err := remote.Plan(spec, path.Join(execRoot, remote.Hash(owner)), sendFile, sendTree)
	if err != nil {
		return 0, err
	}
	if err := remote.CopyMissing(ctx, sh, copies, lock); err != nil {
		return 0, err
	}
	cmd := spec.Command(mapping)
	if cmd.Dir == "" {
		cmd.Dir = path.Dir(cmd.Program)
	}
	return s.adbStreams(ctx, spec.Stdin, spec.Stdout, spec.Stderr, "-s", serial, "shell", cmd.Script())
}

// adbShell runs short commands through "adb shell".
type adbShell struct {
	sdk    SDK
	serial string
}

func (a adbShell) Name() string { return a.serial }

func (a adbShell) Run(ctx context.Context, stdin io.Reader, script string) (string, error) {
	var out, errOut bytes.Buffer
	status, err := a.sdk.adbStreams(ctx, stdin, &out, &errOut, "-s", a.serial, "shell", script)
	if err == nil && status != 0 {
		err = fmt.Errorf("exit status %d: %s", status, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), err
}

// adbStreams runs adb with the streams and returns its exit status, which for
// "adb shell" is the remote command's. It returns an error only when adb could
// not run.
func (s SDK) adbStreams(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, s.adbPath(), args...)
	cmd.Env = append(os.Environ(), "ADB_MDNS=0")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), nil
	default:
		return 0, fmt.Errorf("run %s: %w; check that ANDROID_HOME's platform-tools work", s.adbPath(), err)
	}
}
