// Package state stores the machine-wide record of running instances, pinned
// physical devices, and device definitions, guarded by a file lock so that
// concurrent devices-cli processes see a consistent view.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// Instance is a device that devices-cli started for one owner.
type Instance struct {
	Owner string `json:"owner"`
	Kind  string `json:"kind"`
	// Key identifies the request the instance satisfies; a get that builds the
	// same key for the same owner reuses the instance.
	Key string `json:"key"`
	// Definition names the machine-wide definition the instance runs, if its
	// kind has one.
	Definition string `json:"definition,omitempty"`
	// Description is a human-readable summary of the request.
	Description string `json:"description"`
	// ID is what get prints: an adb serial or a simulator UDID.
	ID       string    `json:"id"`
	PID      int       `json:"pid,omitempty"`
	Port     int       `json:"port,omitempty"`
	Log      string    `json:"log,omitempty"`
	Started  time.Time `json:"started"`
	LastUsed time.Time `json:"lastUsed"`
}

// Pin assigns a physical device to one owner.
type Pin struct {
	Owner    string    `json:"owner"`
	Kind     string    `json:"kind"`
	ID       string    `json:"id"`
	Assigned time.Time `json:"assigned"`
	LastUsed time.Time `json:"lastUsed"`
}

// Definition is a machine-wide device definition that devices-cli created,
// such as an Android Virtual Device.
type Definition struct {
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed"`
}

// State is the content of the state file.
type State struct {
	Instances   []Instance   `json:"instances"`
	Pins        []Pin        `json:"pins"`
	Definitions []Definition `json:"definitions"`
}

// Store reads and writes the state file in a directory.
type Store struct {
	Dir string
}

func (s Store) path() string { return filepath.Join(s.Dir, "state.json") }

// Update locks the state, passes it to fn, and saves it if fn succeeds. The
// lock is held while fn runs, so fn must not wait on anything slow.
func (s Store) Update(fn func(*State) error) error {
	unlock, err := s.lock("state.lock")
	if err != nil {
		return err
	}
	defer unlock()

	st, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.save(st)
}

// Read returns a snapshot of the state.
func (s Store) Read() (*State, error) {
	unlock, err := s.lock("state.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.load()
}

// Lock takes an exclusive lock named name, separate from the state lock, for
// slow work such as installing a definition. It returns the unlock function.
func (s Store) Lock(name string) (func(), error) {
	return s.lock(filepath.Join("locks", name+".lock"))
}

func (s Store) lock(name string) (func(), error) {
	path := filepath.Join(s.Dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

func (s Store) load() (*State, error) {
	var st State
	data, err := os.ReadFile(s.path())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("read %s: %w", s.path(), err)
		}
	}
	// Empty lists stay lists in ls --json.
	if st.Instances == nil {
		st.Instances = []Instance{}
	}
	if st.Pins == nil {
		st.Pins = []Pin{}
	}
	if st.Definitions == nil {
		st.Definitions = []Definition{}
	}
	return &st, nil
}

func (s Store) save(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path())
}

// FindInstance returns the owner's instance for key, or nil.
func (st *State) FindInstance(owner, key string) *Instance {
	i := slices.IndexFunc(st.Instances, func(in Instance) bool {
		return in.Owner == owner && in.Key == key
	})
	if i < 0 {
		return nil
	}
	return &st.Instances[i]
}

// RemoveInstance removes the instance with the given kind and ID.
func (st *State) RemoveInstance(kind, id string) {
	st.Instances = slices.DeleteFunc(st.Instances, func(in Instance) bool {
		return in.Kind == kind && in.ID == id
	})
}

// FindPin returns the pin for the device kind and ID, or nil.
func (st *State) FindPin(kind, id string) *Pin {
	i := slices.IndexFunc(st.Pins, func(p Pin) bool {
		return p.Kind == kind && p.ID == id
	})
	if i < 0 {
		return nil
	}
	return &st.Pins[i]
}

// TouchDefinition records that a definition is in use now, adding it if new.
func (st *State) TouchDefinition(kind, name string, now time.Time) {
	for i := range st.Definitions {
		if st.Definitions[i].Kind == kind && st.Definitions[i].Name == name {
			st.Definitions[i].LastUsed = now
			return
		}
	}
	st.Definitions = append(st.Definitions, Definition{
		Kind: kind, Name: name, Created: now, LastUsed: now,
	})
}
