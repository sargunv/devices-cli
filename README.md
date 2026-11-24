# devices-cli

Gives each git worktree its own ephemeral test device: an Android emulator, an
Apple simulator, an OpenHarmony or HarmonyOS emulator, a FreeBSD virtual
machine, or a Wine prefix. A project asks for a device and gets its ID;
devices-cli starts the device on demand, keeps it apart from every other
worktree's, caps how many run at once, and stops it when the worktree goes away.

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
devices-cli get android --api <level> [--abi <abi>] [--variant <tag>] [--device-profile <id>] [--headful] [-- <emulator flags>]
```

- `--api` is the system image's API level, such as `36` or `36.1`.
- `--abi` defaults to the host's: `arm64-v8a` on arm64, `x86_64` elsewhere.
- `--variant` is the image tag: `default` (AOSP, the default), `google_apis`,
  `google_apis_playstore`, and so on.
- `--headful` opens a window; emulators are headless by default. Window mode is
  part of request identity, so a headful request never reuses a headless
  instance.
- `--device-profile` selects an avdmanager hardware profile ID, such as
  `wearos_small_round` or `tv_1080p`. It requires SDK command-line tools and
  gets its own AVD. Use the matching Wear OS or Android TV `--variant` too.
  Profile requests use emulators even when a physical device is assigned.
- Flags after `--` go to the emulator, such as `-gpu swiftshader` or
  `-memory 4096`. A running instance only matches a request with the same flags.
  devices-cli sets `-avd`, `-port`, `-read-only`, `-no-window` (unless
  `--headful`), `-no-audio`, `-no-boot-anim`, `-no-snapshot`, `-no-metrics`, and
  `-crash-report-mode disabled` itself.

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
OpenHarmony emulator once an application can install and launch in it: the
system has finished booting, user 100 is active, and devices-cli has unlocked
the screen:

```sh
hdc -t "$(devices-cli get ohos)" shell uname -a
```

The emulator is QEMU running the
[Oniro emulator image](https://github.com/eclipse-oniro4openharmony/device_board_oniro/releases)
from the directory that `OHOS_EMULATOR_IMAGES` names, which holds its `bzImage`
and `.img` files. `qemu-system-x86_64`, `qemu-img` and `hdc` (from the
OpenHarmony SDK's `toolchains` directory) must be on `PATH`. Each instance runs
with `-snapshot`, so instances share the image and never change it, and forwards
the guest's hdc port to a host port from 56000 up. Its data partition, where
`exec` copies programs, grows from the image's 1.3 GB to 16 GB. QEMU uses KVM on
an x86_64 Linux host that has it and emulates the machine elsewhere; on an Apple
silicon laptop an instance is ready in about a minute.

### HarmonyOS emulators

```sh
devices-cli get harmonyos [--device-type <type>] [--os-version <version>]
```

prints the hdc connect key, such as `127.0.0.1:15600`, of the worktree's
HarmonyOS emulator once the system has finished booting. It runs the arm64
system images that DevEco Studio downloads, so on an Apple silicon Mac it boots
in about 30 seconds.

- `--device-type` is the image's device type: `phone` (the default), `tablet`,
  `foldable`, and so on.
- `--os-version` picks the newest downloaded image whose version contains it,
  such as `6.0.2`, and defaults to the newest downloaded.

Each worktree's emulator is an instance of its own, which devices-cli creates
with DevEco Studio's `Emulator` command line in its state directory, starts on
an hdc port from 15600 up, and deletes, with its data (about 1.2 GB once
booted), when it is released or collected. The emulator opens a window.
`Emulator` comes from DevEco Studio on macOS, or from `HARMONYOS_EMULATOR`; hdc
from DevEco Studio, whose version matches its emulator, else from `PATH`.
Download images in DevEco Studio's Device Manager.

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

### Wine prefixes

```sh
devices-cli get wine [--dll <path>]...
```

prints the path of the worktree's Wine prefix, for `WINEPREFIX`, once Wine has
booted in it:

```sh
WINEPREFIX="$(devices-cli get wine)" wine ./app.exe
```

Each `--dll` names a 64-bit DLL, or a directory of them, that devices-cli copies
into the prefix's `system32` directory and has Wine load instead of its own, as
`native,builtin`. That is how a prefix gets
[vkd3d-proton](https://github.com/HansKristian-Work/vkd3d-proton)'s Direct3D 12
or [DXVK](https://github.com/doitsujin/dxvk)'s, and DLLs that a program loads
from the system directory. A prefix only serves requests for the same DLLs, so a
DLL rebuilt in place gets a new one.

Prefixes live in devices-cli's state directory, one per worktree and set of
DLLs, and are deleted when they are released or collected. devices-cli starts
the prefix's wineserver and services itself, without the program's output
attached, and keeps them running until then. `wine` and `wineserver` must be on
`PATH`.

### Physical Android devices

```sh
devices-cli assign <serial>
```

assigns a connected device to the worktree. `get android` then returns it to
this worktree whenever it is connected and runs the requested API level and ABI,
and otherwise falls back to an emulator. It never goes to another worktree:
assigning a device that another worktree holds fails until that worktree
releases it. Assigned devices only need `adb` (from `ANDROID_HOME` or `PATH`);
starting an emulator still needs `ANDROID_HOME`.

### Running a program on the device

```sh
devices-cli exec <kind> [get flags] [--env <name or glob>]... [--sync <path>]... [--sync-env <name>]... -- <program> [arguments]
```

gets the worktree's device as `get` does, runs a program built on the host for
it, and exits with the program's exit status. The program's output goes to
stdout and stderr, and devices-cli's own messages go to stderr.

- `--env` forwards host environment variables, by name or by a glob such as
  `RUST_*`.
- `--sync` copies a host file or directory to the device. `--sync-env` copies
  the file or directory that a variable names and forwards the variable.
- An argument, a forwarded value, or the working directory that is the program
  or a synced path, or lies inside one, becomes the path of its copy.

Copies are named for their contents, so an unchanged program, file or directory
is copied once, however many times it runs, and concurrent runs never see a
partial copy. Symbolic links are copied as what they point to; version control
directories and directories with a `CACHEDIR.TAG`, such as Cargo's `target`, are
left out. Android, OpenHarmony and HarmonyOS devices keep the copies in
`/data/local/tmp/devices-cli`. Apple simulators, FreeBSD VMs and Wine prefixes
see the host's paths, so nothing is copied, and a Wine program sees the host's
environment too; a FreeBSD VM's program and synced paths must lie in the
worktree, which the VM mounts. A simulator starts the program in its own data
directory, not the working directory. On OpenHarmony and HarmonyOS, the
program's stdout arrives a line behind, and its stderr after it exits, so `exec`
suits programs that run to completion, such as tests, rather than interactive
ones.

`exec` works as Cargo's
[target runner](https://doc.rust-lang.org/cargo/reference/config.html#targettriplerunner),
which appends the program and its arguments. End the runner with `--`, so that
the program's flags are not taken for devices-cli's:

```sh
export CARGO_TARGET_AARCH64_LINUX_ANDROID_RUNNER='devices-cli exec android --api 36 --sync-env CARGO_MANIFEST_DIR --env RUST_* --'
cargo test --target aarch64-linux-android   # or cargo nextest run
```

Cargo sets `CARGO_MANIFEST_DIR` when it runs a test, so a test that reads its
fixtures from `std::env::var("CARGO_MANIFEST_DIR")` finds the device's copy;
`env!("CARGO_MANIFEST_DIR")` keeps the host's path.

### Other commands

- `devices-cli release` stops the worktree's instances and unassigns its
  devices. `--kind android` releases only Android devices, leaving Apple
  simulators and other kinds running.
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
