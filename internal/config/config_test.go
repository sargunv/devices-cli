package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg, err := Load(path)
	if err != nil || cfg != Defaults() {
		t.Errorf("missing file: got %+v, %v; want the defaults", cfg, err)
	}

	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("max_instances = 3\nidle_timeout = \"30m\"\ndefinition_ttl_days = 0\n")
	cfg, err = Load(path)
	want := Config{MaxInstances: 3, IdleTimeout: 30 * time.Minute, DefinitionTTL: 0}
	if err != nil || cfg != want {
		t.Errorf("got %+v, %v; want %+v", cfg, err, want)
	}

	for _, bad := range []string{"max_instances = 0", "idle_timeout = \"soon\"", "max_instance = 2"} {
		write(bad)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: got no error", bad)
		}
	}
}

func TestDefaultMaxInstances(t *testing.T) {
	for memory, want := range map[uint64]int{0: 1, 4 << 30: 1, 32 << 30: 4, 128 << 30: 16} {
		if got := defaultMaxInstances(memory); got != want {
			t.Errorf("defaultMaxInstances(%d GiB) = %d, want %d", memory>>30, got, want)
		}
	}
}
