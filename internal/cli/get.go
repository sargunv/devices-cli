package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/apple"
	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/devices"
	"github.com/sargunv/devices-cli/internal/freebsd"
	"github.com/sargunv/devices-cli/internal/ohos"
	"github.com/sargunv/devices-cli/internal/owner"
)

type getFlags struct {
	owner       string
	wait        time.Duration
	bootTimeout time.Duration
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
	cmd.PersistentFlags().StringVar(&flags.owner, "owner", "", "directory that owns the device (default: the git worktree of the working directory)")
	cmd.PersistentFlags().DurationVar(&flags.wait, "wait", 10*time.Minute, "how long to wait for a free slot; 0 fails at once")
	cmd.PersistentFlags().DurationVar(&flags.bootTimeout, "boot-timeout", 10*time.Minute, "how long to wait for a new instance to boot")
	cmd.AddCommand(a.getAndroidCommand(&flags))
	for _, platform := range apple.Platforms {
		cmd.AddCommand(a.getAppleCommand(&flags, platform))
	}
	cmd.AddCommand(a.getOHOSCommand(&flags))
	cmd.AddCommand(a.getFreeBSDCommand(&flags))
	return cmd
}

func (a *app) getFreeBSDCommand(flags *getFlags) *cobra.Command {
	var release string
	cmd := &cobra.Command{
		Use:   "freebsd --release <version>",
		Short: "Print the SSH host of a FreeBSD virtual machine",
		Long: `Print the SSH host of a FreeBSD virtual machine for this worktree.

get returns the worktree's VM, or boots one with anyvm and waits until it
answers SSH. The worktree is mounted in the VM over NFS at its own path, so
files keep their paths, and "ssh <host> <command>" runs a command there. The
VM boots anyvm's cached image with --snapshot, so VMs share the image and a
VM's changes go away with it. Release and collection shut the VM down with
"shutdown -p now".

anyvm comes from ANYVM, the path of anyvm.py, or from PATH. It needs QEMU and
UEFI firmware, and devices-cli runs FreeBSD VMs only on a Linux host with KVM.`,
		Example: `  ssh "$(devices-cli get freebsd --release 14.4)" uname -a`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := freebsd.NewRequest(release)
			if err != nil {
				return err
			}
			stateDir, err := config.StateDir()
			if err != nil {
				return err
			}
			cacheDir, err := config.CacheDir()
			if err != nil {
				return err
			}
			req.VMDir = filepath.Join(stateDir, "vms")
			req.CacheDir = filepath.Join(cacheDir, "anyvm")
			if req.LogDir, err = logDir(); err != nil {
				return err
			}
			req.BootTimeout = flags.bootTimeout
			return a.get(cmd, flags, req)
		},
	}
	cmd.Flags().StringVar(&release, "release", "", "FreeBSD release, such as 14.4 (required)")
	_ = cmd.MarkFlagRequired("release")
	return cmd
}

func (a *app) getOHOSCommand(flags *getFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "ohos",
		Short: "Print the hdc connect key of an OpenHarmony emulator",
		Long: `Print the hdc connect key of an OpenHarmony emulator for this worktree.

get returns the worktree's emulator, or starts QEMU on the Oniro emulator image
in $OHOS_EMULATOR_IMAGES and waits until hdc can run a shell command in it.
The emulator runs with -snapshot, so instances share the image and never
change it. qemu-system-x86_64 and hdc must be on PATH. QEMU uses KVM on an
x86_64 Linux host that has it and emulates the machine elsewhere.`,
		Example: `  hdc -t "$(devices-cli get ohos)" shell uname -a`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := ohos.NewRequest()
			if err != nil {
				return err
			}
			req.BootTimeout = flags.bootTimeout
			if req.LogDir, err = logDir(); err != nil {
				return err
			}
			return a.get(cmd, flags, req)
		},
	}
}

func (a *app) getAndroidCommand(flags *getFlags) *cobra.Command {
	req := &android.Request{Log: a.stderr}
	cmd := &cobra.Command{
		Use:   "android --api <level> [flags] [-- <emulator flags>]",
		Short: "Print the adb serial of an Android emulator or assigned device",
		Long: `Print the adb serial of an Android device for this worktree.

A physical device assigned to the worktree with "devices-cli assign" comes
first when it is connected and runs the API level and ABI. Otherwise get
returns the worktree's emulator for the system image, starting one that runs
until it is released, idle too long, or its worktree is gone.

Emulators run headless by default; --headful opens a window.
Each hardware profile has its own AVD. Emulators run read-only from one AVD per system image, which devices-cli
creates on first use, installing the image into $ANDROID_HOME if it is
missing. Flags after -- go to the emulator, and an instance only matches a
request with the same flags. devices-cli sets -avd, -port, -read-only,
-no-window (unless --headful), -no-audio, -no-boot-anim, -no-snapshot,
-no-metrics, and -crash-report-mode disabled.`,
		Example: `  devices-cli get android --api 36
  devices-cli get android --api 34 --variant google_apis --abi x86_64
  devices-cli get android --api 36 -- -gpu swiftshader -memory 4096`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash != 0 && len(args) > 0 {
				return errors.New("unexpected argument; pass emulator flags after --")
			}
			req.Args = args
			if err := req.Validate(); err != nil {
				return err
			}
			sdk, err := android.FindSDK()
			if err != nil {
				return err
			}
			req.SDK = sdk
			req.BootTimeout = flags.bootTimeout
			if req.LogDir, err = logDir(); err != nil {
				return err
			}
			return a.get(cmd, flags, req)
		},
	}
	cmd.Flags().StringVar(&req.API, "api", "", "API level of the system image, such as 36 (required)")
	cmd.Flags().StringVar(&req.ABI, "abi", android.HostABI(), "ABI of the system image")
	cmd.Flags().StringVar(&req.Variant, "variant", "default", "system image variant, such as default, google_apis, or google_apis_playstore")
	cmd.Flags().BoolVar(&req.Headful, "headful", false, "open an emulator window (default: headless)")
	cmd.Flags().StringVar(&req.DeviceProfile, "device-profile", "", "avdmanager hardware profile ID, such as wearos_small_round or tv_1080p")
	_ = cmd.MarkFlagRequired("api")
	return cmd
}

func (a *app) getAppleCommand(flags *getFlags, platform apple.Platform) *cobra.Command {
	var runtime, deviceType string
	cmd := &cobra.Command{
		Use:   platform.Kind + " [flags]",
		Short: "Print the UDID of the worktree's " + platform.Name + " simulator",
		Long: fmt.Sprintf(`Print the UDID of a %[1]s simulator for this worktree.

get returns the worktree's simulator for the runtime and device type, or
creates and boots a new one, named "devices-cli <worktree name>". The
simulator is deleted when it is released, idle too long, or its worktree is
gone. Runtimes come with Xcode; install more with
"xcodebuild -downloadPlatform %[1]s".`, platform.Name),
		Example: fmt.Sprintf(`  devices-cli get %[1]s
  devices-cli get %[1]s --runtime 26.4`, platform.Kind),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := apple.NewRequest(cmd.Context(), platform, runtime, deviceType)
			if err != nil {
				return err
			}
			req.BootTimeout = flags.bootTimeout
			return a.get(cmd, flags, req)
		},
	}
	cmd.Flags().StringVar(&runtime, "runtime", "", platform.Name+" runtime version, such as 26 or 26.4 (default: the newest installed)")
	cmd.Flags().StringVar(&deviceType, "device-type", "", "device type name or identifier (default: the runtime's first "+platform.Family+")")
	return cmd
}

func (a *app) get(cmd *cobra.Command, flags *getFlags, req devices.Request) error {
	ownerPath, err := owner.Resolve(flags.owner)
	if err != nil {
		return err
	}
	m, err := a.manager()
	if err != nil {
		return err
	}
	id, err := m.Get(cmd.Context(), ownerPath, req, flags.wait)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, id)
	return nil
}
