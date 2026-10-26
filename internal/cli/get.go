package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/devices"
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
	return cmd
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

Emulators run read-only from one AVD per system image, which devices-cli
creates on first use, installing the image into $ANDROID_HOME if it is
missing. Flags after -- go to the emulator, and an instance only matches a
request with the same flags. devices-cli sets -avd, -port, -read-only,
-no-window, -no-audio, -no-boot-anim, -no-snapshot, and -no-metrics.`,
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
	_ = cmd.MarkFlagRequired("api")
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
