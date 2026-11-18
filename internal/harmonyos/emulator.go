// Package harmonyos runs HarmonyOS emulators: one instance of a DevEco Studio
// system image per owner, created with DevEco's Emulator command line and
// reached through hdc.
package harmonyos

import (
	"context"
	"encoding/json"
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

	"github.com/sargunv/devices-cli/internal/ohos"
	"github.com/sargunv/devices-cli/internal/process"
	"github.com/sargunv/devices-cli/internal/state"
)

// KindName is the kind of every HarmonyOS instance.
const KindName = "harmonyos"

// The host ports that each instance's hdc daemon listens on.
const (
	firstPort = 15600
	lastPort  = 15799
)

// namePrefix marks the instances that devices-cli owns. Emulator names allow
// letters, digits, spaces, underscores and plus signs.
const namePrefix = "devices_cli_"

// defaultEmulator is where DevEco Studio keeps the Emulator command line on
// macOS.
const defaultEmulator = "/Applications/DevEco-Studio.app/Contents/tools/emulator/Emulator"

// defaultHDC is DevEco Studio's hdc, which the emulators use.
const defaultHDC = "/Applications/DevEco-Studio.app/Contents/sdk/default/openharmony/toolchains/hdc"

// pollInterval separates readiness checks while an instance boots.
var pollInterval = 2 * time.Second

// Request asks for an emulator of one device type and system image.
type Request struct {
	Emulator   string
	HDC        string
	DeviceType string
	// OSVersion is the image's version as the Emulator names it, such as
	// "HarmonyOS 6.0.2(22)".
	OSVersion string
	// Root holds the instances that devices-cli creates.
	Root        string
	LogDir      string
	BootTimeout time.Duration
}

// FindEmulator returns DevEco Studio's Emulator command line: the one
// HARMONYOS_EMULATOR names, else DevEco Studio's on macOS.
func FindEmulator() (string, error) {
	if path := os.Getenv("HARMONYOS_EMULATOR"); path != "" {
		if !fileExists(path) {
			return "", fmt.Errorf("HARMONYOS_EMULATOR is %s, which does not exist", path)
		}
		return path, nil
	}
	if runtime.GOOS != "darwin" {
		return "", errors.New("HarmonyOS emulators run only on macOS, where DevEco Studio has them; set HARMONYOS_EMULATOR to DevEco's Emulator to use another location")
	}
	if !fileExists(defaultEmulator) {
		return "", fmt.Errorf("no HarmonyOS emulator at %s: install DevEco Studio, or set HARMONYOS_EMULATOR to its tools/emulator/Emulator", defaultEmulator)
	}
	return defaultEmulator, nil
}

// NewRequest finds the Emulator, hdc, and a downloaded image of the device
// type. An empty osVersion picks the newest downloaded image; another matches
// an image whose version contains it, such as "6.0.2" or "(22)".
func NewRequest(ctx context.Context, deviceType, osVersion string) (*Request, error) {
	emulator, err := FindEmulator()
	if err != nil {
		return nil, err
	}
	// DevEco Studio's hdc matches its emulator; hdc clients of other versions
	// restart each other's server.
	hdc := defaultHDC
	if !fileExists(hdc) {
		if hdc, err = exec.LookPath("hdc"); err != nil {
			return nil, errors.New("hdc is not on PATH; it comes in the OpenHarmony SDK's toolchains directory")
		}
	}
	version, err := pickImage(ctx, emulator, deviceType, osVersion)
	if err != nil {
		return nil, err
	}
	return &Request{Emulator: emulator, HDC: hdc, DeviceType: deviceType, OSVersion: version}, nil
}

type image struct {
	DeviceType string `json:"deviceType"`
	OSVersion  string `json:"osVersion"`
	Downloaded string `json:"downloaded"`
}

func pickImage(ctx context.Context, emulator, deviceType, want string) (string, error) {
	out, err := exec.CommandContext(ctx, emulator, "-imageList", "-deviceType", deviceType, "-downloaded", "true").Output()
	if err != nil {
		return "", fmt.Errorf("list HarmonyOS images: %w", err)
	}
	var images []image
	if err := json.Unmarshal(out, &images); err != nil {
		return "", fmt.Errorf("read the HarmonyOS image list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var versions []string
	for _, im := range images {
		if im.DeviceType == deviceType && im.Downloaded == "true" && strings.Contains(im.OSVersion, want) {
			versions = append(versions, im.OSVersion)
		}
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("no downloaded HarmonyOS %s image matches %q; download one in DevEco Studio's Device Manager", deviceType, want)
	}
	slices.SortFunc(versions, compareVersions)
	return versions[len(versions)-1], nil
}

// compareVersions orders image versions such as "HarmonyOS 6.0.2(22)" by
// their numbers.
func compareVersions(a, b string) int {
	na, nb := numbers(a), numbers(b)
	return slices.Compare(na, nb)
}

func numbers(s string) []int {
	var out []int
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		n, _ := strconv.Atoi(field)
		out = append(out, n)
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Kind implements devices.Request.
func (r *Request) Kind() string { return KindName }

// Key implements devices.Request.
func (r *Request) Key() string { return r.DeviceType + " " + r.OSVersion }

// Description implements devices.Request.
func (r *Request) Description() string { return r.OSVersion + " " + r.DeviceType }

// Definition implements devices.Request. Each instance is created for its
// owner, which takes no time, so there is no definition.
func (r *Request) Definition() string { return "" }

// Prepare implements devices.Request.
func (r *Request) Prepare(context.Context) error { return nil }

func instanceName(port int) string { return namePrefix + strconv.Itoa(port) }

// Start creates an instance named for the first free hdc port and starts it.
func (r *Request) Start(ctx context.Context, owner string, running []state.Instance) (state.Instance, error) {
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
	name := instanceName(port)
	// A leftover of an instance that devices-cli lost track of; -create fails
	// on any it could not remove.
	_ = removeInstance(r.Root, name)
	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		return state.Instance{}, err
	}
	out, err := exec.CommandContext(ctx, r.Emulator, "-create", name, "-deviceType", r.DeviceType, "-osVersion", r.OSVersion, "-instancePath", r.Root).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "success") {
		return state.Instance{}, fmt.Errorf("create a HarmonyOS emulator: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return state.Instance{}, err
	}
	logPath := filepath.Join(r.LogDir, "harmonyos-"+strconv.Itoa(port)+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return state.Instance{}, err
	}
	defer logFile.Close()
	cmd := exec.Command(r.Emulator, "-hvd", name, "-path", r.Root, "-hdcPort", strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Its own session keeps the emulator running after devices-cli exits and
	// out of the terminal's signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = removeInstance(r.Root, name)
		return state.Instance{}, fmt.Errorf("start the HarmonyOS emulator: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	now := time.Now()
	return state.Instance{
		Owner:       owner,
		Kind:        KindName,
		Key:         r.Key(),
		Description: r.Description(),
		ID:          "127.0.0.1:" + strconv.Itoa(port),
		PID:         cmd.Process.Pid,
		Port:        port,
		Log:         logPath,
		Started:     now,
		LastUsed:    now,
	}, nil
}

// WaitReady waits until hdc runs shell commands on the instance and the
// system has finished booting.
func (r *Request) WaitReady(ctx context.Context, inst state.Instance) error {
	deadline := time.Now().Add(r.BootTimeout)
	for {
		if !alive(inst) {
			return fmt.Errorf("HarmonyOS emulator %s exited before it booted%s", inst.ID, process.LogTail(inst.Log))
		}
		out, err := ohos.HDC(ctx, r.HDC, "-t", inst.ID, "shell", "param", "get", "bootevent.boot.completed")
		if err == nil && out == "true" {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("the system has not finished booting (bootevent.boot.completed: %s)", out)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("HarmonyOS emulator %s was not ready within %s (last: %w); pass a longer --boot-timeout if the machine is busy%s",
				inst.ID, r.BootTimeout, err, process.LogTail(inst.Log))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Kind manages HarmonyOS emulator instances. Root is the directory that holds
// the instances, which Stop removes; Emulator, when set, stops an instance
// before its process is signalled.
type Kind struct {
	Root     string
	Emulator string
}

// Alive implements devices.Kind. It checks the command lines in the
// instance's process group for its name, so a reused process ID does not
// count, and neither does a group without the emulator.
func (Kind) Alive(_ context.Context, inst state.Instance) bool { return alive(inst) }

func alive(inst state.Instance) bool {
	return process.GroupRunning(inst.PID, instanceName(inst.Port))
}

// Stop implements devices.Kind. It asks the Emulator to stop the instance,
// signals the instance's process group if any of the emulator is left, and
// removes the instance's files once none is.
func (k Kind) Stop(ctx context.Context, inst state.Instance) error {
	name := instanceName(inst.Port)
	if alive(inst) && k.Emulator != "" {
		_ = exec.CommandContext(ctx, k.Emulator, "-stop", name).Run()
		for deadline := time.Now().Add(15 * time.Second); alive(inst) && time.Now().Before(deadline); {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if alive(inst) {
		if err := process.StopGroup(inst.PID, 15*time.Second); err != nil {
			return err
		}
	}
	// An error keeps the instance recorded, so collection retries.
	return removeInstance(k.Root, name)
}

// removeInstance deletes an instance's files: its directory and the .ini that
// names it.
func removeInstance(root, name string) error {
	if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
		return fmt.Errorf("remove HarmonyOS emulator %s: %w", name, err)
	}
	if err := os.Remove(filepath.Join(root, name+".ini")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove HarmonyOS emulator %s: %w", name, err)
	}
	return nil
}

// RemoveDefinition implements devices.Kind. HarmonyOS emulators have no
// definitions.
func (Kind) RemoveDefinition(string) error { return nil }
