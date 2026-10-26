package devices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sargunv/devices-cli/internal/config"
	"github.com/sargunv/devices-cli/internal/state"
)

// fakeKind tracks instances in memory: an instance is alive until it is
// stopped or marked dead.
type fakeKind struct {
	mu       sync.Mutex
	alive    map[string]bool
	stopped  []string
	removed  []string
	started  int
	bootErr  error
	pinError map[string]error
}

func newFakeKind() *fakeKind {
	return &fakeKind{alive: map[string]bool{}, pinError: map[string]error{}}
}

func (k *fakeKind) Alive(_ context.Context, inst state.Instance) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.alive[inst.ID]
}

func (k *fakeKind) Stop(_ context.Context, inst state.Instance) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.alive, inst.ID)
	k.stopped = append(k.stopped, inst.ID)
	return nil
}

func (k *fakeKind) RemoveDefinition(name string) error {
	k.removed = append(k.removed, name)
	return nil
}

type fakeRequest struct {
	kind *fakeKind
	key  string
}

func (r fakeRequest) Kind() string                  { return "fake" }
func (r fakeRequest) Key() string                   { return r.key }
func (r fakeRequest) Description() string           { return "fake " + r.key }
func (r fakeRequest) Definition() string            { return "def-" + r.key }
func (r fakeRequest) Prepare(context.Context) error { return nil }

func (r fakeRequest) Start(_ context.Context, owner string, _ []state.Instance) (state.Instance, error) {
	r.kind.mu.Lock()
	defer r.kind.mu.Unlock()
	r.kind.started++
	id := fmt.Sprintf("fake-%d", r.kind.started)
	r.kind.alive[id] = true
	now := time.Now()
	return state.Instance{
		Owner: owner, Kind: "fake", Key: r.key, Definition: r.Definition(),
		Description: r.Description(), ID: id, Started: now, LastUsed: now,
	}, nil
}

func (r fakeRequest) WaitReady(context.Context, state.Instance) error { return r.kind.bootErr }

func (r fakeRequest) MatchPin(_ context.Context, id string) error {
	return r.kind.pinError[id]
}

func newManager(t *testing.T, kind *fakeKind) *Manager {
	t.Helper()
	return &Manager{
		Store:      state.Store{Dir: t.TempDir()},
		Config:     config.Config{MaxInstances: 2, IdleTimeout: time.Hour, DefinitionTTL: 24 * time.Hour},
		ConfigPath: "config.toml",
		Kinds:      map[string]Kind{"fake": kind},
		Log:        io.Discard,
		Poll:       10 * time.Millisecond,
		Now:        time.Now,
	}
}

func TestGetReusesTheOwnersInstance(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	a, b := t.TempDir(), t.TempDir()
	ctx := t.Context()

	first, err := m.Get(ctx, a, fakeRequest{kind, "x"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Get(ctx, a, fakeRequest{kind, "x"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Get(ctx, b, fakeRequest{kind, "x"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("same owner and request got %s, then %s", first, again)
	}
	if other == first {
		t.Errorf("two owners share %s", first)
	}
}

func TestConcurrentGetsForOneOwnerStartOneInstance(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	owner := t.TempDir()

	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Go(func() {
			// Each get opens the store itself, as separate processes do.
			m := *m
			id, err := m.Get(t.Context(), owner, fakeRequest{kind, "x"}, 0)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		})
	}
	wg.Wait()
	if kind.started != 1 {
		t.Errorf("started %d instances, want 1", kind.started)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("got IDs %v, want all the same", ids)
		}
	}
}

func TestGetFailsWhenAllSlotsAreInUse(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	m.Config.MaxInstances = 1
	if _, err := m.Get(t.Context(), t.TempDir(), fakeRequest{kind, "x"}, 0); err != nil {
		t.Fatal(err)
	}

	_, err := m.Get(t.Context(), t.TempDir(), fakeRequest{kind, "x"}, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "max_instances") {
		t.Fatalf("got %v, want an error that names max_instances", err)
	}
}

func TestGetWaitsForASlot(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	m.Config.MaxInstances = 1
	holder := t.TempDir()
	if _, err := m.Get(t.Context(), holder, fakeRequest{kind, "x"}, 0); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := m.Release(context.Background(), holder); err != nil {
			t.Error(err)
		}
	}()
	if _, err := m.Get(t.Context(), t.TempDir(), fakeRequest{kind, "x"}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestGetDiscardsAnInstanceThatFailsToBoot(t *testing.T) {
	kind := newFakeKind()
	kind.bootErr = errors.New("no boot")
	m := newManager(t, kind)

	if _, err := m.Get(t.Context(), t.TempDir(), fakeRequest{kind, "x"}, 0); err == nil {
		t.Fatal("got no error")
	}
	st, err := m.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Instances) != 0 || len(kind.stopped) != 1 {
		t.Errorf("instances %v, stopped %v; want the instance stopped and forgotten", st.Instances, kind.stopped)
	}
}

func TestCollect(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	now := time.Now()
	gone := filepath.Join(t.TempDir(), "gone")
	present := t.TempDir()
	kind.alive["dead"] = false
	for _, id := range []string{"gone", "idle", "live"} {
		kind.alive[id] = true
	}
	st := &state.State{
		Instances: []state.Instance{
			{Owner: gone, Kind: "fake", ID: "gone", Definition: "used", LastUsed: now},
			{Owner: present, Kind: "fake", ID: "idle", LastUsed: now.Add(-2 * time.Hour)},
			{Owner: present, Kind: "fake", ID: "dead", LastUsed: now},
			{Owner: present, Kind: "fake", ID: "live", Definition: "used", LastUsed: now},
			{Owner: gone, Kind: "newer", ID: "unknown", LastUsed: now},
		},
		Pins: []state.Pin{
			{Owner: gone, Kind: "fake", ID: "gone-pin"},
			{Owner: present, Kind: "fake", ID: "kept-pin"},
		},
		Definitions: []state.Definition{
			{Kind: "fake", Name: "used", LastUsed: now.Add(-48 * time.Hour)},
			{Kind: "fake", Name: "stale", LastUsed: now.Add(-48 * time.Hour)},
			{Kind: "fake", Name: "fresh", LastUsed: now},
		},
	}

	m.collect(t.Context(), st)

	var ids, pins, defs []string
	for _, in := range st.Instances {
		ids = append(ids, in.ID)
	}
	for _, p := range st.Pins {
		pins = append(pins, p.ID)
	}
	for _, d := range st.Definitions {
		defs = append(defs, d.Name)
	}
	check := func(what string, got []string, want string) {
		if strings.Join(got, " ") != want {
			t.Errorf("%s: got %v, want %s", what, got, want)
		}
	}
	check("instances", ids, "live unknown")
	check("stopped", kind.stopped, "gone idle dead")
	check("pins", pins, "kept-pin")
	check("definitions", defs, "used fresh")
	check("removed definitions", kind.removed, "stale")
}

func TestPinnedDeviceGoesOnlyToItsOwner(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	a, b := t.TempDir(), t.TempDir()
	ctx := t.Context()
	if err := m.Assign(ctx, a, "fake", "phone"); err != nil {
		t.Fatal(err)
	}

	if id, err := m.Get(ctx, a, fakeRequest{kind, "x"}, 0); err != nil || id != "phone" {
		t.Errorf("owner got %q, %v; want phone", id, err)
	}
	if id, err := m.Get(ctx, b, fakeRequest{kind, "x"}, 0); err != nil || id == "phone" {
		t.Errorf("other owner got %q, %v; want an instance", id, err)
	}
	if err := m.Assign(ctx, b, "fake", "phone"); err == nil {
		t.Error("assigned a device that another owner holds")
	}

	kind.pinError["phone"] = errors.New("wrong API level")
	if id, err := m.Get(ctx, a, fakeRequest{kind, "x"}, 0); err != nil || id == "phone" {
		t.Errorf("owner got %q, %v for a request the device does not match; want an instance", id, err)
	}

	if err := m.Release(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := m.Assign(ctx, b, "fake", "phone"); err != nil {
		t.Errorf("released device stays assigned: %v", err)
	}
}

func TestAssignRefusesInstances(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	owner := t.TempDir()
	id, err := m.Get(t.Context(), owner, fakeRequest{kind, "x"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Assign(t.Context(), owner, "fake", id); err == nil {
		t.Errorf("assigned instance %s", id)
	}
}

func TestReleaseStopsOnlyTheOwnersInstances(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	a, b := t.TempDir(), t.TempDir()
	ctx := t.Context()
	idA, _ := m.Get(ctx, a, fakeRequest{kind, "x"}, 0)
	idB, _ := m.Get(ctx, b, fakeRequest{kind, "x"}, 0)

	if err := m.Release(ctx, a); err != nil {
		t.Fatal(err)
	}
	if !kind.alive[idB] || kind.alive[idA] {
		t.Errorf("alive after release: %v; want only %s", kind.alive, idB)
	}
}

func TestGetOfAPinnedDeviceCollectsFirst(t *testing.T) {
	kind := newFakeKind()
	m := newManager(t, kind)
	ctx := t.Context()
	gone := t.TempDir()
	if _, err := m.Get(ctx, gone, fakeRequest{kind, "x"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	owner := t.TempDir()
	if err := m.Store.Update(func(st *state.State) error {
		st.Pins = append(st.Pins, state.Pin{Owner: owner, Kind: "fake", ID: "phone"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if id, err := m.Get(ctx, owner, fakeRequest{kind, "x"}, 0); err != nil || id != "phone" {
		t.Fatalf("got %q, %v; want phone", id, err)
	}
	if len(kind.stopped) != 1 {
		t.Errorf("stopped %v; want the instance whose owner is gone", kind.stopped)
	}
}
