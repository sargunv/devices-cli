// Package remote describes a host-built program to run on a device: its
// arguments and environment, the host directories it needs on the device, and
// how host paths map to their copies there.
package remote

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is a program to run on a device, as a host would run it.
type Spec struct {
	// Program is the absolute host path of the executable.
	Program string
	Args    []string
	// Env holds the variables to forward, by name, with their host values.
	Env map[string]string
	// Syncs are absolute host files or directories to copy to the device.
	Syncs []string
	// Dir is the host working directory.
	Dir string
	// Stdin, Stdout and Stderr are the program's streams.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Mapping maps host paths to device paths: each key is an absolute host path,
// a file or a directory, and its value the path of its copy on the device.
type Mapping map[string]string

// Path returns the device path of a host path that is a mapped path or lies
// inside a mapped directory, and false otherwise. Paths are compared as
// written and with symbolic links resolved, since a working directory comes
// resolved (/private/var on macOS) while a variable may name /var.
func (m Mapping) Path(host string) (string, bool) {
	if !filepath.IsAbs(host) {
		return "", false
	}
	if p, ok := m.lookup(filepath.Clean(host), false); ok {
		return p, true
	}
	if resolved, err := filepath.EvalSymlinks(host); err == nil {
		return m.lookup(resolved, true)
	}
	return "", false
}

func (m Mapping) lookup(host string, resolve bool) (string, bool) {
	best, bestKey := "", ""
	for key := range m {
		prefix := key
		if resolve {
			if resolved, err := filepath.EvalSymlinks(key); err == nil {
				prefix = resolved
			}
		}
		if (host == prefix || strings.HasPrefix(host, prefix+string(filepath.Separator))) && len(prefix) > len(best) {
			best, bestKey = prefix, key
		}
	}
	if bestKey == "" {
		return "", false
	}
	rel, err := filepath.Rel(best, host)
	if err != nil {
		return "", false
	}
	return path.Join(m[bestKey], filepath.ToSlash(rel)), true
}

// Rewrite returns the value with a mapped host path replaced by its device
// path, and other values unchanged.
func (m Mapping) Rewrite(value string) string {
	if p, ok := m.Path(value); ok {
		return p
	}
	return value
}

// Command is the program, its arguments, its environment and its working
// directory as they are on the device.
type Command struct {
	Program string
	Args    []string
	// Env is sorted by name, as NAME=value.
	Env []string
	// Dir is empty when the host working directory has no copy on the device.
	Dir string
}

// Command rewrites the spec's paths through the mapping.
func (s Spec) Command(m Mapping) Command {
	c := Command{Program: m.Rewrite(s.Program)}
	for _, arg := range s.Args {
		c.Args = append(c.Args, m.Rewrite(arg))
	}
	names := make([]string, 0, len(s.Env))
	for name := range s.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c.Env = append(c.Env, name+"="+m.Rewrite(s.Env[name]))
	}
	if dir, ok := m.Path(s.Dir); ok {
		c.Dir = dir
	}
	return c
}

// ShellQuote quotes a word for a POSIX shell.
func ShellQuote(word string) string {
	if word != "" && strings.IndexFunc(word, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./=:,+@%", r)
	}) < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// Script is a POSIX shell command line that runs the command.
func (c Command) Script() string {
	var b strings.Builder
	if c.Dir != "" {
		b.WriteString("cd " + ShellQuote(c.Dir) + " && ")
	}
	b.WriteString("exec")
	if len(c.Env) > 0 {
		b.WriteString(" env")
		for _, kv := range c.Env {
			b.WriteString(" " + ShellQuote(kv))
		}
	}
	b.WriteString(" " + ShellQuote(c.Program))
	for _, arg := range c.Args {
		b.WriteString(" " + ShellQuote(arg))
	}
	return b.String()
}

// Hash returns a short hex digest of the parts, for names on a device.
func Hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// FileKey identifies a host file's contents by its path, size and
// modification time, without reading it.
func FileKey(name string) (string, error) {
	info, err := os.Stat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	return Hash(name, fmt.Sprint(info.Size()), fmt.Sprint(info.ModTime().UnixNano())), nil
}

// Tree is a host directory as it is copied to a device: its regular files and
// directories, with symbolic links replaced by what they point to.
type Tree struct {
	Root    string
	entries []entry
}

type entry struct {
	rel  string // slash-separated, relative to the root
	host string
	info fs.FileInfo
}

// Snapshot walks the directory. It follows symbolic links and skips version
// control directories and build directories that carry a CACHEDIR.TAG, such
// as Cargo's target directory.
func Snapshot(root string) (*Tree, error) {
	t := &Tree{Root: root}
	seen := map[string]bool{}
	if err := t.walk(root, "", seen); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tree) walk(dir, rel string, seen map[string]bool) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if seen[resolved] {
		return nil // a link back up the tree
	}
	seen[resolved] = true
	defer delete(seen, resolved)
	names, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, d := range names {
		name := d.Name()
		if name == ".git" || name == ".jj" || name == ".hg" {
			continue
		}
		host := filepath.Join(dir, name)
		info, err := os.Stat(host) // follows links
		if err != nil {
			if d.Type()&fs.ModeSymlink != 0 && os.IsNotExist(err) {
				continue // a dangling link
			}
			return err
		}
		childRel := path.Join(rel, name)
		switch {
		case info.IsDir():
			if _, err := os.Stat(filepath.Join(host, "CACHEDIR.TAG")); err == nil {
				continue
			}
			t.entries = append(t.entries, entry{childRel, host, info})
			if err := t.walk(host, childRel, seen); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			t.entries = append(t.entries, entry{childRel, host, info})
		}
	}
	return nil
}

// Key identifies the tree's contents by its files' paths, sizes, modes and
// modification times.
func (t *Tree) Key() string {
	parts := []string{t.Root}
	for _, e := range t.entries {
		parts = append(parts, e.rel, fmt.Sprint(e.info.Size()), e.info.Mode().String(), fmt.Sprint(e.info.ModTime().UnixNano()))
	}
	return Hash(parts...)
}

// WriteTar writes the tree as a tar archive.
func (t *Tree) WriteTar(w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, e := range t.entries {
		hdr := &tar.Header{Name: e.rel, ModTime: e.info.ModTime(), Format: tar.FormatPAX}
		if e.info.IsDir() {
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, e.rel+"/", 0o755
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			continue
		}
		hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, e.info.Size(), int64(e.info.Mode().Perm()|0o600)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(e.host)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, e.info.Size())
		f.Close()
		if err != nil {
			return fmt.Errorf("copy %s: %w", e.host, err)
		}
	}
	return tw.Close()
}
