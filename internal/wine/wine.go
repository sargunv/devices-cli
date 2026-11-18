// Package wine gives each owner a Wine prefix of its own, set up once with the
// native DLLs a request names, and runs Windows programs in it.
package wine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sargunv/devices-cli/internal/remote"
	"github.com/sargunv/devices-cli/internal/state"
)

// KindName is the kind of every Wine prefix.
const KindName = "wine"

// readyFile marks a prefix whose setup finished, and serverFile names the
// wineserver that runs its session, for Stop to end it without Wine on PATH.
const (
	readyFile  = ".devices-cli-ready"
	serverFile = ".devices-cli-wineserver"
)

// Request asks for a Wine prefix with a set of native DLLs.
type Request struct {
	// DLLs are the absolute host paths of the DLLs that replace Wine's own,
	// sorted.
	DLLs []string
	// Root holds one directory per prefix.
	Root        string
	BootTimeout time.Duration
}

// NewRequest checks that Wine is on PATH and expands the DLL arguments: each
// names a DLL, or a directory whose DLLs all count.
func NewRequest(dlls []string) (*Request, error) {
	for _, tool := range []string{"wine", "wineserver"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("no %s on PATH: install Wine, or put its bin directory on PATH", tool)
		}
	}
	req := &Request{}
	for _, arg := range dlls {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("--dll %s: %w", arg, err)
		}
		if !info.IsDir() {
			if !isDLL(abs) {
				return nil, fmt.Errorf("--dll %s is not a .dll file; name a DLL or a directory of them", arg)
			}
			req.DLLs = append(req.DLLs, abs)
			continue
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range entries {
			if !e.IsDir() && isDLL(e.Name()) {
				req.DLLs = append(req.DLLs, filepath.Join(abs, e.Name()))
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("--dll %s holds no .dll files; name a DLL or a directory of them", arg)
		}
	}
	slices.Sort(req.DLLs)
	req.DLLs = slices.Compact(req.DLLs)
	names := map[string]string{}
	for _, dll := range req.DLLs {
		name := strings.ToLower(filepath.Base(dll))
		if other, ok := names[name]; ok {
			return nil, fmt.Errorf("--dll names two DLLs called %s: %s and %s", name, other, dll)
		}
		names[name] = dll
	}
	return req, nil
}

func isDLL(name string) bool { return strings.EqualFold(filepath.Ext(name), ".dll") }

// Kind implements devices.Request.
func (r *Request) Kind() string { return KindName }

// Key implements devices.Request. A prefix only serves requests for the same
// DLLs.
func (r *Request) Key() string {
	if len(r.DLLs) == 0 {
		return "wine"
	}
	return "wine " + digest(strings.Join(r.DLLs, "\x00"))
}

// Description implements devices.Request.
func (r *Request) Description() string {
	if len(r.DLLs) == 0 {
		return "wine prefix"
	}
	return fmt.Sprintf("wine prefix with %d DLLs", len(r.DLLs))
}

// Definition implements devices.Request. Prefixes have no shared definition.
func (r *Request) Definition() string { return "" }

// Prepare implements devices.Request. There is nothing to prepare.
func (r *Request) Prepare(context.Context) error { return nil }

// Start creates the prefix's empty directory; WaitReady sets it up, outside
// the state lock.
func (r *Request) Start(_ context.Context, owner string, _ []state.Instance) (state.Instance, error) {
	dir := filepath.Join(r.Root, "wine-"+digest(owner+"\x00"+r.Key()))
	if err := os.RemoveAll(dir); err != nil {
		return state.Instance{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return state.Instance{}, err
	}
	now := time.Now()
	return state.Instance{
		Owner:       owner,
		Kind:        KindName,
		Key:         r.Key(),
		Description: r.Description(),
		ID:          dir,
		Started:     now,
		LastUsed:    now,
	}, nil
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// WaitReady sets the prefix up unless an earlier get did, and starts its Wine
// session unless it runs. Concurrent gets wait for one another.
func (r *Request) WaitReady(ctx context.Context, inst state.Instance) error {
	lock, err := os.OpenFile(filepath.Join(inst.ID, ".devices-cli.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", lock.Name(), err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.BootTimeout)
	defer cancel()
	if _, err := os.Stat(filepath.Join(inst.ID, readyFile)); err != nil {
		if err := r.setUp(ctx, inst.ID); err != nil {
			return err
		}
	}
	return start(ctx, inst.ID)
}

// start starts the prefix's Wine session unless it runs: a wineserver that
// lasts until Stop, and the services that the first program would otherwise
// start. Started by a program, they would hold its output open after it
// exits.
func start(ctx context.Context, prefix string) error {
	wineserver, err := exec.LookPath("wineserver")
	if err != nil {
		return err
	}
	if wineserver, err = filepath.Abs(wineserver); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(prefix, serverFile), []byte(wineserver), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, wineserver, "--persistent")
	cmd.Env = append(os.Environ(), "WINEPREFIX="+prefix)
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		// A wineserver already runs for the prefix.
		return nil
	}
	if err != nil {
		return fmt.Errorf("start wineserver for %s: %w", prefix, err)
	}
	return run(ctx, prefix, "", "wine", "wineboot")
}

// setUp boots Wine in a new prefix, copies the DLLs into its system32
// directory, and has Wine prefer them to its own.
func (r *Request) setUp(ctx context.Context, prefix string) error {
	// No Wine Mono or Gecko: their installers would ask in a window.
	if err := run(ctx, prefix, "mscoree,mshtml=", "wine", "wineboot", "--init"); err != nil {
		return err
	}
	system32 := filepath.Join(prefix, "drive_c", "windows", "system32")
	for _, dll := range r.DLLs {
		if err := copyFile(dll, filepath.Join(system32, strings.ToLower(filepath.Base(dll)))); err != nil {
			return err
		}
	}
	if len(r.DLLs) > 0 {
		var reg strings.Builder
		reg.WriteString("Windows Registry Editor Version 5.00\r\n\r\n[HKEY_CURRENT_USER\\Software\\Wine\\DllOverrides]\r\n")
		for _, dll := range r.DLLs {
			fmt.Fprintf(&reg, "\"%s\"=\"native,builtin\"\r\n", strings.ToLower(strings.TrimSuffix(filepath.Base(dll), filepath.Ext(dll))))
		}
		if err := os.WriteFile(filepath.Join(prefix, "drive_c", "devices-cli.reg"), []byte(reg.String()), 0o644); err != nil {
			return err
		}
		if err := run(ctx, prefix, "", "wine", "regedit", "/S", `C:\devices-cli.reg`); err != nil {
			return err
		}
	}
	// The registry reaches the prefix's files when its wineserver exits.
	if err := run(ctx, prefix, "", "wineserver", "--wait"); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(prefix, readyFile), nil, 0o644)
}

// run runs a Wine tool in the prefix, quietly, with extra DLL overrides. Its
// output goes to a file rather than a pipe, which the Wine processes it
// starts would hold open.
func run(ctx context.Context, prefix, overrides, name string, args ...string) error {
	out, err := os.CreateTemp("", "devices-cli-wine-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "WINEPREFIX="+prefix, "WINEDEBUG=-all", "WINEDLLOVERRIDES="+overrides)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		log, _ := os.ReadFile(out.Name())
		return fmt.Errorf("set up Wine prefix %s: %s %s: %w\n%s", prefix, name, strings.Join(args, " "), err, log)
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Exec runs the spec's program with Wine in the prefix and returns its exit
// status. Wine sees the host's files and environment, so the program keeps
// its paths and variables, and nothing is copied. WINEDEBUG defaults to -all,
// so Wine's own messages stay out of the program's output.
func Exec(ctx context.Context, prefix string, spec remote.Spec) (int, error) {
	cmd := exec.CommandContext(ctx, "wine", append([]string{spec.Program}, spec.Args...)...)
	cmd.Env = os.Environ()
	if _, ok := os.LookupEnv("WINEDEBUG"); !ok {
		cmd.Env = append(cmd.Env, "WINEDEBUG=-all")
	}
	cmd.Env = append(cmd.Env, "WINEPREFIX="+prefix)
	cmd.Dir = spec.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), nil
	default:
		return 0, fmt.Errorf("wine %s: %w", spec.Program, err)
	}
}

// Kind manages Wine prefixes.
type Kind struct{}

// Alive implements devices.Kind. A prefix lasts until it is removed.
func (Kind) Alive(_ context.Context, inst state.Instance) bool {
	_, err := os.Stat(inst.ID)
	return err == nil
}

// Stop implements devices.Kind. It ends the prefix's Wine processes with the
// wineserver that started them, else the one on PATH, and removes the prefix.
// wineserver --kill fails alike when no server runs, so its status is not
// checked.
func (Kind) Stop(ctx context.Context, inst state.Instance) error {
	wineserver, _ := os.ReadFile(filepath.Join(inst.ID, serverFile))
	path := string(wineserver)
	if _, err := os.Stat(path); err != nil {
		path, _ = exec.LookPath("wineserver")
	}
	if path != "" {
		cmd := exec.CommandContext(ctx, path, "--kill")
		cmd.Env = append(os.Environ(), "WINEPREFIX="+inst.ID)
		_ = cmd.Run()
	}
	return os.RemoveAll(inst.ID)
}

// RemoveDefinition implements devices.Kind. Prefixes have no definitions.
func (Kind) RemoveDefinition(string) error { return nil }
