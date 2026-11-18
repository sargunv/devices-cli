package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/apple"
	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/devices"
	"github.com/sargunv/devices-cli/internal/freebsd"
	"github.com/sargunv/devices-cli/internal/harmonyos"
	"github.com/sargunv/devices-cli/internal/ohos"
	"github.com/sargunv/devices-cli/internal/owner"
	"github.com/sargunv/devices-cli/internal/wine"
)

type getFlags struct {
	owner       string
	wait        time.Duration
	bootTimeout time.Duration
}

func (flags *getFlags) bind(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&flags.owner, "owner", "", "directory that owns the device (default: the git worktree of the working directory)")
	cmd.PersistentFlags().DurationVar(&flags.wait, "wait", 10*time.Minute, "how long to wait for a free slot; 0 fails at once")
	cmd.PersistentFlags().DurationVar(&flags.bootTimeout, "boot-timeout", 10*time.Minute, "how long to wait for a new instance to boot")
}

// kindCommand is one kind's subcommand of get or exec: its flags, and the
// request they make.
type kindCommand struct {
	name    string // the subcommand's name, which is the kind's
	usage   string // the flags in the usage line, after the name
	id      string // what get prints, such as "adb serial"
	device  string // the device, such as "an Android emulator"
	long    string // how get finds or starts the device
	example string // get's examples
	// bind adds the kind's flags; request makes the request from them.
	bind    func(cmd *cobra.Command)
	request func(ctx context.Context, flags *getFlags) (devices.Request, error)
}

// kindCommands lists the kinds, in the order the help shows them. Each call
// returns fresh flag storage.
func (a *app) kindCommands() []kindCommand {
	kinds := []kindCommand{a.androidKind()}
	for _, platform := range apple.Platforms {
		kinds = append(kinds, appleKind(platform))
	}
	return append(kinds, ohosKind(), harmonyosKind(), freebsdKind(), wineKind())
}

func (a *app) getCommand() *cobra.Command {
	var flags getFlags
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Print the ID of a device for this worktree, starting one if needed",
		Long: `Print the ID of a device for this worktree on stdout, starting one if needed.

get reuses the worktree's running instance that matches the request, or starts
a new one and waits until it has booted. Progress goes to stderr. When every
slot is in use, get waits up to --wait for one to free up.`,
	}
	flags.bind(cmd)
	for _, kind := range a.kindCommands() {
		sub := &cobra.Command{
			Use:     kind.name + kind.usage,
			Short:   "Print the " + kind.id + " of " + kind.device,
			Long:    "Print the " + kind.id + " of " + kind.device + " for this worktree.\n\n" + kind.long,
			Example: kind.example,
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				req, err := kind.request(cmd.Context(), &flags)
				if err != nil {
					return err
				}
				_, id, err := a.device(cmd, &flags, req)
				if err != nil {
					return err
				}
				fmt.Fprintln(a.stdout, id)
				return nil
			},
		}
		kind.bind(sub)
		if kind.name == android.KindName {
			a.allowEmulatorArgs(sub)
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

// device resolves the owner and returns it with the ID of its device for the
// request, starting one if needed.
func (a *app) device(cmd *cobra.Command, flags *getFlags, req devices.Request) (string, string, error) {
	ownerPath, err := owner.Resolve(flags.owner)
	if err != nil {
		return "", "", err
	}
	m, err := a.manager()
	if err != nil {
		return "", "", err
	}
	id, err := m.Get(cmd.Context(), ownerPath, req, flags.wait)
	return ownerPath, id, err
}

// The emulator flags of "get android", which go after --.
func (a *app) allowEmulatorArgs(cmd *cobra.Command) {
	cmd.Use = "android --api <level> [flags] [-- <emulator flags>]"
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if dash := cmd.ArgsLenAtDash(); dash != 0 && len(args) > 0 {
			return fmt.Errorf("unexpected argument %q; pass emulator flags after --", args[0])
		}
		a.emulatorArgs = args
		return nil
	}
}

func (a *app) androidKind() kindCommand {
	req := &android.Request{Log: a.stderr}
	return kindCommand{
		name:   android.KindName,
		usage:  " --api <level> [flags]",
		id:     "adb serial",
		device: "an Android emulator or assigned device",
		long: `A physical device assigned to the worktree with "devices-cli assign" comes
first when it is connected and runs the API level and ABI. Otherwise devices-cli
uses the worktree's emulator for the system image, starting one that runs
until it is released, idle too long, or its worktree is gone.

Emulators run headless by default; --headful opens a window.
Each hardware profile has its own AVD. Emulators run read-only from one AVD per system image, which devices-cli
creates on first use, installing the image into $ANDROID_HOME if it is
missing. Flags after -- go to the emulator, and an instance only matches a
request with the same flags. devices-cli sets -avd, -port, -read-only,
-no-window (unless --headful), -no-audio, -no-boot-anim, -no-snapshot,
-no-metrics, and -crash-report-mode disabled.`,
		example: `  devices-cli get android --api 36
  devices-cli get android --api 34 --variant google_apis --abi x86_64
  devices-cli get android --api 36 -- -gpu swiftshader -memory 4096`,
		bind: func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&req.API, "api", "", "API level of the system image, such as 36 (required)")
			cmd.Flags().StringVar(&req.ABI, "abi", android.HostABI(), "ABI of the system image")
			cmd.Flags().StringVar(&req.Variant, "variant", "default", "system image variant, such as default, google_apis, or google_apis_playstore")
			cmd.Flags().BoolVar(&req.Headful, "headful", false, "open an emulator window (default: headless)")
			cmd.Flags().StringVar(&req.DeviceProfile, "device-profile", "", "avdmanager hardware profile ID, such as wearos_small_round or tv_1080p")
			_ = cmd.MarkFlagRequired("api")
		},
		request: func(_ context.Context, flags *getFlags) (devices.Request, error) {
			req.Args = a.emulatorArgs
			if err := req.Validate(); err != nil {
				return nil, err
			}
			sdk, err := android.FindSDK()
			if err != nil {
				return nil, err
			}
			req.SDK = sdk
			req.BootTimeout = flags.bootTimeout
			if req.LogDir, err = logDir(); err != nil {
				return nil, err
			}
			return req, nil
		},
	}
}

func appleKind(platform apple.Platform) kindCommand {
	var runtime, deviceType string
	return kindCommand{
		name:   platform.Kind,
		usage:  " [flags]",
		id:     "UDID",
		device: article(platform.Name) + " " + platform.Name + " simulator",
		long: fmt.Sprintf(`devices-cli uses the worktree's simulator for the runtime and device type,
or creates and boots a new one, named "devices-cli <worktree name>". The
simulator is deleted when it is released, idle too long, or its worktree is
gone. Runtimes come with Xcode; install more with
"xcodebuild -downloadPlatform %[1]s".`, platform.Name),
		example: fmt.Sprintf(`  devices-cli get %[1]s
  devices-cli get %[1]s --runtime 26.4`, platform.Kind),
		bind: func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&runtime, "runtime", "", platform.Name+" runtime version, such as 26 or 26.4 (default: the newest installed)")
			cmd.Flags().StringVar(&deviceType, "device-type", "", "device type name or identifier (default: the runtime's first "+platform.Family+")")
		},
		request: func(ctx context.Context, flags *getFlags) (devices.Request, error) {
			req, err := apple.NewRequest(ctx, platform, runtime, deviceType)
			if err != nil {
				return nil, err
			}
			req.BootTimeout = flags.bootTimeout
			return req, nil
		},
	}
}

func ohosKind() kindCommand {
	return kindCommand{
		name:   ohos.KindName,
		usage:  "",
		id:     "hdc connect key",
		device: "an OpenHarmony emulator",
		long: `devices-cli uses the worktree's emulator, or starts QEMU on the Oniro emulator
image in $OHOS_EMULATOR_IMAGES and waits until an application can install and
launch in it: the system has booted, user 100 is active, and the screen is
unlocked. The emulator runs with -snapshot, so instances share the image and
never change it. qemu-system-x86_64 and hdc must be on PATH. QEMU uses KVM on
an x86_64 Linux host that has it and emulates the machine elsewhere.`,
		example: `  hdc -t "$(devices-cli get ohos)" shell uname -a`,
		bind:    func(*cobra.Command) {},
		request: func(_ context.Context, flags *getFlags) (devices.Request, error) {
			req, err := ohos.NewRequest()
			if err != nil {
				return nil, err
			}
			req.BootTimeout = flags.bootTimeout
			if req.LogDir, err = logDir(); err != nil {
				return nil, err
			}
			return req, nil
		},
	}
}

func harmonyosKind() kindCommand {
	var deviceType, osVersion string
	return kindCommand{
		name:   harmonyos.KindName,
		usage:  " [flags]",
		id:     "hdc connect key",
		device: "a HarmonyOS emulator",
		long: `devices-cli uses the worktree's emulator, or creates one from a system image
that DevEco Studio downloaded and starts it, and waits until the system has
booted. Each worktree's emulator has its own data, and is deleted when it is
released, idle too long, or its worktree is gone. The Emulator comes from
DevEco Studio on macOS, or from HARMONYOS_EMULATOR; hdc from DevEco Studio,
else PATH. The emulator opens a window.`,
		example: `  hdc -t "$(devices-cli get harmonyos)" shell uname -a
  devices-cli get harmonyos --device-type tablet --os-version 6.0.2`,
		bind: func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&deviceType, "device-type", "phone", "device type of the system image, such as phone, tablet, or foldable")
			cmd.Flags().StringVar(&osVersion, "os-version", "", "system image version, or part of it, such as 6.0.2 (default: the newest downloaded)")
		},
		request: func(ctx context.Context, flags *getFlags) (devices.Request, error) {
			req, err := harmonyos.NewRequest(ctx, deviceType, osVersion)
			if err != nil {
				return nil, err
			}
			stateDir, err := config.StateDir()
			if err != nil {
				return nil, err
			}
			req.Root = filepath.Join(stateDir, "harmonyos")
			if req.LogDir, err = logDir(); err != nil {
				return nil, err
			}
			req.BootTimeout = flags.bootTimeout
			return req, nil
		},
	}
}

func freebsdKind() kindCommand {
	var release string
	return kindCommand{
		name:   freebsd.KindName,
		usage:  " --release <version>",
		id:     "SSH host",
		device: "a FreeBSD virtual machine",
		long: `devices-cli uses the worktree's VM, or boots one with anyvm and waits until it
answers SSH. The worktree is mounted in the VM over NFS at its own path, so
files keep their paths, and "ssh <host> <command>" runs a command there. The
VM boots anyvm's cached image with --snapshot, so VMs share the image and a
VM's changes go away with it. Release and collection shut the VM down with
"shutdown -p now".

anyvm comes from ANYVM, the path of anyvm.py, or from PATH. It needs QEMU and
UEFI firmware, and devices-cli runs FreeBSD VMs only on a Linux host with KVM.`,
		example: `  ssh "$(devices-cli get freebsd --release 14.4)" uname -a`,
		bind: func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&release, "release", "", "FreeBSD release, such as 14.4 (required)")
			_ = cmd.MarkFlagRequired("release")
		},
		request: func(_ context.Context, flags *getFlags) (devices.Request, error) {
			req, err := freebsd.NewRequest(release)
			if err != nil {
				return nil, err
			}
			stateDir, err := config.StateDir()
			if err != nil {
				return nil, err
			}
			cacheDir, err := config.CacheDir()
			if err != nil {
				return nil, err
			}
			req.VMDir = filepath.Join(stateDir, "vms")
			req.CacheDir = filepath.Join(cacheDir, "anyvm")
			if req.LogDir, err = logDir(); err != nil {
				return nil, err
			}
			req.BootTimeout = flags.bootTimeout
			return req, nil
		},
	}
}

func wineKind() kindCommand {
	var dlls []string
	return kindCommand{
		name:   wine.KindName,
		usage:  " [--dll <path>]...",
		id:     "WINEPREFIX",
		device: "a Wine prefix",
		long: `devices-cli uses the worktree's prefix for the DLLs, or creates one in its state
directory and boots Wine in it. Each --dll names a 64-bit DLL, or a directory
of them, that devices-cli copies into the prefix's system32 directory and has
Wine load instead of its own, such as vkd3d-proton's d3d12.dll; a prefix only
serves requests for the same DLLs. The prefix is deleted when it is released,
idle too long, or its worktree is gone. wine and wineserver must be on PATH.`,
		example: `  WINEPREFIX="$(devices-cli get wine)" wine ./app.exe
  devices-cli get wine --dll ~/vkd3d-proton/x64`,
		bind: func(cmd *cobra.Command) {
			cmd.Flags().StringArrayVar(&dlls, "dll", nil, "DLL, or directory of DLLs, to load instead of Wine's own (repeatable)")
		},
		request: func(_ context.Context, flags *getFlags) (devices.Request, error) {
			req, err := wine.NewRequest(dlls)
			if err != nil {
				return nil, err
			}
			stateDir, err := config.StateDir()
			if err != nil {
				return nil, err
			}
			req.Root = filepath.Join(stateDir, "wine")
			req.BootTimeout = flags.bootTimeout
			return req, nil
		},
	}
}

// article returns the indefinite article for a platform name: "an iOS", "a
// tvOS".
func article(name string) string {
	if strings.ContainsRune("AEIOUaeiou", rune(name[0])) {
		return "an"
	}
	return "a"
}
