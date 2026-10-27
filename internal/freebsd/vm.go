// Package freebsd runs FreeBSD virtual machines with anyvm: one per owner, with
// the owner directory mounted at its own path.
package freebsd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sargunv/devices-cli/internal/process"
	"github.com/sargunv/devices-cli/internal/state"
)

// KindName is the kind of every FreeBSD instance.
const KindName = "freebsd"

// Each VM takes an even host port for SSH and the next one for its serial
// console.
const (
	firstPort = 60000
	lastPort  = 60198
)

var releasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// Request asks for a VM of one FreeBSD release.
type Request struct {
	Release string
	// AnyVM is the command that runs anyvm.
	AnyVM []string
	// VMDir holds one directory per VM; CacheDir is anyvm's shared image
	// cache.
	VMDir, CacheDir string
	LogDir          string
	BootTimeout     time.Duration

	// launchLock is held from Prepare until anyvm exits.
	launchLock *os.File
}

// NewRequest checks the host and finds anyvm: ANYVM names anyvm.py, or anyvm
// is on PATH.
func NewRequest(release string) (*Request, error) {
	if !releasePattern.MatchString(release) {
		return nil, fmt.Errorf("--release %q is not a FreeBSD release such as 14.4", release)
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New("FreeBSD VMs need a Linux host with KVM")
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("FreeBSD VMs need read and write access to /dev/kvm: %w", err)
	}
	kvm.Close()
	var anyvm []string
	if path := os.Getenv("ANYVM"); path != "" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("ANYVM: %w", err)
		}
		anyvm = []string{"python3", path}
	} else if path, err := exec.LookPath("anyvm"); err == nil {
		anyvm = []string{path}
	} else {
		return nil, errors.New("no anyvm: set ANYVM to anyvm.py (from github.com/anyvm-org/anyvm) or put anyvm on PATH")
	}
	return &Request{Release: release, AnyVM: anyvm}, nil
}

// Kind implements devices.Request.
func (r *Request) Kind() string { return KindName }

// Key implements devices.Request.
func (r *Request) Key() string { return "freebsd " + r.Release }

// Description implements devices.Request.
func (r *Request) Description() string { return "freebsd " + r.Release }

// Definition implements devices.Request. anyvm owns its image cache.
func (r *Request) Definition() string { return "" }

// Prepare takes the machine-wide FreeBSD launch lock. anyvm shares downloads
// and SSH configuration files between VMs without locking them, so one anyvm
// sets a VM up at a time; the VMs then run side by side. Start hands the lock
// to anyvm, which holds it until it exits.
func (r *Request) Prepare(context.Context) error {
	if err := os.MkdirAll(r.VMDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.VMDir, "launch.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	r.launchLock = f
	return nil
}

// releaseLaunchLock drops this process's hold on the launch lock.
func (r *Request) releaseLaunchLock() {
	if r.launchLock != nil {
		r.launchLock.Close()
		r.launchLock = nil
	}
}

// Start launches anyvm for the owner on the first free pair of ports. anyvm
// boots the VM, mounts the owner directory, and exits, leaving QEMU running in
// the process group it started.
func (r *Request) Start(_ context.Context, owner string, running []state.Instance) (state.Instance, error) {
	port := 0
	for p := firstPort; p <= lastPort; p += 2 {
		taken := slices.ContainsFunc(running, func(in state.Instance) bool { return in.Kind == KindName && in.Port == p })
		if !taken && process.Bindable(p) && process.Bindable(p+1) {
			port = p
			break
		}
	}
	if port == 0 {
		return state.Instance{}, fmt.Errorf("no free SSH port between %d and %d", firstPort, lastPort)
	}
	dir := dataDir(r.VMDir, port)
	if err := os.RemoveAll(dir); err != nil {
		return state.Instance{}, err
	}
	for _, d := range []string{dir, r.CacheDir, r.LogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return state.Instance{}, err
		}
	}
	logPath := filepath.Join(r.LogDir, fmt.Sprintf("freebsd-%d.log", port))
	logFile, err := os.Create(logPath)
	if err != nil {
		return state.Instance{}, err
	}
	defer logFile.Close()

	alias := sshAlias(port)
	// --snapshot boots the cached image without writing it, so VMs share it.
	args := append(slices.Clone(r.AnyVM[1:]),
		"--os", "freebsd",
		"--release", r.Release,
		"--data-dir", dir,
		"--cache-dir", r.CacheDir,
		"--ssh-port", strconv.Itoa(port),
		"--serial", strconv.Itoa(port+1),
		"--ssh-name", alias,
		"--vnc", "off",
		"--remote-vnc", "no",
		"--sync", "nfs",
		"-v", owner+":"+owner,
		"--snapshot",
		"-d",
	)
	cmd := exec.Command(r.AnyVM[0], args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own session keeps the VM running after devices-cli exits and makes
	// anyvm and QEMU a process group to stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if r.launchLock != nil {
		cmd.ExtraFiles = []*os.File{r.launchLock}
	}
	err = cmd.Start()
	r.releaseLaunchLock()
	if err != nil {
		return state.Instance{}, fmt.Errorf("start anyvm: %w", err)
	}
	go func() { _ = cmd.Wait() }()

	now := time.Now()
	return state.Instance{
		Owner:       owner,
		Kind:        KindName,
		Key:         r.Key(),
		Description: r.Description(),
		ID:          alias,
		PID:         cmd.Process.Pid,
		Port:        port,
		Log:         logPath,
		Started:     now,
		LastUsed:    now,
	}, nil
}

// dataDir returns the directory of the VM on a port.
func dataDir(vmDir string, port int) string {
	return filepath.Join(vmDir, fmt.Sprintf("freebsd-%d", port))
}

// sshAlias is the host name that anyvm adds to the SSH configuration.
func sshAlias(port int) string { return fmt.Sprintf("devices-cli-freebsd-%d", port) }

// WaitReady waits until anyvm has finished and the VM answers SSH.
func (r *Request) WaitReady(ctx context.Context, inst state.Instance) error {
	// A get that reuses a running VM starts no anyvm.
	r.releaseLaunchLock()
	deadline := time.Now().Add(r.BootTimeout)
	for {
		if !alive(inst) {
			return fmt.Errorf("FreeBSD VM %s exited before it booted%s", inst.ID, process.LogTail(inst.Log))
		}
		// anyvm exits once the VM is up. It only warns when the mount fails,
		// so the mount is checked too.
		if !process.Running(inst.PID, "anyvm") && ssh(ctx, inst.ID, "true") == nil {
			if mounted(ctx, inst) {
				return nil
			}
			return fmt.Errorf("FreeBSD VM %s did not mount %s%s", inst.ID, inst.Owner, process.LogTail(inst.Log))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("FreeBSD VM %s did not boot within %s%s", inst.ID, r.BootTimeout, process.LogTail(inst.Log))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// mounted reports whether the guest has a file system mounted at the owner
// directory's path: one whose device differs from its parent's.
func mounted(ctx context.Context, inst state.Instance) bool {
	command := "stat -f %d " + shellQuote(inst.Owner) + " " + shellQuote(filepath.Dir(inst.Owner))
	out, err := sshOutput(ctx, inst.ID, command)
	if err != nil {
		return false
	}
	devices := strings.Fields(out)
	return len(devices) == 2 && devices[0] != devices[1]
}

// shellQuote quotes s for the guest's shell, which ssh hands the command to.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ssh(ctx context.Context, host string, command ...string) error {
	_, err := sshOutput(ctx, host, command...)
	return err
}

func sshOutput(ctx context.Context, host string, command ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host}, command...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}

// Kind manages FreeBSD VM instances. VMDir is the directory that holds the
// VMs' directories, which Stop removes.
type Kind struct {
	VMDir string
}

// Alive implements devices.Kind.
func (k Kind) Alive(_ context.Context, inst state.Instance) bool { return alive(inst) }

// alive reports whether the instance's process group still holds anyvm or
// QEMU for its data directory, so a reused process ID does not count.
func alive(inst state.Instance) bool {
	pattern := regexp.QuoteMeta("freebsd-"+strconv.Itoa(inst.Port)) + "([ /]|$)"
	return process.GroupRunning(inst.PID, pattern)
}

// Stop implements devices.Kind. It shuts the guest down, as `shutdown -p now`
// does on a machine, and only signals QEMU when the guest does not power off.
// Then it removes the VM's directory and its SSH alias.
func (k Kind) Stop(ctx context.Context, inst state.Instance) error {
	if alive(inst) {
		_ = ssh(ctx, inst.ID, "shutdown", "-p", "now")
		if !waitDead(inst, time.Minute) {
			if err := process.StopGroup(inst.PID, 15*time.Second); err != nil {
				return err
			}
		}
	}
	if err := removeSSHConfig(inst); err != nil {
		return err
	}
	return os.RemoveAll(dataDir(k.VMDir, inst.Port))
}

func waitDead(inst state.Instance, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for alive(inst) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
	return true
}

// removeSSHConfig deletes the file that anyvm wrote for the VM's port, if it
// still names the VM's alias.
func removeSSHConfig(inst state.Instance) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".ssh", "config.d", strconv.Itoa(inst.Port)+".conf")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !strings.Contains(string(data), " "+inst.ID+"\n")) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// RemoveDefinition implements devices.Kind. FreeBSD VMs have no definitions.
func (Kind) RemoveDefinition(string) error { return nil }
