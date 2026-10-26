// Package apple gives each owner its own Apple simulator, created with simctl
// and deleted when the owner releases it.
package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sargunv/devices-cli/internal/state"
)

// Platform is a simulator platform and the device family it defaults to.
type Platform struct {
	// Kind is the devices-cli kind, such as "ios".
	Kind string
	// Name is the simctl platform, such as "iOS".
	Name string
	// Family is the device family to pick when no device type is named.
	Family string
}

// Platforms lists the simulator kinds.
var Platforms = []Platform{
	{Kind: "ios", Name: "iOS", Family: "iPhone"},
	{Kind: "tvos", Name: "tvOS", Family: "Apple TV"},
	{Kind: "visionos", Name: "visionOS", Family: "Apple Vision"},
	{Kind: "watchos", Name: "watchOS", Family: "Apple Watch"},
}

// namePrefix marks the simulators that devices-cli owns; it never touches
// others.
const namePrefix = "devices-cli "

// Request asks for a simulator of one device type on one runtime.
type Request struct {
	Platform    Platform
	Runtime     runtime
	DeviceType  deviceType
	BootTimeout time.Duration
}

type runtime struct {
	Identifier  string       `json:"identifier"`
	Version     string       `json:"version"`
	Platform    string       `json:"platform"`
	IsAvailable bool         `json:"isAvailable"`
	DeviceTypes []deviceType `json:"supportedDeviceTypes"`
}

type deviceType struct {
	Identifier    string `json:"identifier"`
	Name          string `json:"name"`
	ProductFamily string `json:"productFamily"`
}

type device struct {
	UDID  string `json:"udid"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func simctl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return out, fmt.Errorf("simctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
		}
		return out, fmt.Errorf("simctl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// NewRequest resolves a runtime version and device type for the platform. An
// empty version picks the newest available runtime; an empty device type picks
// the runtime's first device type of the platform's family. A version matches
// a runtime whose version equals it or starts with it and a dot.
func NewRequest(ctx context.Context, platform Platform, version, deviceTypeName string) (*Request, error) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		return nil, errors.New("xcrun is not on PATH: Apple simulators need Xcode on macOS")
	}
	out, err := simctl(ctx, "list", "runtimes", "--json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Runtimes []runtime `json:"runtimes"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("read simctl runtimes: %w", err)
	}

	var candidates []runtime
	var versions []string
	for _, rt := range list.Runtimes {
		if rt.Platform != platform.Name || !rt.IsAvailable {
			continue
		}
		versions = append(versions, rt.Version)
		if version == "" || rt.Version == version || strings.HasPrefix(rt.Version, version+".") {
			candidates = append(candidates, rt)
		}
	}
	if len(candidates) == 0 {
		have := "none"
		if len(versions) > 0 {
			have = strings.Join(versions, ", ")
		}
		return nil, fmt.Errorf("no %s simulator runtime matches %q (installed: %s); install one with `xcodebuild -downloadPlatform %s`",
			platform.Name, version, have, platform.Name)
	}
	rt := slices.MaxFunc(candidates, func(a, b runtime) int { return compareVersions(a.Version, b.Version) })

	i := slices.IndexFunc(rt.DeviceTypes, func(dt deviceType) bool {
		if deviceTypeName == "" {
			return dt.ProductFamily == platform.Family
		}
		return dt.Name == deviceTypeName || dt.Identifier == deviceTypeName
	})
	if i < 0 {
		var names []string
		for _, dt := range rt.DeviceTypes {
			names = append(names, dt.Name)
		}
		return nil, fmt.Errorf("%s %s has no device type %q; it supports: %s",
			platform.Name, rt.Version, deviceTypeName, strings.Join(names, ", "))
	}
	return &Request{Platform: platform, Runtime: rt, DeviceType: rt.DeviceTypes[i]}, nil
}

func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// Kind implements devices.Request.
func (r *Request) Kind() string { return r.Platform.Kind }

// Key implements devices.Request.
func (r *Request) Key() string { return r.Runtime.Identifier + " " + r.DeviceType.Identifier }

// Description implements devices.Request.
func (r *Request) Description() string {
	return fmt.Sprintf("%s %s %s", r.DeviceType.Name, r.Platform.Name, r.Runtime.Version)
}

// Definition implements devices.Request. Simulators have no shared definition;
// each owner's simulator is its own.
func (r *Request) Definition() string { return "" }

// Prepare implements devices.Request; NewRequest already found the runtime.
func (r *Request) Prepare(context.Context) (bool, error) { return false, nil }

// Start creates the owner's simulator and boots it. The simulator is booted
// before Start returns, so other commands see it as alive.
func (r *Request) Start(ctx context.Context, owner string, _ []state.Instance) (state.Instance, error) {
	out, err := simctl(ctx, "create", namePrefix+filepath.Base(owner), r.DeviceType.Identifier, r.Runtime.Identifier)
	if err != nil {
		return state.Instance{}, err
	}
	udid := strings.TrimSpace(string(out))
	if _, err := simctl(ctx, "boot", udid); err != nil {
		_, _ = simctl(context.WithoutCancel(ctx), "delete", udid)
		return state.Instance{}, err
	}
	now := time.Now()
	return state.Instance{
		Owner:       owner,
		Kind:        r.Platform.Kind,
		Key:         r.Key(),
		Description: r.Description(),
		ID:          udid,
		Started:     now,
		LastUsed:    now,
	}, nil
}

// WaitReady waits until the simulator has finished booting.
func (r *Request) WaitReady(ctx context.Context, inst state.Instance) error {
	ctx, cancel := context.WithTimeout(ctx, r.BootTimeout)
	defer cancel()
	if _, err := simctl(ctx, "bootstatus", inst.ID); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("simulator %s did not boot within %s", inst.ID, r.BootTimeout)
		}
		return err
	}
	return nil
}

// Kind manages Apple simulator instances.
type Kind struct{}

func find(ctx context.Context, udid string) (*device, error) {
	out, err := simctl(ctx, "list", "devices", "--json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Devices map[string][]device `json:"devices"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("read simctl devices: %w", err)
	}
	for _, devices := range list.Devices {
		for _, d := range devices {
			if d.UDID == udid {
				return &d, nil
			}
		}
	}
	return nil, nil
}

// Alive implements devices.Kind: the simulator exists and is booted.
func (Kind) Alive(ctx context.Context, inst state.Instance) bool {
	d, err := find(ctx, inst.ID)
	return err == nil && d != nil && (d.State == "Booted" || d.State == "Booting")
}

// Stop implements devices.Kind: it shuts the simulator down and deletes it.
func (Kind) Stop(ctx context.Context, inst state.Instance) error {
	d, err := find(ctx, inst.ID)
	if err != nil {
		return err
	}
	if d == nil {
		return nil
	}
	if !strings.HasPrefix(d.Name, namePrefix) {
		return fmt.Errorf("refusing to delete simulator %s (%s), which devices-cli did not create", d.UDID, d.Name)
	}
	if d.State != "Shutdown" {
		if _, err := simctl(ctx, "shutdown", d.UDID); err != nil {
			return err
		}
	}
	_, err = simctl(ctx, "delete", d.UDID)
	return err
}

// RemoveDefinition implements devices.Kind. Simulators have no definitions.
func (Kind) RemoveDefinition(string) error { return nil }
