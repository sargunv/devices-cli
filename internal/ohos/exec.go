package ohos

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sargunv/devices-cli/internal/remote"
)

// execRoot holds what exec copies to a device, one directory per owner.
const execRoot = "/data/local/tmp/devices-cli"

// statusMarker starts the line that reports the program's exit status, since
// hdc shell exits 0 whatever the command does.
const statusMarker = "__devices_cli_status="

// Exec runs the spec's program on the device with the hdc connect key and
// returns its exit status. The program and the synced directories are copied
// under a directory of the owner's (see remote.Plan). hdc shell merges the
// program's standard error into its output, so the program's standard error
// goes to a file on the device, which is written to the spec's Stderr after
// the program exits.
func Exec(ctx context.Context, hdcPath, key, owner string, spec remote.Spec, lock remote.Locker) (int, error) {
	sh := hdcShell{path: hdcPath, key: key}
	sendFile := func(ctx context.Context, host, dir string) error {
		dest := path.Join(dir, filepath.Base(host))
		if err := sh.send(ctx, host, dest); err != nil {
			return err
		}
		_, err := sh.Run(ctx, nil, "chmod 755 "+remote.ShellQuote(dest))
		return err
	}
	sendTree := func(ctx context.Context, tree *remote.Tree, dir string) error {
		f, err := os.CreateTemp("", "devices-cli-sync-*.tar")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		err = tree.WriteTar(f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		archive := dir + ".tar"
		if err := sh.send(ctx, f.Name(), archive); err != nil {
			return err
		}
		q := remote.ShellQuote
		_, err = sh.Run(ctx, nil, "tar -xf "+q(archive)+" -C "+q(dir)+" && rm -f "+q(archive))
		return err
	}
	root := path.Join(execRoot, remote.Hash(owner))
	copies, mapping, err := remote.Plan(spec, root, sendFile, sendTree)
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
	errFile := path.Join(root, fmt.Sprintf("stderr-%d-%d", os.Getpid(), time.Now().UnixNano()))
	q := remote.ShellQuote
	script := "(" + cmd.Script() + ") 2>" + q(errFile) + "; printf '\\n" + statusMarker + "%d\\n' $?"
	status, err := sh.stream(ctx, nil, spec.Stdout, script)
	if errOut, catErr := sh.Run(ctx, nil, "cat "+q(errFile)+"; rm -f "+q(errFile)); catErr == nil && errOut != "" {
		fmt.Fprintln(spec.Stderr, errOut)
	}
	return status, err
}

// hdcShell runs commands through "hdc shell" on one device.
type hdcShell struct {
	path, key string
}

func (h hdcShell) Name() string { return h.key }

func (h hdcShell) send(ctx context.Context, host, dest string) error {
	out, err := hdc(ctx, h.path, "-t", h.key, "file", "send", host, dest)
	if err != nil {
		return fmt.Errorf("hdc file send %s: %w", host, err)
	}
	if strings.Contains(strings.ToLower(out), "fail") {
		return fmt.Errorf("hdc file send %s: %s", host, out)
	}
	return nil
}

// Run implements remote.Shell.
func (h hdcShell) Run(ctx context.Context, stdin io.Reader, script string) (string, error) {
	if stdin != nil {
		return "", errors.New("hdc shell takes no input")
	}
	var out bytes.Buffer
	status, err := h.stream(ctx, nil, &out, script+" 2>&1; printf '\\n"+statusMarker+"%d\\n' $?")
	text := strings.TrimSpace(out.String())
	if err == nil && status != 0 {
		err = fmt.Errorf("exit status %d: %s", status, text)
	}
	return text, err
}

// stream runs a script that ends by printing the status marker line, copies
// its output before that line to stdout, and returns the status it reports.
func (h hdcShell) stream(ctx context.Context, stdin io.Reader, stdout io.Writer, script string) (int, error) {
	cmd := exec.CommandContext(ctx, h.path, "-t", h.key, "shell", script)
	cmd.Stdin = stdin
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("run %s: %w; put the OpenHarmony SDK's toolchains directory on PATH", h.path, err)
	}
	status := -1
	reader := bufio.NewReader(pipe)
	// Each line is written once the next one arrives, so that the blank line
	// before the marker, which the script adds, is never written.
	var pending []byte
	for {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.ReplaceAll(line, []byte("\r\n"), []byte("\n"))
		if rest, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte(statusMarker)); ok {
			if n, err := strconv.Atoi(string(rest)); err == nil {
				status = n
				pending = bytes.TrimSuffix(pending, []byte("\n"))
				continue
			}
		}
		if len(line) > 0 {
			if _, err := stdout.Write(pending); err != nil {
				_ = cmd.Process.Kill()
				return 0, err
			}
			pending = line
		}
		if readErr != nil {
			break
		}
	}
	if _, err := stdout.Write(pending); err != nil {
		return 0, err
	}
	if err := cmd.Wait(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return 0, fmt.Errorf("hdc shell on %s: %w: %s", h.key, err, strings.TrimSpace(errOut.String()))
	}
	if status < 0 {
		return 0, fmt.Errorf("hdc shell on %s ended without the command's exit status; check that the emulator still runs (devices-cli ls)", h.key)
	}
	return status, nil
}
