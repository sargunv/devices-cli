// Package ohos runs OpenHarmony emulators: one QEMU instance of the Oniro
// image per owner, reached through hdc.
package ohos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sargunv/devices-cli/internal/process"
	"github.com/sargunv/devices-cli/internal/state"
)

// KindName is the kind of every OpenHarmony instance.
const KindName = "ohos"

// The host ports that forward to each guest's hdc daemon.
const (
	firstPort = 56000
	lastPort  = 56199
	guestPort = 55555
)

var images = []string{"bzImage", "ramdisk.img", "updater.img", "system.img", "vendor.img", "userdata.img"}

// Request asks for an emulator of the Oniro image in a directory.
type Request struct {
	// Images is the directory that holds the Oniro image files.
	Images      string
	QEMU        string
	HDC         string
	LogDir      string
	BootTimeout time.Duration
}

// NewRequest finds the image directory, QEMU, and hdc in the environment.
func NewRequest() (*Request, error) {
	dir := os.Getenv("OHOS_EMULATOR_IMAGES")
	if dir == "" {
		return nil, errors.New("no OpenHarmony emulator image: set OHOS_EMULATOR_IMAGES to the directory with the Oniro image's bzImage and .img files")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("OHOS_EMULATOR_IMAGES: %w", err)
	}
	for _, name := range images {
		if !fileExists(filepath.Join(resolved, name)) {
			return nil, fmt.Errorf("OHOS_EMULATOR_IMAGES is %s, which has no %s", dir, name)
		}
	}
	qemu, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return nil, errors.New("qemu-system-x86_64 is not on PATH; install QEMU with your system package manager")
	}
	hdc, err := exec.LookPath("hdc")
	if err != nil {
		return nil, errors.New("hdc is not on PATH; it comes in the OpenHarmony SDK's toolchains directory")
	}
	return &Request{Images: resolved, QEMU: qemu, HDC: hdc}, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Kind implements devices.Request.
func (r *Request) Kind() string { return KindName }

// Key implements devices.Request.
func (r *Request) Key() string { return r.Images }

// Description implements devices.Request.
func (r *Request) Description() string { return "ohos " + filepath.Base(filepath.Dir(r.Images)) }

// Definition implements devices.Request. The image belongs to the project that
// installed it, so there is nothing to create.
func (r *Request) Definition() string { return "" }

// Prepare implements devices.Request.
func (r *Request) Prepare(context.Context) error { return nil }

// Start launches QEMU on the image with its hdc daemon forwarded to the first
// free host port.
func (r *Request) Start(_ context.Context, owner string, running []state.Instance) (state.Instance, error) {
	port := 0
	for p := firstPort; p <= lastPort; p++ {
		taken := slices.ContainsFunc(running, func(in state.Instance) bool { return in.Kind == KindName && in.Port == p })
		if !taken && process.Bindable(p) {
			port = p
			break
		}
	}
	if port == 0 {
		return state.Instance{}, fmt.Errorf("no free hdc port between %d and %d", firstPort, lastPort)
	}
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return state.Instance{}, err
	}
	logPath := filepath.Join(r.LogDir, fmt.Sprintf("ohos-%d.log", port))
	logFile, err := os.Create(logPath)
	if err != nil {
		return state.Instance{}, err
	}
	defer logFile.Close()

	cmd := exec.Command(r.QEMU, r.args(port)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own session keeps QEMU running after devices-cli exits and out of
	// the terminal's signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return state.Instance{}, fmt.Errorf("start QEMU: %w", err)
	}
	go func() { _ = cmd.Wait() }()

	now := time.Now()
	return state.Instance{
		Owner:       owner,
		Kind:        KindName,
		Key:         r.Key(),
		Description: r.Description(),
		ID:          connectKey(port),
		PID:         cmd.Process.Pid,
		Port:        port,
		Log:         logPath,
		Started:     now,
		LastUsed:    now,
	}, nil
}

func connectKey(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

// args are the QEMU flags for the Oniro image, as its run.sh boots it but
// headless. -snapshot keeps the guest's writes in temporary files, so
// instances share the image and never change it.
func (r *Request) args(port int) []string {
	args := []string{
		"-snapshot",
		"-machine", "q35",
		"-smp", "6",
		"-m", "4096M",
		"-boot", "c",
		"-vga", "none",
		"-device", "virtio-gpu-pci,xres=360,yres=720,max_outputs=1,addr=08.0",
		"-display", "none",
		"-serial", "stdio",
		"-monitor", "none",
		"-rtc", "base=utc,clock=host",
		"-initrd", filepath.Join(r.Images, "ramdisk.img"),
		"-kernel", filepath.Join(r.Images, "bzImage"),
	}
	for i, name := range []string{"updater", "system", "vendor", "userdata"} {
		args = append(args,
			"-drive", fmt.Sprintf("if=none,file=%s,format=raw,id=%s,index=%d", filepath.Join(r.Images, name+".img"), name, i),
			"-device", "virtio-blk-pci,drive="+name)
	}
	args = append(args,
		"-append", "ip=dhcp loglevel=4 console=ttyS0,115200 init=init root=/dev/ram0 rw ohos.boot.hardware=x86_general "+
			"ohos.required_mount.system=/dev/block/vdb@/usr@ext4@ro,barrier=1@wait,required "+
			"ohos.required_mount.vendor=/dev/block/vdc@/vendor@ext4@ro,barrier=1@wait,required "+
			"ohos.required_mount.misc=/dev/block/vda@/misc@none@none=@wait,required",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:%d", port, guestPort),
		"-device", "virtio-net-pci,netdev=net0",
	)
	// The image is x86_64, so only an x86_64 Linux host with KVM accelerates
	// it; elsewhere, including Apple silicon, QEMU emulates it.
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" && kvmAvailable() {
		return append(args, "-enable-kvm", "-cpu", "host")
	}
	return append(args, "-accel", "tcg,thread=multi", "-cpu", "max")
}

func kvmAvailable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// WaitReady waits until hdc connects to the instance and runs a shell command.
func (r *Request) WaitReady(ctx context.Context, inst state.Instance) error {
	deadline := time.Now().Add(r.BootTimeout)
	for {
		if !alive(inst) {
			return fmt.Errorf("OpenHarmony emulator %s exited before it booted%s", inst.ID, process.LogTail(inst.Log))
		}
		err := r.ready(ctx, inst.ID)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("OpenHarmony emulator %s did not answer hdc within %s (last: %w)%s",
				inst.ID, r.BootTimeout, err, process.LogTail(inst.Log))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *Request) ready(ctx context.Context, key string) error {
	if _, err := hdc(ctx, r.HDC, "tconn", key); err != nil {
		return fmt.Errorf("hdc tconn %s: %w", key, err)
	}
	out, err := hdc(ctx, r.HDC, "-t", key, "shell", "echo", "ready")
	if err != nil || out != "ready" {
		return fmt.Errorf("hdc -t %s shell echo ready: %w: %s", key, err, out)
	}
	return nil
}

// hdc runs an hdc command with a deadline, since a call to a guest that is
// still booting can block.
func hdc(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	// An hdc client that starts the hdc server can leave it holding the
	// output pipe; the client's own exit is what matters.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrWaitDelay):
		err = nil
	case errors.As(err, &exit):
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return strings.TrimSpace(strings.ReplaceAll(string(out), "\r", "")), err
}

// Kind manages OpenHarmony emulator instances.
type Kind struct{}

// Alive implements devices.Kind. It checks the process's command line, so a
// reused process ID does not count.
func (Kind) Alive(_ context.Context, inst state.Instance) bool { return alive(inst) }

func alive(inst state.Instance) bool {
	return process.Running(inst.PID, fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:", inst.Port))
}

// Stop implements devices.Kind. It disconnects hdc first, so the forwarded
// port is free for the next instance at once, then stops QEMU.
func (Kind) Stop(ctx context.Context, inst state.Instance) error {
	if hdcPath, err := exec.LookPath("hdc"); err == nil {
		_, _ = hdc(ctx, hdcPath, "tconn", inst.ID, "-remove")
	}
	if !alive(inst) {
		return nil
	}
	return process.StopGroup(inst.PID, 15*time.Second)
}

// RemoveDefinition implements devices.Kind. OpenHarmony emulators have no
// definitions.
func (Kind) RemoveDefinition(string) error { return nil }
