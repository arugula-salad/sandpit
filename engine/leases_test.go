package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/arugula-salad/sandpit/internal/store"
)

// expiresAt gives a sprite a lease that runs out at at, which no API call will
// do for a moment already past (it is refused as a typo).
func expiresAt(at time.Time) func(*store.Sprite) {
	return func(sp *store.Sprite) { sp.ExpiresAt = &at }
}

// The reaper picks candidates from a list read before it waits on each sprite's
// lock, exactly as the warm-TTL janitor does. A renewal that got the lock first
// has already written a new deadline by the time the reaper gets in, and the
// reaper has to see it: deciding on its own stale copy would delete a workspace
// somebody just extended.
func TestARenewalThatLandsFirstBeatsTheReaper(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := createSprite(t, l, "game", expiresAt(time.Now().Add(-time.Second)))

	rt := l.rt(sp.ID)
	rt.mu.Lock() // a renewal (or any other transition) in flight
	done := make(chan struct{})
	go func() {
		l.leases.reap(sp.Record) // the reaper, holding the record it listed
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the reaper deleted the sprite without waiting for its lock")
	case <-time.After(50 * time.Millisecond):
	}
	// What a renewal (ChangeDeadline) does before letting go.
	later := time.Now().Add(time.Hour)
	l.store.UpdateByName(store.Sprites, "game", func(sp *store.Sprite) { sp.ExpiresAt = &later })
	rt.mu.Unlock()
	<-done

	if _, err := l.store.GetByName(store.Sprites, "game"); err != nil {
		t.Fatal("the reaper deleted a sprite renewed moments before, deciding on a stale lease")
	}
}

// The other order: once a reap has committed, the sprite is on its way out and
// every change to its deadline is refused rather than written onto a record
// that is about to go.
func TestADeadlineChangeAfterTheReapIsRefused(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := createSprite(t, l, "game", expiresAt(time.Now().Add(-time.Second)))
	l.leases.claim(sp.ID) // the reaper has committed and is deleting

	later := time.Now().Add(time.Hour)
	for what, change := range map[string]func() error{
		"renew":     func() error { _, err := l.ChangeDeadline(sp.ID, func(d *Deadline) { d.At = &later }); return err },
		"protect":   func() error { _, err := l.ChangeDeadline(sp.ID, func(d *Deadline) { d.Protected = true }); return err },
		"clear":     func() error { _, err := l.ChangeDeadline(sp.ID, func(d *Deadline) { d.At = nil }); return err },
		"deadline":  func() error { _, err := l.SetDeadline(sp.ID, &later, store.DeadlineDelete); return err },
		"lifecycle": func() error { _, err := l.SetPolicy(sp.ID, store.LifecyclePolicy{}); return err },
	} {
		if err := change(); !errors.Is(err, ErrLeaseReaping) {
			t.Errorf("%s: %v, want ErrLeaseReaping", what, err)
		}
	}
	if cur, _ := l.store.GetRecord(sp.ID); cur.Protected || cur.ExpiresAt == nil || !cur.ExpiresAt.Equal(*sp.ExpiresAt) {
		t.Fatalf("a refused change still changed the record: %+v", cur)
	}
}

// The guest is told once per deadline, when sprite.expiring goes out, so it
// can save what the deletion would lose.
func TestTheGuestIsToldItsLeaseIsRunningOut(t *testing.T) {
	l := newTestEngine(t, Options{})
	var told []time.Time
	l.leases.notify = func(_ store.Record, at time.Time) { told = append(told, at) }
	at := time.Now().Add(time.Minute)
	sp := createSprite(t, l, "game", expiresAt(at))
	l.leases.warn(sp.Record, time.Now())
	l.leases.sweep()
	if len(told) != 1 || !told[0].Equal(at) {
		t.Fatalf("told %v, want once for %s", told, at)
	}
	far := createSprite(t, l, "later", expiresAt(time.Now().Add(time.Hour)))
	l.leases.warn(far.Record, time.Now())
	if len(told) != 1 {
		t.Fatalf("told about a lease outside the warning window: %v", told)
	}
}
