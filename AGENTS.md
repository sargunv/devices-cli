# devices-cli

A Go CLI that gives each git worktree its own ephemeral test device. The README
is the user contract; `--help` text must agree with it.

## Project map

- `main.go`: entry point.
- `internal/cli/`: cobra commands and output formatting.
- `internal/devices/`: the manager: get, assign, release, list, and collection
  over the `Request` and `Kind` interfaces that each device kind implements.
- `internal/state/`: the machine-wide state file and its file locks.
- `internal/config/`: settings file and XDG directories.
- `internal/owner/`: resolves the owning worktree.
- `internal/android/`: SDK discovery and installs, AVD definitions, emulator
  instances, and assigned physical devices.
- `internal/apple/`: per-owner simulators through simctl.
- `internal/ohos/`: per-owner QEMU instances of the Oniro image, reached through
  hdc.
- `internal/process/`: helpers for the device processes kinds launch.
- `integration/`: boots real devices through the built binary (build tag
  `integration`).

## Dev tool commands

`mise run check`, `mise run fix`, `mise run test`, `mise run build` (to `bin/`),
and `mise run integration` (boots real emulators and simulators; the Android
test needs `ANDROID_HOME`).

## Project invariants

- `get` prints only the device ID on stdout; everything else goes to stderr.
- devices-cli only stops, deletes, or edits instances and definitions it created
  and recorded. Android definitions carry the `devices-cli-` prefix and
  simulators the `devices-cli ` name prefix.
- The state lock covers only quick work. Installs and boot waits happen outside
  it; slow definition setup takes its own per-definition lock.
- Collection must work without an SDK in the environment, since scheduled `gc`
  runs without one.
- Unit tests never start real devices; they use fakes and fake SDK scripts. Only
  `integration/` boots real ones.
- Every error says what to do next.
