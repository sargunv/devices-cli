package owner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(sub)
	if got, err := Resolve(""); err != nil || got != repo {
		t.Errorf("Resolve in a worktree = %q, %v; want %q", got, err, repo)
	}
	if got, err := Resolve("."); err != nil || got != sub {
		t.Errorf("Resolve(.) = %q, %v; want %q", got, err, sub)
	}

	t.Chdir(t.TempDir())
	if _, err := Resolve(""); err == nil {
		t.Error("Resolve outside a worktree succeeded")
	}
	if _, err := Resolve(filepath.Join(repo, "missing")); err == nil {
		t.Error("Resolve of a missing directory succeeded")
	}
}
