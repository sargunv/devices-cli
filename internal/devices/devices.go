// Package devices allocates devices to owners: it reuses an owner's running
// instance, starts one within the machine-wide cap, hands out pinned physical
// devices, and collects instances and definitions that are no longer needed.
package devices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/owner"
	"github.com/sargunv/devices-cli/internal/state"
)

// Request describes the device a get asks for.
type Request interface {
	Kind() string
	// Key identifies the request among one owner's instances.
	Key() string
	// Description summarizes the request for people.
	Description() string
	// Definition names the machine-wide definition the request runs, or is
	// empty when its kind has none.
	Definition() string
	// Prepare creates the definition when it is missing. It may be slow, and
	// it runs under a lock for the definition but not the state lock.
	Prepare(ctx context.Context) error
	// Start launches an instance and returns without waiting for it to boot.
	// It runs under the state lock and sees every recorded instance.
	Start(ctx context.Context, owner string, running []state.Instance) (state.Instance, error)
	// WaitReady blocks until the instance can be used.
	WaitReady(ctx context.Context, inst state.Instance) error
}

// PinMatcher is a request that a pinned physical device can satisfy.
type PinMatcher interface {
	// MatchPin returns nil when the connected device id satisfies the request,
	// and otherwise an error that says why not.
	MatchPin(ctx context.Context, id string) error
}

// Kind manages the instances and definitions of one kind of device.
type Kind interface {
	// Alive reports whether the instance still runs.
	Alive(ctx context.Context, inst state.Instance) bool
	// Stop stops the instance and removes what it left behind. It succeeds for
	// an instance that has already stopped.
	Stop(ctx context.Context, inst state.Instance) error
	// RemoveDefinition deletes a definition that this kind created.
	RemoveDefinition(name string) error
}

// Manager allocates devices from the machine-wide state.
type Manager struct {
	Store  state.Store
	Config config.Config
	// ConfigPath is named in messages that suggest changing a setting.
	ConfigPath string
	Kinds      map[string]Kind
	// Log receives progress and collection messages.
	Log io.Writer
	// Poll is how often a get waiting for a free slot checks again.
	Poll time.Duration
	Now  func() time.Time
}

func (m *Manager) logf(format string, args ...any) {
	fmt.Fprintf(m.Log, "devices-cli: "+format+"\n", args...)
}

var errFull = errors.New("all device slots are in use")

// Get returns the ID of a device for the owner that satisfies the request,
// starting an instance if the owner has none. When every slot is in use, it
// waits up to wait for one to free up.
func (m *Manager) Get(ctx context.Context, ownerPath string, req Request, wait time.Duration) (string, error) {
	if id, ok, err := m.pinned(ctx, ownerPath, req); err != nil || ok {
		return id, err
	}
	if err := m.prepare(ctx, req); err != nil {
		return "", err
	}

	deadline := m.Now().Add(wait)
	waiting := false
	for {
		var inst state.Instance
		started := false
		err := m.Store.Update(func(st *state.State) error {
			m.collect(ctx, st)
			now := m.Now()
			if found := st.FindInstance(ownerPath, req.Key()); found != nil {
				found.LastUsed = now
				inst = *found
				return nil
			}
			if len(st.Instances) >= m.Config.MaxInstances {
				return errFull
			}
			var err error
			inst, err = req.Start(ctx, ownerPath, st.Instances)
			if err != nil {
				return err
			}
			st.Instances = append(st.Instances, inst)
			if def := req.Definition(); def != "" {
				st.TouchDefinition(req.Kind(), def, now)
			}
			started = true
			return nil
		})
		if errors.Is(err, errFull) {
			if !m.Now().Before(deadline) {
				return "", m.fullError(wait)
			}
			if !waiting {
				m.logf("all %d device slots are in use; waiting up to %s for one (see `devices-cli ls`)", m.Config.MaxInstances, wait)
				waiting = true
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(m.Poll):
			}
			continue
		}
		if err != nil {
			return "", err
		}

		if started {
			m.logf("started %s %s for %s; waiting for it to boot", inst.Description, inst.ID, inst.Owner)
		}
		if err := req.WaitReady(ctx, inst); err != nil {
			if started {
				m.discard(ctx, inst)
			}
			return "", err
		}
		return inst.ID, nil
	}
}

func (m *Manager) fullError(wait time.Duration) error {
	return fmt.Errorf("all %d device slots are still in use after waiting %s.\n"+
		"See who holds them with `devices-cli ls`, free one with `devices-cli release` in its worktree,\n"+
		"wait longer with --wait, or raise max_instances in %s", m.Config.MaxInstances, wait, m.ConfigPath)
}

// prepare records the definition as used, so a concurrent gc keeps it, and
// then creates it if it is missing.
func (m *Manager) prepare(ctx context.Context, req Request) error {
	def := req.Definition()
	if def == "" {
		return req.Prepare(ctx)
	}
	err := m.Store.Update(func(st *state.State) error {
		st.TouchDefinition(req.Kind(), def, m.Now())
		return nil
	})
	if err != nil {
		return err
	}
	unlock, err := m.Store.Lock(def)
	if err != nil {
		return err
	}
	defer unlock()
	return req.Prepare(ctx)
}

// discard stops an instance that this get started but that failed to boot,
// unless another command already removed it and its ID went to a new one.
func (m *Manager) discard(ctx context.Context, inst state.Instance) {
	err := m.Store.Update(func(st *state.State) error {
		recorded := st.FindInstance(inst.Owner, inst.Key)
		if recorded == nil || recorded.ID != inst.ID || !recorded.Started.Equal(inst.Started) {
			return nil
		}
		if err := m.Kinds[inst.Kind].Stop(ctx, inst); err != nil {
			return err
		}
		st.RemoveInstance(inst.Kind, inst.ID)
		return nil
	})
	if err != nil {
		m.logf("could not stop %s: %v", inst.ID, err)
	}
}

// pinned returns the owner's pinned device when it satisfies the request.
func (m *Manager) pinned(ctx context.Context, ownerPath string, req Request) (string, bool, error) {
	matcher, ok := req.(PinMatcher)
	if !ok {
		return "", false, nil
	}
	// Listing collects first, as every command does.
	st, err := m.List(ctx)
	if err != nil {
		return "", false, err
	}
	for _, pin := range st.Pins {
		if pin.Owner != ownerPath || pin.Kind != req.Kind() {
			continue
		}
		if err := matcher.MatchPin(ctx, pin.ID); err != nil {
			m.logf("not using assigned device %s: %v", pin.ID, err)
			continue
		}
		// The device may have been released while adb answered.
		stillPinned := false
		err := m.Store.Update(func(st *state.State) error {
			if p := st.FindPin(pin.Kind, pin.ID); p != nil && p.Owner == ownerPath {
				p.LastUsed = m.Now()
				stillPinned = true
			}
			return nil
		})
		if err != nil || stillPinned {
			return pin.ID, stillPinned, err
		}
	}
	return "", false, nil
}

// Assign pins a physical device to the owner. A device pinned to another owner
// stays pinned there.
func (m *Manager) Assign(ctx context.Context, ownerPath, kind, id string) error {
	return m.Store.Update(func(st *state.State) error {
		m.collect(ctx, st)
		if slices.ContainsFunc(st.Instances, func(in state.Instance) bool { return in.Kind == kind && in.ID == id }) {
			return fmt.Errorf("%s is an instance that devices-cli started; only physical devices can be assigned", id)
		}
		now := m.Now()
		if pin := st.FindPin(kind, id); pin != nil {
			if pin.Owner != ownerPath {
				return fmt.Errorf("%s is assigned to %s; run `devices-cli release --owner %s` to free it", id, pin.Owner, pin.Owner)
			}
			pin.LastUsed = now
			return nil
		}
		st.Pins = append(st.Pins, state.Pin{Owner: ownerPath, Kind: kind, ID: id, Assigned: now, LastUsed: now})
		return nil
	})
}

// Release stops the owner's instances and unpins its devices.
func (m *Manager) Release(ctx context.Context, ownerPath string) error {
	return m.ReleaseKind(ctx, ownerPath, "")
}

// ReleaseKind releases the owner's devices of one kind; an empty kind releases all.
func (m *Manager) ReleaseKind(ctx context.Context, ownerPath, kind string) error {
	return m.Store.Update(func(st *state.State) error {
		m.collect(ctx, st)
		var errs []error
		st.Instances = slices.DeleteFunc(st.Instances, func(in state.Instance) bool {
			if in.Owner != ownerPath || (kind != "" && in.Kind != kind) {
				return false
			}
			if err := m.stop(ctx, in); err != nil {
				errs = append(errs, err)
				return false
			}
			m.logf("stopped %s %s", in.Description, in.ID)
			return true
		})
		st.Pins = slices.DeleteFunc(st.Pins, func(p state.Pin) bool {
			if p.Owner != ownerPath || (kind != "" && p.Kind != kind) {
				return false
			}
			m.logf("unassigned %s", p.ID)
			return true
		})
		return errors.Join(errs...)
	})
}

// List returns the state after collecting what is no longer needed.
func (m *Manager) List(ctx context.Context) (*state.State, error) {
	var snapshot state.State
	err := m.Store.Update(func(st *state.State) error {
		m.collect(ctx, st)
		snapshot = *st
		return nil
	})
	return &snapshot, err
}

// Collect stops instances and removes pins and definitions that are no longer
// needed. Every command does this first.
func (m *Manager) Collect(ctx context.Context) error {
	return m.Store.Update(func(st *state.State) error {
		m.collect(ctx, st)
		return nil
	})
}

func (m *Manager) collect(ctx context.Context, st *state.State) {
	now := m.Now()
	st.Instances = slices.DeleteFunc(st.Instances, func(in state.Instance) bool {
		kind, known := m.Kinds[in.Kind]
		var reason string
		switch {
		case !known:
			// Recorded by a newer devices-cli; leave it to that version.
			return false
		case !owner.Exists(in.Owner):
			reason = "its owner " + in.Owner + " is gone"
		case m.Config.IdleTimeout > 0 && now.Sub(in.LastUsed) > m.Config.IdleTimeout:
			reason = "it was idle for " + now.Sub(in.LastUsed).Round(time.Second).String()
		case !kind.Alive(ctx, in):
			reason = "it is no longer running"
		default:
			return false
		}
		if err := m.stop(ctx, in); err != nil {
			m.logf("%v", err)
			return false
		}
		m.logf("removed %s %s of %s: %s", in.Description, in.ID, in.Owner, reason)
		return true
	})

	st.Pins = slices.DeleteFunc(st.Pins, func(p state.Pin) bool {
		if owner.Exists(p.Owner) {
			return false
		}
		m.logf("unassigned %s: its owner %s is gone", p.ID, p.Owner)
		return true
	})

	if m.Config.DefinitionTTL <= 0 {
		return
	}
	st.Definitions = slices.DeleteFunc(st.Definitions, func(d state.Definition) bool {
		kind, known := m.Kinds[d.Kind]
		inUse := slices.ContainsFunc(st.Instances, func(in state.Instance) bool {
			return in.Kind == d.Kind && in.Definition == d.Name
		})
		if !known || inUse || now.Sub(d.LastUsed) <= m.Config.DefinitionTTL {
			return false
		}
		if err := kind.RemoveDefinition(d.Name); err != nil {
			m.logf("could not remove definition %s: %v", d.Name, err)
			return false
		}
		m.logf("removed definition %s: unused for %d days", d.Name, int(now.Sub(d.LastUsed).Hours()/24))
		return true
	})
}

func (m *Manager) stop(ctx context.Context, in state.Instance) error {
	if err := m.Kinds[in.Kind].Stop(ctx, in); err != nil {
		return fmt.Errorf("could not stop %s %s: %w", in.Description, in.ID, err)
	}
	return nil
}
