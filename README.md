# devices-cli

Gives each git worktree its own ephemeral test device: an Android emulator, an
Apple simulator, an OpenHarmony emulator, or a FreeBSD virtual machine. A
project asks for a device and gets its ID; devices-cli starts the device on
demand, keeps it apart from every other worktree's, caps how many run at once,
and stops it when the worktree goes away.

It is for machines that run many coding agents or checkouts side by side.
Sharing one emulator between worktrees breaks suites that install an app, and
per-project emulators pile up until they starve RAM and disk. With devices-cli,
a project only declares what it needs; the machine's devices belong to the tool.

## Install

With [mise](https://mise.jdx.dev), pin a commit as a Go pseudo-version:

```toml
[tools]
"go:github.com/sargunv/devices-cli" = "v0.0.0-20261026000000-000000000000"
```

`go list -m github.com/sargunv/devices-cli@main` prints the pseudo-version of
the latest commit. Or install with Go directly:

```sh
go install github.com/sargunv/devices-cli@main
```

devices-cli runs on macOS and Linux.

## Use

```sh
devices-cli get android --api 36
```

prints an adb serial such as `emulator-5554` on stdout once the device has
booted, and progress on stderr. The device belongs to the git worktree that
contains the working directory; `--owner <path>` names another directory, for
example in CI outside a checkout. Calling `get` again from the same worktree
returns the same running device at once.

A project that should also honor a device its user chose writes:

```sh
ANDROID_SERIAL=${ANDROID_SERIAL:-$(devices-cli get android --api 36)}
```

For example, as a mise task:

```toml
[tasks.test-android]
run = '''
export ANDROID_SERIAL=${ANDROID_SERIAL:-$(devices-cli get android --api 36)}
./gradlew connectedCheck
'''
```

### Android emulators

```sh
devices-cli get android --api <level> [--abi <abi>] [--variant <tag>] [-- <emulator flags>]
```

- `--api` is the system image's API level, such as `36` or `36.1`.
- `--abi` defaults to the host's: `arm64-v8a` on arm64, `x86_64` elsewhere.
- `--variant` is the image tag: `default` (AOSP, the default), `google_apis`,
  `google_apis_playstore`, and so on.
- Flags after `--` go to the emulator, such as `-gpu swiftshader` or
  `-memory 4096`. A running instance only matches a request with the same flags.
  devices-cli sets `-avd`, `-port`, `-read-only`, `-no-window`, `-no-audio`,
  `-no-boot-anim`, `-no-snapshot`, and `-no-metrics` itself.

The Android SDK comes from `ANDROID_HOME` (or `ANDROID_SDK_ROOT`), which a
project usually sets in its mise environment. When the SDK lacks
`platform-tools`, `emulator`, or the system image, devices-cli installs them
with Google's `android` CLI (from the SDK's command-line tools or `PATH`; mise
has it as `http:android-cli`), falling back to `sdkmanager`.

Each system image gets one machine-wide AVD, named
`devices-cli-android-<api>-<variant>-<abi>` in the default AVD home and created
on first use. Every worktree runs its own read-only instance of it: the instance
keeps its writes in temporary files that go away when it stops, so instances
never change the AVD and no worktree accumulates disk. Instances cold-boot,
which takes about 30 seconds on an Apple silicon laptop. A new AVD first boots
once on its own to set up its disk images, which adds one boot to the first
`get` that uses it.

On Linux, emulators need KVM; on a GitHub Actions runner, enable it first:

```sh
echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' | sudo tee /etc/udev/rules.d/99-kvm4all.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --name-match=kvm
```

### Apple simulators

```sh
devices-cli get ios [--runtime <version>] [--device-type <name>]
```

prints the UDID of the worktree's simulator, creating and booting one if needed.
`tvos`, `visionos`, and `watchos` work the same way.

- `--runtime` is a runtime version such as `26` or `26.4`; it picks the newest
  installed runtime that matches, and defaults to the newest installed.
- `--device-type` is a device type name or identifier, such as `iPhone 17`; it
  defaults to the runtime's first iPhone, Apple TV, Apple Vision, or Apple
  Watch.

Each worktree gets its own simulator, named `devices-cli <worktree name>`, which
is deleted, with its data, when it is released or collected. Runtimes come with
Xcode; `xcodebuild -downloadPlatform <platform>` installs more. A simulator
boots in about 30 seconds.

### OpenHarmony emulators

```sh
devices-cli get ohos
```

prints the hdc connect key, such as `127.0.0.1:56000`, of the worktree's
OpenHarmony emulator once `hdc -t <key> shell` works in it:

```sh
hdc -t "$(devices-cli get ohos)" shell uname -a
```

The emulator is QEMU running the
[Oniro emulator image](https://github.com/eclipse-oniro4openharmony/device_board_oniro/releases)
from the directory that `OHOS_EMULATOR_IMAGES` names, which holds its `bzImage`
and `.img` files. `qemu-system-x86_64` and `hdc` (from the OpenHarmony SDK's
`toolchains` directory) must be on `PATH`. Each instance runs with `-snapshot`,
so instances share the image and never change it, and forwards the guest's hdc
port to a host port from 56000 up. QEMU uses KVM on an x86_64 Linux host that
has it and emulates the machine elsewhere; on an Apple silicon laptop an
instance answers hdc in about 30 seconds.

### FreeBSD virtual machines

```sh
devices-cli get freebsd --release 14.4
```

prints an SSH host, such as `devices-cli-freebsd-60000`, for the worktree's
FreeBSD virtual machine once it answers SSH:

```sh
ssh "$(devices-cli get freebsd --release 14.4)" uname -a
```

[anyvm](https://github.com/anyvm-org/anyvm) boots the VM and adds the host to
`~/.ssh/config.d`, which `~/.ssh/config` includes. The worktree is mounted in
the VM over NFS at its own path, so a command can use the same paths there. The
VM boots anyvm's cached image (shared in `$XDG_CACHE_HOME/anyvm`) with
`--snapshot`, so VMs share the image and a VM's changes, such as installed
packages, go away with it. Releasing or collecting a VM runs `shutdown -p now`
in it and signals QEMU only if the guest does not power off.

anyvm comes from `ANYVM`, the path of `anyvm.py`, or from `anyvm` on `PATH`. It
needs `python3`, QEMU, and UEFI firmware (the `ovmf` package on Debian and
Ubuntu). FreeBSD VMs run only on a Linux host with KVM.

### Physical Android devices

```sh
devices-cli assign <serial>
```

assigns a connected device to the worktree. `get android` then returns it to
this worktree whenever it is connected and runs the requested API level and ABI,
and otherwise falls back to an emulator. It never goes to another worktree:
assigning a device that another worktree holds fails until that worktree
releases it.

### Other commands

- `devices-cli release` stops the worktree's instances and unassigns its
  devices.
- `devices-cli ls` lists running instances and assigned devices with their
  owners and idle times; `--json` adds the definitions.
- `devices-cli gc` collects what is no longer needed (see below).

Every kind of device counts toward the same cap.

`devices-cli <command> --help` describes every flag.

## Limits and cleanup

At most `max_instances` instances run at once, across all worktrees and
projects. A `get` that finds every slot taken waits up to `--wait` (10 minutes
by default) for one to free up, then fails with a message that says who holds
them.

Every command first collects what is no longer needed, and `devices-cli gc` does
only that:

- an instance whose owner directory is gone, that stopped on its own, or that no
  `get` has asked for in `idle_timeout` is stopped;
- a device assignment whose owner directory is gone is dropped;
- a definition that no `get` has asked for in `definition_ttl_days` is removed.

devices-cli only ever touches the instances and definitions it created.

### Settings

`$XDG_CONFIG_HOME/devices-cli/config.toml` (default
`~/.config/devices-cli/config.toml`) holds the machine-wide settings. All are
optional:

```toml
# Most instances that run at once. Default: one per 8 GiB of RAM.
max_instances = 4
# Stop an instance that no get has asked for in this long; "0" never does.
# Default: "2h". A get refreshes it, so a suite that runs longer should ask
# again as it goes.
idle_timeout = "2h"
# Remove a definition that no get has asked for in this many days; 0 never
# does. Default: 14.
definition_ttl_days = 14
```

State, locks, and emulator logs live in `$XDG_STATE_HOME/devices-cli` (default
`~/.local/state/devices-cli`).

### Scheduled collection

Commands collect as they run, so a machine that stops running them keeps its
instances until the next one. A scheduled `devices-cli gc` covers that. It needs
no Android SDK. Replace `/path/to/devices-cli` with the output of
`command -v devices-cli`.

On macOS, save as `~/Library/LaunchAgents/dev.devices-cli.gc.plist` and run
`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.devices-cli.gc.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.devices-cli.gc</string>
  <key>ProgramArguments</key>
  <array>
    <string>/path/to/devices-cli</string>
    <string>gc</string>
  </array>
  <key>StartInterval</key>
  <integer>900</integer>
</dict>
</plist>
```

On Linux, save as `~/.config/systemd/user/devices-cli-gc.service`:

```ini
[Unit]
Description=Collect unneeded devices-cli instances

[Service]
Type=oneshot
ExecStart=/path/to/devices-cli gc
```

and `~/.config/systemd/user/devices-cli-gc.timer`:

```ini
[Unit]
Description=Collect unneeded devices-cli instances every 15 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min

[Install]
WantedBy=timers.target
```

then run `systemctl --user enable --now devices-cli-gc.timer`.

## Development

`mise install` sets up the tools. `mise run check` lints and checks formatting,
`mise run fix` formats, and `mise run test` runs the unit tests, which use fake
SDK tools and boot nothing.

`mise run integration` boots real devices through the built CLI and checks
reuse, isolation between owners, owner-gone collection, and release. Each test
uses its own state directory and takes about a minute once its image is
installed; append `-- -run <test>` to run one:

- `TestAndroidEmulators` needs `ANDROID_HOME`, and KVM on Linux.
- `TestAppleSimulators` runs on macOS with Xcode.
- `TestOHOSEmulators` needs `OHOS_EMULATOR_IMAGES` and `qemu-system-x86_64` and
  `hdc` on `PATH`. Put the real `hdc` ahead of mise's shims.
- `TestFreeBSDVMs` runs on Linux with KVM and needs `ANYVM`, QEMU, and UEFI
  firmware.

CI runs the Android, OpenHarmony, and FreeBSD tests on Linux with KVM and the
Apple test on macOS.
