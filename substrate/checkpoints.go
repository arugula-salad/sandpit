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

// CreateCheckpoint suspends the sandbox and tags its snapshot.
func (e *Engine) CreateCheckpoint(rec store.Record, from *engine.GuestChan, comment string, info engine.Progress) (store.Checkpoint, error) {
	if info == nil {
		info = func(string, ...any) {}
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
	if _, err := e.api.CreateTag(ctx, &ateapipb.CreateTagRequest{Tag: &ateapipb.Tag{
		Metadata:    &ateapipb.ResourceMetadata{Atespace: e.opts.Atespace, Name: tagName(rec.ID, cp.ID)},
		Scope:       ateapipb.TagScope_TAG_SCOPE_ATESPACE,
		SourceActor: e.ref(rec.ID),
	}}); err != nil {
		return store.Checkpoint{}, fmt.Errorf("substrate: CreateTag: %w", err)
	}
	info("Snapshot tagged in %s", time.Since(start).Round(time.Millisecond))
	cur, _ = e.store.UpdateRecord(rec.ID, func(r *store.Record) {
		r.NextCheckpoint++
		r.Lineage = append([]string{cp.ID}, r.Lineage...)
		r.Checkpoints = append(r.Checkpoints, cp)
	})
	e.log.Info("checkpoint created", "id", rec.ID, "checkpoint", cp.ID)
	e.Emit(cur, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "auto": false})
	return cp, nil
}

// DeleteCheckpoint deletes its tag and its record.
func (e *Engine) DeleteCheckpoint(rec store.Record, id string) error {
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
	defer e.lock(rec.ID)()
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		return err
	}
	target := engine.FindCheckpoint(cur, id)
	if target == nil {
		return engine.ErrNoCheckpoint
	}
	if beforeStop != nil {
		beforeStop()
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := e.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: e.ref(rec.ID), AnyState: true}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("substrate: DeleteActor: %w", err)
	}
	e.mu.Lock()
	delete(e.running, rec.ID)
	e.mu.Unlock()
	e.mu.Lock()
	tmpl := e.templates[apiOf(cur.API)]
	e.mu.Unlock()
	if err := e.createActor(ctx, rec.ID, tmpl, e.tagRef(rec.ID, id)); err != nil {
		return err
	}
	info("Restored from %s in %s", id, time.Since(start).Round(time.Millisecond))
	lineage := append([]string{id}, target.History...)
	cur, _ = e.store.UpdateRecord(rec.ID, func(r *store.Record) { r.Lineage = lineage })
	e.log.Info("checkpoint restored", "id", rec.ID, "checkpoint", id)
	e.Emit(cur, "checkpoint.restored", map[string]any{"checkpoint": id})
	return nil
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
// with: src's agent token, since the snapshot's agent was claimed with it,
// and started, since the snapshot is not a template's golden one.
func fromCheckpoint(src store.Record) json.RawMessage {
	x := extOf(src)
	raw, _ := json.Marshal(ext{Started: true, AgentToken: x.AgentToken})
	return raw
}
