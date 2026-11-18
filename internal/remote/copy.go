package remote

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// completeMarker marks a copy that finished; a copy without it is redone.
// Each run that uses the copy touches it.
const completeMarker = ".devices-cli-complete"

// staleAfter is how long an earlier copy, or an unfinished one, goes
// untouched before a new copy of the same directory removes it.
const staleAfter = "60" // minutes, for find -mmin

// Shell runs short POSIX shell commands on a device.
type Shell interface {
	// Run runs the script with stdin, if any, and returns its trimmed
	// standard output, or an error when it fails.
	Run(ctx context.Context, stdin io.Reader, script string) (string, error)
	// Name names the device in messages.
	Name() string
}

// Locker takes an exclusive host lock by name and returns its unlock function.
type Locker func(name string) (func(), error)

// Copy is a directory on a device that Fill writes into a temporary directory
// before it takes its final name, Dir. Dir's name identifies the contents, so
// a finished copy is reused as it is. When Stale is set, the other
// directories in it are earlier copies, which a new copy removes once they
// have gone unused for staleAfter: a concurrent run may still use one.
type Copy struct {
	Dir   string
	Stale string
	Fill  func(ctx context.Context, tmp string) error
}

// CopyMissing makes each copy that the device lacks. Each runs under a host
// lock, so concurrent runs make it once, and none sees a partial copy.
func CopyMissing(ctx context.Context, sh Shell, copies []Copy, lock Locker) error {
	missing, err := missingCopies(ctx, sh, copies)
	if err != nil {
		return err
	}
	for _, c := range missing {
		if err := copyOne(ctx, sh, c, lock); err != nil {
			return err
		}
	}
	return nil
}

func copyOne(ctx context.Context, sh Shell, c Copy, lock Locker) error {
	unlock, err := lock("exec-" + Hash(sh.Name(), c.Dir))
	if err != nil {
		return err
	}
	defer unlock()
	if still, err := missingCopies(ctx, sh, []Copy{c}); err != nil || len(still) == 0 {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", c.Dir, os.Getpid())
	q := ShellQuote
	if _, err := sh.Run(ctx, nil, "rm -rf "+q(tmp)+" && mkdir -p "+q(tmp)); err != nil {
		return fmt.Errorf("prepare %s on %s: %w", c.Dir, sh.Name(), err)
	}
	if err := c.Fill(ctx, tmp); err != nil {
		return fmt.Errorf("copy to %s on %s: %w", c.Dir, sh.Name(), err)
	}
	script := "touch " + q(path.Join(tmp, completeMarker)) + " && rm -rf " + q(c.Dir) + " && mv " + q(tmp) + " " + q(c.Dir)
	if c.Stale != "" {
		// A finished copy's marker shows its last use; an unfinished one's
		// directory shows when its upload began.
		script += " && for d in " + q(c.Stale) + "/*; do [ \"$d\" = " + q(c.Dir) + " ] && continue; " +
			"m=\"$d/" + completeMarker + "\"; [ -e \"$m\" ] || m=\"$d\"; " +
			"[ -n \"$(find \"$m\" -maxdepth 0 -mmin +" + staleAfter + ")\" ] && rm -rf \"$d\"; done; true"
	}
	if _, err := sh.Run(ctx, nil, script); err != nil {
		return fmt.Errorf("finish %s on %s: %w", c.Dir, sh.Name(), err)
	}
	return nil
}

// missingCopies returns the copies that the device lacks, in one call.
func missingCopies(ctx context.Context, sh Shell, copies []Copy) ([]Copy, error) {
	var script strings.Builder
	for i, c := range copies {
		marker := ShellQuote(path.Join(c.Dir, completeMarker))
		fmt.Fprintf(&script, "if [ -e %s ]; then touch %s; else echo %d; fi; ", marker, marker, i)
	}
	script.WriteString("true")
	out, err := sh.Run(ctx, nil, script.String())
	if err != nil {
		return nil, fmt.Errorf("check %s for earlier copies: %w", sh.Name(), err)
	}
	var missing []Copy
	for _, field := range strings.Fields(out) {
		var i int
		if _, err := fmt.Sscan(field, &i); err == nil && i >= 0 && i < len(copies) {
			missing = append(missing, copies[i])
		}
	}
	return missing, nil
}

// Plan lays out a spec's copies under root on a device: the program in
// bin/<key>/ and each synced directory in sync/<dir hash>/<contents key>/.
// sendFile copies a host file into a device directory; sendTree unpacks a
// tree into one. It returns the copies and the mapping from host paths to
// their copies.
func Plan(spec Spec, root string, sendFile func(ctx context.Context, host, dir string) error, sendTree func(ctx context.Context, t *Tree, dir string) error) ([]Copy, Mapping, error) {
	key, err := FileKey(spec.Program)
	if err != nil {
		return nil, nil, err
	}
	programDir := path.Join(root, "bin", key)
	copies := []Copy{{
		Dir:  programDir,
		Fill: func(ctx context.Context, tmp string) error { return sendFile(ctx, spec.Program, tmp) },
	}}
	mapping := Mapping{spec.Program: path.Join(programDir, baseName(spec.Program))}
	for _, dir := range spec.Syncs {
		tree, err := Snapshot(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s to sync it: %w", dir, err)
		}
		parent := path.Join(root, "sync", Hash(dir))
		target := path.Join(parent, tree.Key())
		mapping[dir] = target
		copies = append(copies, Copy{
			Dir:   target,
			Stale: parent,
			Fill:  func(ctx context.Context, tmp string) error { return sendTree(ctx, tree, tmp) },
		})
	}
	return copies, mapping, nil
}

func baseName(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	return p[i+1:]
}
