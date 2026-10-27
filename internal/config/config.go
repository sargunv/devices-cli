// Package config loads the machine-wide settings and locates the directories
// devices-cli uses.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds the machine-wide settings.
type Config struct {
	// MaxInstances is the most instances that may run at once, of all kinds.
	MaxInstances int
	// IdleTimeout stops an instance that no get has asked for in this long.
	// Zero never stops an instance for being idle.
	IdleTimeout time.Duration
	// DefinitionTTL removes a definition that no get has asked for in this
	// long. Zero never removes definitions.
	DefinitionTTL time.Duration
}

type file struct {
	MaxInstances      *int    `toml:"max_instances"`
	IdleTimeout       *string `toml:"idle_timeout"`
	DefinitionTTLDays *int    `toml:"definition_ttl_days"`
}

// Defaults returns the settings used when the config file sets none.
func Defaults() Config {
	return Config{
		MaxInstances:  defaultMaxInstances(totalMemory()),
		IdleTimeout:   2 * time.Hour,
		DefinitionTTL: 14 * 24 * time.Hour,
	}
}

// One instance per 8 GiB of RAM leaves room for the host and the builds that
// feed the devices.
func defaultMaxInstances(memory uint64) int {
	return max(1, int(memory/(8<<30)))
}

// Path returns the config file path.
func Path() (string, error) {
	dir, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "devices-cli", "config.toml"), nil
}

// StateDir returns the directory for the state file, locks, and logs.
func StateDir() (string, error) {
	dir, err := xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "devices-cli"), nil
}

// CacheDir returns the user's cache directory, which tools such as anyvm share.
func CacheDir() (string, error) {
	return xdgDir("XDG_CACHE_HOME", ".cache")
}

func xdgDir(env, fallback string) (string, error) {
	if dir := os.Getenv(env); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback), nil
}

// Load reads the config file at path over the defaults. A missing file is not
// an error.
func Load(path string) (Config, error) {
	cfg := Defaults()
	var f file
	md, err := toml.DecodeFile(path, &f)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return cfg, fmt.Errorf("%s: unknown setting %q", path, keys[0].String())
	}

	if f.MaxInstances != nil {
		if *f.MaxInstances < 1 {
			return cfg, fmt.Errorf("%s: max_instances must be at least 1", path)
		}
		cfg.MaxInstances = *f.MaxInstances
	}
	if f.IdleTimeout != nil {
		d, err := time.ParseDuration(*f.IdleTimeout)
		if err != nil || d < 0 {
			return cfg, fmt.Errorf("%s: idle_timeout must be a duration such as \"2h\" or \"0\"", path)
		}
		cfg.IdleTimeout = d
	}
	if f.DefinitionTTLDays != nil {
		if *f.DefinitionTTLDays < 0 {
			return cfg, fmt.Errorf("%s: definition_ttl_days must be 0 or more", path)
		}
		cfg.DefinitionTTL = time.Duration(*f.DefinitionTTLDays) * 24 * time.Hour
	}
	return cfg, nil
}
