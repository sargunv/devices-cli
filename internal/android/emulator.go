package android

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// KindName is the kind of every Android instance and pin.
const KindName = "android"

// The console port is even and adb takes the next one. Emulators outside adb's
// scan range of 5554 to 5585 still register with the adb server.
const (
	firstPort = 5554
	lastPort  = 5682
)

// Request asks for an emulator that runs one system image.
type Request struct {
	SDK SDK
	// API is the API level, such as "36" or "36.1".
	API string
	// Variant is the system image tag, such as "default" or "google_apis".
	Variant string
	// ABI is the system image ABI, such as "arm64-v8a".
	ABI string
	// DeviceProfile is an avdmanager hardware profile, such as wearos_small_round.
	DeviceProfile string
	// Headful opens an emulator window. The default is headless.
	Headful bool
	// Args are extra emulator flags.
	Args []string
	// LogDir receives one log file per emulator.
	LogDir      string
	BootTimeout time.Duration
	// Log receives installer output.
	Log io.Writer
}

var (
	apiPattern     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	variantPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// HostABI returns the system image ABI that runs natively on this machine.
func HostABI() string {
	if runtime.GOARCH == "arm64" {
		return "arm64-v8a"
	}
	return "x86_64"
}

// Validate checks the request's fields.
func (r *Request) Validate() error {
	if !apiPattern.MatchString(r.API) {
		return fmt.Errorf("--api %q is not an API level such as 36 or 36.1", r.API)
	}
	if !variantPattern.MatchString(r.Variant) {
		return fmt.Errorf("--variant %q is not a system image tag such as default or google_apis", r.Variant)
	}
	if _, ok := cpuArch[r.ABI]; !ok {
		return fmt.Errorf("--abi %q is not one of arm64-v8a, x86_64, x86, or armeabi-v7a", r.ABI)
	}
	if r.DeviceProfile != "" && !variantPattern.MatchString(r.DeviceProfile) {
		return fmt.Errorf("--device-profile %q must be an avdmanager profile ID such as wearos_small_round", r.DeviceProfile)
	}
	for _, arg := range r.Args {
		if arg == "-avd" || arg == "-port" || arg == "-ports" || arg == "-read-only" || arg == "-no-window" || arg == "-qt-hide-window" || strings.HasPrefix(arg, "@") {
			return fmt.Errorf("emulator flag %s is set by devices-cli and cannot be passed", arg)
		}
	}
	return nil
}

// Kind implements devices.Request.
func (r *Request) Kind() string { return KindName }

// Definition implements devices.Request.
func (r *Request) Definition() string {
	name := definitionPrefix + strings.Join([]string{"android", r.API, r.Variant, r.ABI}, "-")
	if r.DeviceProfile != "" {
		name += "-profile-" + r.DeviceProfile
	}
	return name
}

// Key implements devices.Request.
func (r *Request) Key() string {
	mode := "headless"
	if r.Headful {
		mode = "headful"
	}
	return strings.Join(append([]string{r.Definition(), mode}, r.Args...), " ")
}

// Description implements devices.Request.
func (r *Request) Description() string {
	desc := fmt.Sprintf("android-%s %s %s", r.API, r.Variant, r.ABI)
	if r.DeviceProfile != "" {
		desc += " " + r.DeviceProfile
	}
	if r.Headful {
		desc += " headful"
	}
	if len(r.Args) > 0 {
		desc += " " + strings.Join(r.Args, " ")
	}
	return desc
}

// Prepare installs missing SDK packages, writes the AVD, and primes a new AVD.
func (r *Request) Prepare(ctx context.Context) error {
	var missing [][]string
	for _, pkg := range [][]string{
		{"platform-tools"},
		{"emulator"},
		imagePackage(r.API, r.Variant, r.ABI),
	} {
		if !r.SDK.has(pkg) {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(r.Log, "devices-cli: installing %d Android SDK package(s) into %s\n", len(missing), r.SDK.Root)
		if err := r.SDK.install(ctx, r.Log, missing); err != nil {
			return err
		}
	}
	if err := r.writeDefinition(ctx); err != nil {
		return err
	}
	marker, err := primedMarker(r.Definition())
	if err != nil || fileExists(marker) {
		return err
	}
	if err := r.prime(ctx); err != nil {
		return err
	}
	return os.WriteFile(marker, nil, 0o644)
}

// prime boots a new AVD once and stops it. The first instance of an AVD
// creates its disk images and keeps them locked for as long as it runs, and on
// Linux every other instance then fails to start, so that first instance must
// not be one that an owner uses. Its ports lie above the instances' range, so
// it needs no state lock.
func (r *Request) prime(ctx context.Context) error {
	port := 0
	for p := lastPort + 2; p <= lastPort+100; p += 2 {
		if process.Bindable(p) && process.Bindable(p+1) {
			port = p
			break
		}
	}
	if port == 0 {
		return fmt.Errorf("no free emulator console port above %d to set up %s", lastPort, r.Definition())
	}
	fmt.Fprintf(r.Log, "devices-cli: booting new AVD %s once to set it up\n", r.Definition())
	inst, err := r.launch(port, filepath.Join(r.LogDir, "prime-"+r.Definition()+".log"), nil, false)
	if err != nil {
		return err
	}
	// Also stops the emulator when devices-cli is interrupted.
	defer func() { _ = Kind{}.Stop(context.WithoutCancel(ctx), inst) }()
	return r.WaitReady(ctx, inst)
}

// Start launches a read-only emulator on the first free console port.
func (r *Request) Start(_ context.Context, owner string, running []state.Instance) (state.Instance, error) {
	port, err := freePort(running)
	if err != nil {
		return state.Instance{}, err
	}
	inst, err := r.launch(port, filepath.Join(r.LogDir, fmt.Sprintf("emulator-%d.log", port)), r.Args, r.Headful)
	if err != nil {
		return state.Instance{}, err
	}
	now := time.Now()
	inst.Owner = owner
	inst.Key = r.Key()
	inst.Description = r.Description()
	inst.Started = now
	inst.LastUsed = now
	return inst, nil
}

// launch starts a read-only emulator of the AVD on a console port, logging to
// logPath, which it replaces.
func (r *Request) launch(port int, logPath string, extra []string, headful bool) (state.Instance, error) {
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return state.Instance{}, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return state.Instance{}, err
	}
	defer logFile.Close()

	// A read-only instance keeps its writes in temporary files, so instances
	// of one AVD run side by side and none of them changes it.
	args := []string{
		"-avd", r.Definition(),
		"-read-only",
		"-port", strconv.Itoa(port),
		"-no-audio",
		"-no-boot-anim",
		"-no-snapshot",
		"-no-metrics",
	}
	if !headful {
		args = append(args, "-no-window")
	}
	cmd := exec.Command(r.SDK.emulatorPath(), append(args, extra...)...)
	cmd.Env = append(os.Environ(), "ANDROID_HOME="+r.SDK.Root, "ANDROID_SDK_ROOT="+r.SDK.Root, "ADB_MDNS=0")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own session keeps the emulator running after devices-cli exits and
	// out of the terminal's signals, and makes it a process group to stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return state.Instance{}, fmt.Errorf("start emulator: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return state.Instance{
		Kind:       KindName,
		Definition: r.Definition(),
		ID:         fmt.Sprintf("emulator-%d", port),
		PID:        cmd.Process.Pid,
		Port:       port,
		Log:        logPath,
	}, nil
}

func freePort(running []state.Instance) (int, error) {
	for port := firstPort; port <= lastPort; port += 2 {
		taken := slices.ContainsFunc(running, func(in state.Instance) bool { return in.Port == port })
		if !taken && process.Bindable(port) && process.Bindable(port+1) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free emulator console port between %d and %d", firstPort, lastPort)
}

// WaitReady waits until the emulator reports sys.boot_completed.
func (r *Request) WaitReady(ctx context.Context, inst state.Instance) error {
	deadline := time.Now().Add(r.BootTimeout)
	for {
		if !alive(inst) {
			return fmt.Errorf("emulator %s exited before it booted%s", inst.ID, process.LogTail(inst.Log))
		}
		if out, err := r.SDK.adb(ctx, "-s", inst.ID, "shell", "getprop", "sys.boot_completed"); err == nil && out == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("emulator %s did not boot within %s%s", inst.ID, r.BootTimeout, process.LogTail(inst.Log))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// MatchPin checks that a pinned physical device is connected and runs the
// requested API level and ABI. The image variant does not apply to it.
func (r *Request) MatchPin(ctx context.Context, serial string) error {
	if r.DeviceProfile != "" {
		return errors.New("a hardware profile requires an emulator")
	}
	if err := r.SDK.Connected(ctx, serial); err != nil {
		return errors.New("it is not connected")
	}
	sdk, err := r.SDK.adb(ctx, "-s", serial, "shell", "getprop", "ro.build.version.sdk")
	if err != nil {
		return fmt.Errorf("read its API level: %w", err)
	}
	if want, _, minor := strings.Cut(r.API, "."); sdk != want {
		return fmt.Errorf("it runs API %s, not %s", sdk, r.API)
	} else if minor {
		// Minor API levels such as 36.1 appear only in sdk_full.
		full, err := r.SDK.adb(ctx, "-s", serial, "shell", "getprop", "ro.build.version.sdk_full")
		if err != nil {
			return fmt.Errorf("read its API level: %w", err)
		}
		if full != r.API {
			return fmt.Errorf("it runs API %s, not %s", full, r.API)
		}
	}
	abis, err := r.SDK.adb(ctx, "-s", serial, "shell", "getprop", "ro.product.cpu.abilist")
	if err != nil {
		return fmt.Errorf("read its ABIs: %w", err)
	}
	if !slices.Contains(strings.Split(abis, ","), r.ABI) {
		return fmt.Errorf("it supports %s, not %s", abis, r.ABI)
	}
	return nil
}

// Connected checks that adb lists the serial as a device it can use.
func (s SDK) Connected(ctx context.Context, serial string) error {
	state, err := s.adb(ctx, "-s", serial, "get-state")
	if err != nil || state != "device" {
		return fmt.Errorf("adb does not list %s as a connected device; check `adb devices`", serial)
	}
	return nil
}

// Kind manages Android emulator instances and AVD definitions.
type Kind struct{}

// Alive implements devices.Kind. It checks the process's command line, so a
// reused process ID does not count.
func (Kind) Alive(_ context.Context, inst state.Instance) bool { return alive(inst) }

func alive(inst state.Instance) bool {
	return process.Running(inst.PID, " -avd "+inst.Definition+" ", " -port "+strconv.Itoa(inst.Port)+" ")
}

// Stop implements devices.Kind. It signals the emulator's process group, which
// also holds the helpers it starts. The log stays for diagnosis until the next
// emulator on its port.
func (Kind) Stop(_ context.Context, inst state.Instance) error {
	if !alive(inst) {
		return nil
	}
	return process.StopGroup(inst.PID, 15*time.Second)
}
