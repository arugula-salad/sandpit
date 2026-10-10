package substrate

import (
	"context"
	"net/http"
	"time"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/netpolicy"
	"github.com/arugula-salad/sandpit/internal/store"
)

// The rest of engine.Backend. What Agent Substrate can't do (yet) answers
// engine.ErrUnsupported, which the APIs turn into 501.

var _ engine.Backend = (*Engine)(nil)

// Status is "running" while this engine has the actor resumed, "warm" once it
// has a snapshot of its own (every actor that has started), else "cold".
func (e *Engine) Status(rec store.Record) string {
	e.mu.Lock()
	_, running := e.running[rec.ID]
	e.mu.Unlock()
	switch {
	case running:
		return "running"
	case extOf(rec).Started:
		return "warm"
	}
	return "cold"
}

// BeginUse counts a use of a running sandbox, as Acquire does, so the idle
// rule leaves it alone until end.
func (e *Engine) BeginUse(id string) (end func()) {
	if g := e.use(id); g != nil {
		return g.release
	}
	return func() {}
}

func (e *Engine) Quitting() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.quitting
}

// StartReaping: the janitor already takes deadlines from the start.
func (e *Engine) StartReaping() {}

// ChangeDeadline edits the record's deadline the way the Firecracker engine's
// leases do, minus the warnings ahead of it.
func (e *Engine) ChangeDeadline(id string, change func(*engine.Deadline)) (store.Record, error) {
	defer e.lock(id)()
	return e.store.UpdateRecord(id, func(r *store.Record) {
		d := engine.Deadline{At: r.ExpiresAt, Protected: r.Protected, Action: r.Lifecycle.OnDeadline()}
		change(&d)
		r.ExpiresAt, r.Protected = d.At, d.Protected
		p := store.LifecyclePolicy{}
		if r.Lifecycle != nil {
			p = *r.Lifecycle
		}
		p.DeadlineAction = d.Action
		r.Lifecycle = &p
		r.UpdatedAt = time.Now().UTC()
	})
}

// SetPolicy stores the lifecycle policy. The idle rule here is the engine's
// own (Options.IdleSuspend); per-sandbox auto-stop is a later step.
func (e *Engine) SetPolicy(id string, p store.LifecyclePolicy) (store.Record, error) {
	defer e.lock(id)()
	return e.store.UpdateRecord(id, func(r *store.Record) {
		r.Lifecycle = &p
		r.UpdatedAt = time.Now().UTC()
	})
}

// ApplyPolicy has nothing to push into a guest.
func (e *Engine) ApplyPolicy(context.Context, store.Record) error { return nil }

func (e *Engine) SetNetworkPolicy(string, []store.NetworkRule, *netpolicy.Policy) error {
	return engine.ErrUnsupported
}

func (e *Engine) RepublishNetworkPolicy(string) {}

func (e *Engine) CreateCheckpoint(store.Record, *engine.GuestChan, string, engine.Progress) (store.Checkpoint, error) {
	return store.Checkpoint{}, engine.ErrUnsupported
}

func (e *Engine) DeleteCheckpoint(store.Record, string) error { return engine.ErrUnsupported }

func (e *Engine) HoldCheckpoint(store.Record, string) (store.Record, string, func(), error) {
	return store.Record{}, "", nil, engine.ErrUnsupported
}

func (e *Engine) RestoreCheckpoint(store.Record, *engine.GuestChan, string, engine.Progress, func()) error {
	return engine.ErrUnsupported
}

func (e *Engine) MountCheckpoint(context.Context, store.Record, *engine.GuestChan, string) (int, error) {
	return 0, engine.ErrUnsupported
}

func (e *Engine) UnmountCheckpoint(context.Context, store.Record, *engine.GuestChan, string) error {
	return engine.ErrUnsupported
}

// No backups (the actor's snapshot lives in the cluster's object store) and
// no image cache (an ActorTemplate names its image).
func (e *Engine) BackupState(string) *engine.BackupState                         { return nil }
func (e *Engine) SetBackupFilter(func(store.Sprite) bool)                        {}
func (e *Engine) Images() *engine.ImageCache                                     { return nil }
func (e *Engine) Events() *engine.Bus                                            { return e.events }
func (e *Engine) SetGuestAPI(func(store.Record, *engine.GuestChan) http.Handler) {}

// Emit publishes an event about rec, named by the describer as the
// Firecracker engine's are.
func (e *Engine) Emit(rec store.Record, typ string, detail map[string]any) {
	ev := engine.Event{Type: typ, SpriteID: rec.ID, Detail: detail}
	e.mu.Lock()
	d := e.describer
	e.mu.Unlock()
	if d != nil {
		if sp, err := e.store.Get(rec.ID); err == nil {
			ev.Sprite, ev.ParentID = d(sp)
		}
	}
	e.events.Publish(ev)
}

func (e *Engine) SetDescriber(f engine.Describer) {
	e.mu.Lock()
	e.describer = f
	e.mu.Unlock()
}

func (e *Engine) OnDelete(f func(store.Sprite)) {
	e.mu.Lock()
	e.onDelete = append(e.onDelete, f)
	e.mu.Unlock()
}
