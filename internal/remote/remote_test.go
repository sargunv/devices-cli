package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestMappingRewritesPathsInside(t *testing.T) {
	m := Mapping{"/w/crate": "/dev/sync/1", "/w/crate/target/test": "/dev/bin/2/test"}
	cases := map[string]string{
		"/w/crate":                     "/dev/sync/1",
		"/w/crate/testdata/x.json":     "/dev/sync/1/testdata/x.json",
		"/w/crate/target/test":         "/dev/bin/2/test",
		"/w/crateful/x":                "/w/crateful/x",
		"relative/path":                "relative/path",
		"--filter=/w/crate/testdata/x": "--filter=/w/crate/testdata/x",
	}
	for in, want := range cases {
		if got := m.Rewrite(in); got != want {
			t.Errorf("Rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMappingMatchesThroughSymbolicLinks(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(target, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	m := Mapping{link: "/dev/sync/1"}
	if got, ok := m.Path(resolved); !ok || got != "/dev/sync/1/sub" {
		t.Errorf("Path(%q) = %q, %v; want /dev/sync/1/sub", resolved, got, ok)
	}
}

func TestCommandScript(t *testing.T) {
	spec := Spec{
		Program: "/w/target/t",
		Args:    []string{"--exact", "it's", "/w/crate/x"},
		Env:     map[string]string{"B": "/w/crate", "A": "a b"},
		Dir:     "/w/crate",
	}
	got := spec.Command(Mapping{"/w/crate": "/d/s", "/w/target/t": "/d/b/t"}).Script()
	want := `cd /d/s && exec env 'A=a b' B=/d/s /d/b/t --exact 'it'\''s' /d/s/x`
	if got != want {
		t.Errorf("Script() =\n  %s\nwant\n  %s", got, want)
	}
	if got := spec.Command(Mapping{}).Script(); !strings.HasPrefix(got, "exec env") {
		t.Errorf("an unmapped working directory gets no cd: %s", got)
	}
}

func TestSnapshotFollowsLinksAndSkipsBuildDirectories(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/lib.rs", "lib")
	write("../outside/LICENSE", "license")
	write("target/CACHEDIR.TAG", "Signature: 8a477f597d28d172789f06886806bc55")
	write("target/debug/big", "skip me")
	write(".git/HEAD", "skip me")
	for name, to := range map[string]string{"LICENSE": "../outside/LICENSE", "loop": ".", "dangling": "nowhere"} {
		if err := os.Symlink(to, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	tree, err := Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tree.WriteTar(&buf); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	r := tar.NewReader(&buf)
	for {
		hdr, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r)
		files[hdr.Name] = string(body)
	}
	want := map[string]string{"src/": "", "src/lib.rs": "lib", "LICENSE": "license"}
	for name, body := range want {
		if got, ok := files[name]; !ok || got != body {
			t.Errorf("archive has %q = %q, %v; want %q", name, got, ok, body)
		}
	}
	for name := range files {
		if _, ok := want[name]; !ok && !strings.HasPrefix(name, "loop/") {
			t.Errorf("archive has unexpected %q", name)
		}
		if strings.HasPrefix(name, "loop/loop/") {
			t.Errorf("archive follows the link loop: %q", name)
		}
	}

	key := tree.Key()
	write("src/lib.rs", "changed")
	changed, err := Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Key() == key {
		t.Error("the key stays the same after a file changes")
	}
}

// fakeShell records scripts and answers the check for earlier copies with
// the copies it has made.
type fakeShell struct {
	made    map[string]bool
	scripts []string
}

func (f *fakeShell) Name() string { return "fake" }

func (f *fakeShell) Run(_ context.Context, _ io.Reader, script string) (string, error) {
	f.scripts = append(f.scripts, script)
	if checks := checkPattern.FindAllStringSubmatch(script, -1); checks != nil {
		var out []string
		for _, c := range checks {
			if !f.made[strings.TrimSuffix(c[1], "/"+completeMarker)] {
				out = append(out, c[2])
			}
		}
		return strings.Join(out, "\n"), nil
	}
	if strings.HasPrefix(script, "touch ") {
		f.made[strings.Fields(script)[5]] = true
	}
	return "", nil
}

var checkPattern = regexp.MustCompile(`if \[ -e (\S+) \]; then touch \S+; else echo (\d+); fi`)

func TestCopyMissingCopiesOnce(t *testing.T) {
	sh := &fakeShell{made: map[string]bool{"/d/a": true}}
	var filled []string
	copies := []Copy{
		{Dir: "/d/a", Fill: func(context.Context, string) error { filled = append(filled, "a"); return nil }},
		{Dir: "/d/b", Fill: func(context.Context, string) error { filled = append(filled, "b"); return nil }},
	}
	locks := 0
	lock := func(string) (func(), error) { locks++; return func() {}, nil }
	if err := CopyMissing(context.Background(), sh, copies, lock); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(filled, []string{"b"}) || locks != 1 {
		t.Fatalf("filled %v under %d locks; want only b, under one", filled, locks)
	}
	filled = nil
	if err := CopyMissing(context.Background(), sh, copies, lock); err != nil {
		t.Fatal(err)
	}
	if len(filled) != 0 {
		t.Errorf("filled %v again", filled)
	}
}

func TestPlanSyncsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "launcher.sh")
	syncedFile := filepath.Join(dir, "test-bin")
	syncedDir := filepath.Join(dir, "pkg")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(syncedFile, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(syncedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncedDir, "fixture.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	var files, trees []string
	copies, mapping, err := Plan(
		Spec{Program: program, Syncs: []string{syncedFile, syncedDir}},
		"/dev/root",
		func(_ context.Context, host, _ string) error {
			files = append(files, host)
			return nil
		},
		func(_ context.Context, tree *Tree, _ string) error {
			trees = append(trees, tree.Root)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 3 {
		t.Fatalf("got %d copies, want 3", len(copies))
	}
	for _, c := range copies {
		if err := c.Fill(context.Background(), c.Dir); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(files, []string{program, syncedFile}) {
		t.Errorf("sendFile called with %v, want [%s %s]", files, program, syncedFile)
	}
	if !slices.Equal(trees, []string{syncedDir}) {
		t.Errorf("sendTree called with %v, want [%s]", trees, syncedDir)
	}
	if got := mapping.Rewrite(syncedFile); !strings.HasSuffix(got, "/test-bin") || !strings.HasPrefix(got, "/dev/root/sync/") {
		t.Errorf("synced file mapped to %q", got)
	}
	if got := mapping.Rewrite(filepath.Join(syncedDir, "fixture.txt")); !strings.HasSuffix(got, "/fixture.txt") || !strings.HasPrefix(got, "/dev/root/sync/") {
		t.Errorf("synced dir child mapped to %q", got)
	}
}
