package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/store"
)

// Checkpoints are Substrate tags. A tag captures the snapshot a suspended actor
// holds, memory and disk together, so a checkpoint suspends the sandbox first
// (the next request resumes it, processes and all). Restoring replaces the
// actor with one created from the tag, under the same name; creating a sandbox
// from a checkpoint creates its actor from the tag. The records keep the
// Firecracker engine's shape (v1, v2, ..., lineage), so the APIs list them the
// same way. Mounting one inside a sandbox stays ErrUnsupported.

// tagName is a checkpoint's tag: a short name from a hash, since sandbox IDs
// may hold characters a Substrate name can't.
func tagName(sandboxID, checkpoint string) string {
	h := sha256.Sum256([]byte(sandboxID + "/" + checkpoint))
	return "cp-" + hex.EncodeToString(h[:])[:24]
}

func (e *Engine) tagRef(sandboxID, checkpoint string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: e.opts.Atespace, Name: tagName(sandboxID, checkpoint)}
}

// inGuestCheckpointDelay is how long a checkpoint asked for from inside the
// sandbox waits before it is taken, for the answer to reach the guest first:
// taking it suspends the sandbox, which would cut the request off. Requests
// from outside wait for it (Acquire), so only the guest's own background
// work in this window lands in the checkpoint.
const inGuestCheckpointDelay = 200 * time.Millisecond

// CreateCheckpoint suspends the sandbox and tags its snapshot. Asked from
// inside (from is set), it answers at once with the checkpoint it is about to
// take, then takes it and resumes the sandbox: the guest sees a pause of about
// a second, and a stream attached across it ends.
func (e *Engine) CreateCheckpoint(rec store.Record, from *engine.GuestChan, comment string, info engine.Progress) (store.Checkpoint, error) {
	if info == nil {
		info = func(string, ...any) {}
	}
	if from != nil {
		return e.checkpointFromInside(rec, comment, info)
	}
	defer e.lock(rec.ID)()
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		return store.Checkpoint{}, err
	}
	start := time.Now()
	if err := e.suspendLocked(rec.ID, "checkpoint"); err != nil {
		return store.Checkpoint{}, err
	}
	cp := store.Checkpoint{ID: fmt.Sprintf("v%d", cur.NextCheckpoint+1), Comment: comment, History: cur.Lineage, CreateTime: start.UTC()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := e.tag(ctx, rec.ID, cp.ID); err != nil {
		return store.Checkpoint{}, err
	}
	info("Snapshot tagged in %s", time.Since(start).Round(time.Millisecond))
	cur, _ = e.store.UpdateRecord(rec.ID, func(r *store.Record) {
		r.NextCheckpoint++
		r.Lineage = append([]string{cp.ID}, r.Lineage...)
		r.Checkpoints = append(r.Checkpoints, cp)
		setExt(r, extOf(*r).tagged(cp.ID))
	})
	e.log.Info("checkpoint created", "id", rec.ID, "checkpoint", cp.ID)
	e.Emit(cur, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "auto": false})
	return cp, nil
}

// tag tags the snapshot the suspended actor of id holds as checkpoint.
func (e *Engine) tag(ctx context.Context, id, checkpoint string) error {
	if _, err := e.api.CreateTag(ctx, &ateapipb.CreateTagRequest{Tag: &ateapipb.Tag{
		Metadata:    &ateapipb.ResourceMetadata{Atespace: e.opts.Atespace, Name: tagName(id, checkpoint)},
		Scope:       ateapipb.TagScope_TAG_SCOPE_ATESPACE,
		SourceActor: e.ref(id),
	}}); err != nil {
		return fmt.Errorf("substrate: CreateTag: %w", err)
	}
	return nil
}

// checkpointFromInside records the checkpoint and answers, then takes it a
// moment later and resumes the sandbox. If taking it fails, the record goes.
func (e *Engine) checkpointFromInside(rec store.Record, comment string, info engine.Progress) (store.Checkpoint, error) {
	unlock := e.lock(rec.ID)
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		unlock()
		return store.Checkpoint{}, err
	}
	cp := store.Checkpoint{ID: fmt.Sprintf("v%d", cur.NextCheckpoint+1), Comment: comment, History: cur.Lineage, CreateTime: time.Now().UTC()}
	cur, _ = e.store.UpdateRecord(rec.ID, func(r *store.Record) {
		r.NextCheckpoint++
		r.Lineage = append([]string{cp.ID}, r.Lineage...)
		r.Checkpoints = append(r.Checkpoints, cp)
	})
	done := e.startPending(rec.ID, cp.ID)
	unlock()
	info("Checkpoint taken in a moment: the sprite pauses for it")
	go func() {
		time.Sleep(inGuestCheckpointDelay)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		unlock := e.lock(rec.ID)
		err := e.suspendLocked(rec.ID, "checkpoint")
		if err == nil {
			err = e.tag(ctx, rec.ID, cp.ID)
		}
		unlock()
		if err != nil {
			e.log.Error("checkpoint from inside failed", "id", rec.ID, "checkpoint", cp.ID, "err", err)
			e.store.UpdateRecord(rec.ID, func(r *store.Record) {
				r.Checkpoints = slices.DeleteFunc(r.Checkpoints, func(c store.Checkpoint) bool { return c.ID == cp.ID })
				r.Lineage = slices.DeleteFunc(r.Lineage, func(id string) bool { return id == cp.ID })
			})
		} else {
			e.log.Info("checkpoint created", "id", rec.ID, "checkpoint", cp.ID, "from", "inside")
			e.Emit(cur, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "auto": false})
		}
		done() // before resuming it: Acquire waits for pending checkpoints
		// It was running when it asked: carry on.
		if r, gerr := e.store.GetRecord(rec.ID); gerr == nil {
			if _, release, aerr := e.Acquire(ctx, r); aerr == nil {
				release()
			}
		}
	}()
	return cp, nil
}

// startPending marks a checkpoint asked for from inside as not yet taken;
// done unmarks it.
func (e *Engine) startPending(sandbox, checkpoint string) (done func()) {
	ch := make(chan struct{})
	key := sandbox + "/" + checkpoint
	e.mu.Lock()
	e.pending[key] = ch
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		delete(e.pending, key)
		e.mu.Unlock()
		close(ch)
	}
}

// waitPending waits for a checkpoint asked for from inside to be taken, if it
// has not been yet. Called without the sandbox's lock, which taking it needs.
func (e *Engine) waitPending(sandbox, checkpoint string) {
	e.mu.Lock()
	ch := e.pending[sandbox+"/"+checkpoint]
	e.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(2 * time.Minute):
		}
	}
}

// DeleteCheckpoint deletes its tag and its record.
func (e *Engine) DeleteCheckpoint(rec store.Record, id string) error {
	e.waitPending(rec.ID, id)
	defer e.lock(rec.ID)()
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		return err
	}
	if engine.FindCheckpoint(cur, id) == nil {
		return engine.ErrNoCheckpoint
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := e.api.DeleteTag(ctx, &ateapipb.DeleteTagRequest{Tag: e.tagRef(rec.ID, id)}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("substrate: DeleteTag: %w", err)
	}
	cur, _ = e.store.UpdateRecord(rec.ID, func(r *store.Record) {
		r.Checkpoints = slices.DeleteFunc(r.Checkpoints, func(cp store.Checkpoint) bool { return cp.ID == id })
	})
	e.Emit(cur, "checkpoint.deleted", map[string]any{"checkpoint": id})
	return nil
}

// HoldCheckpoint locks the sandbox and resolves one of its checkpoints (the
// newest when id is ""), as the Firecracker engine's does.
func (e *Engine) HoldCheckpoint(rec store.Record, id string) (store.Record, string, func(), error) {
	if id != "" {
		e.waitPending(rec.ID, id)
	} else {
		e.waitAllPending(rec.ID)
	}
	unlock := e.lock(rec.ID)
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		unlock()
		return cur, "", nil, err
	}
	cp := id
	if cp == "" {
		if cps := engine.ListCheckpoints(cur, "", false); len(cps) > 0 {
			cp = cps[0].ID
		}
	}
	if cp == "" || engine.FindCheckpoint(cur, cp) == nil {
		unlock()
		return cur, "", nil, engine.ErrNoCheckpoint
	}
	return cur, cp, unlock, nil
}

// RestoreCheckpoint puts the sandbox back to a checkpoint: its actor is
// replaced by one created from the checkpoint's tag, under the same name.
func (e *Engine) RestoreCheckpoint(rec store.Record, from *engine.GuestChan, id string, info engine.Progress, beforeStop func()) error {
	if info == nil {
		info = func(string, ...any) {}
	}
	e.waitPending(rec.ID, id)
	defer e.lock(rec.ID)()
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		return err
	}
	target := engine.FindCheckpoint(cur, id)
	if target == nil {
		return engine.ErrNoCheckpoint
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The tag first: the actor is only replaced once there is one to replace it from.
	if _, err := e.api.GetTag(ctx, &ateapipb.GetTagRequest{Tag: e.tagRef(rec.ID, id)}); err != nil {
		return fmt.Errorf("substrate: checkpoint %s: %w", id, err)
	}
	// Asked from inside, the guest is told first: saving the current state
	// below suspends the sandbox, which ends the request that asked.
	if from != nil && beforeStop != nil {
		beforeStop()
		beforeStop = nil
		time.Sleep(200 * time.Millisecond)
	}
	// What the restore throws away is kept as an automatic checkpoint, so a
	// restore can be undone, as on Firecracker.
	if err := e.autoCheckpointLocked(ctx, rec.ID, "before restore to "+id, id); err != nil {
		return fmt.Errorf("save current state first: %w", err)
	}
	if beforeStop != nil {
		beforeStop()
	}
	start := time.Now()
	if _, err := e.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: e.ref(rec.ID), AnyState: true}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("substrate: DeleteActor: %w", err)
	}
	e.mu.Lock()
	e.dropRunningLocked(rec.ID)
	e.mu.Unlock()
	e.mu.Lock()
	tmpl := e.templates[apiOf(cur.API)]
	e.mu.Unlock()
	if err := e.createActor(ctx, rec.ID, tmpl, e.tagRef(rec.ID, id)); err != nil {
		return err
	}
	info("Restored from %s in %s", id, time.Since(start).Round(time.Millisecond))
	lineage := append([]string{id}, target.History...)
	cur, _ = e.store.UpdateRecord(rec.ID, func(r *store.Record) {
		r.Lineage = lineage
		setExt(r, extOf(*r).atCheckpoint(id))
	})
	e.log.Info("checkpoint restored", "id", rec.ID, "checkpoint", id)
	e.Emit(cur, "checkpoint.restored", map[string]any{"checkpoint": id})
	return nil
}

// autoCheckpointLocked suspends the sandbox and tags its state as auto-N,
// keeping the newest AutoCheckpointKeep autos (and spare, a restore's target).
// The caller holds the sandbox's lock.
func (e *Engine) autoCheckpointLocked(ctx context.Context, id, comment, spare string) error {
	keep := e.opts.AutoCheckpointKeep
	if keep < 1 {
		return nil
	}
	cur, err := e.store.GetRecord(id)
	if err != nil {
		return err
	}
	if err := e.suspendLocked(id, "checkpoint"); err != nil {
		return err
	}
	cp := store.Checkpoint{ID: fmt.Sprintf("auto-%d", cur.NextAuto+1), Comment: comment, History: cur.Lineage,
		CreateTime: time.Now().UTC(), IsAuto: true}
	if err := e.tag(ctx, id, cp.ID); err != nil {
		return err
	}
	cur, _ = e.store.UpdateRecord(id, func(r *store.Record) {
		r.NextAuto++
		r.Checkpoints = append(r.Checkpoints, cp)
		setExt(r, extOf(*r).tagged(cp.ID))
	})
	e.Emit(cur, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "auto": true})
	// Prune: the oldest autos beyond keep, never spare.
	var autos []string
	for _, c := range cur.Checkpoints {
		if c.IsAuto && c.ID != spare {
			autos = append(autos, c.ID)
		}
	}
	for len(autos) > keep {
		old := autos[0]
		autos = autos[1:]
		if _, err := e.api.DeleteTag(ctx, &ateapipb.DeleteTagRequest{Tag: e.tagRef(id, old)}); err != nil && status.Code(err) != codes.NotFound {
			e.log.Warn("pruning an automatic checkpoint", "id", id, "checkpoint", old, "err", err)
			continue
		}
		e.store.UpdateRecord(id, func(r *store.Record) {
			r.Checkpoints = slices.DeleteFunc(r.Checkpoints, func(c store.Checkpoint) bool { return c.ID == old })
		})
	}
	return nil
}

// waitAllPending waits for every checkpoint of sandbox asked for from inside.
func (e *Engine) waitAllPending(sandbox string) {
	e.mu.Lock()
	var chs []chan struct{}
	for k, ch := range e.pending {
		if len(k) > len(sandbox) && k[:len(sandbox)+1] == sandbox+"/" {
			chs = append(chs, ch)
		}
	}
	e.mu.Unlock()
	for _, ch := range chs {
		select {
		case <-ch:
		case <-time.After(2 * time.Minute):
		}
	}
}

// createActor makes the actor named id from template, starting from tag's
// snapshot rather than the template's golden one when tag is set. Substrate
// wants the template either way.
func (e *Engine) createActor(ctx context.Context, id, template string, tag *ateapipb.ObjectRef) error {
	a := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: e.opts.Atespace, Name: id},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: e.opts.Atespace, Name: template}, SourceTag: tag}
	if _, err := e.api.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: a}); err != nil {
		return fmt.Errorf("substrate: CreateActor: %w", err)
	}
	return nil
}

// fromCheckpoint is the ext a sandbox created from src's checkpoint starts
// with: src's agent token, since the snapshot's agent was claimed with it (or
// is claimed with it on the first resume, from a cold checkpoint), and
// started unless the checkpoint is cold.
func fromCheckpoint(src store.Record, checkpoint string) json.RawMessage {
	x := extOf(src)
	raw, _ := json.Marshal(ext{AgentToken: x.AgentToken, Started: !slices.Contains(x.Cold, checkpoint)})
	return raw
}
