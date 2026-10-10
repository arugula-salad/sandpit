package engine

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/arugula-salad/sandpit/internal/netpolicy"
	"github.com/arugula-salad/sandpit/internal/store"
)

// ErrUnsupported is what a Backend answers for something it can't do, such as
// a Firecracker-only feature on Agent Substrate. The APIs turn it into 501.
var ErrUnsupported = errors.New("not supported on this backend")

// Backend is everything sandpitd (the Sprites API, the daemon and every
// front end) asks of the thing that runs sandboxes. *Engine, the Firecracker
// engine, is one; substrate.Engine (Agent Substrate) is the other. Each front
// end still declares the slice of it that it uses.
type Backend interface {
	Create(ctx context.Context, spec CreateSpec) (store.Sprite, error)
	Delete(sp store.Record) error
	Acquire(ctx context.Context, sp store.Record) (g Guest, release func(), err error)
	Stop(sp store.Record) error
	Suspend(sp store.Record) error
	Cool(sp store.Record) bool
	Status(sp store.Record) string
	Peek(id string) VMView
	BeginUse(id string) (end func())
	Quitting() bool
	Shutdown()
	StartReaping()

	SetDeadline(id string, at *time.Time, action store.DeadlineAction) (store.Record, error)
	ChangeDeadline(id string, change func(*Deadline)) (store.Record, error)
	SetPolicy(id string, p store.LifecyclePolicy) (store.Record, error)
	ApplyPolicy(ctx context.Context, sp store.Record) error
	SetNetworkPolicy(id string, rules []store.NetworkRule, p *netpolicy.Policy) error
	RepublishNetworkPolicy(id string)

	CreateCheckpoint(sp store.Record, from *GuestChan, comment string, info Progress) (store.Checkpoint, error)
	DeleteCheckpoint(sp store.Record, id string) error
	HoldCheckpoint(sp store.Record, id string) (cur store.Record, checkpoint string, release func(), err error)
	RestoreCheckpoint(sp store.Record, from *GuestChan, id string, info Progress, beforeStop func()) error
	MountCheckpoint(ctx context.Context, sp store.Record, from *GuestChan, id string) (int, error)
	UnmountCheckpoint(ctx context.Context, sp store.Record, from *GuestChan, id string) error

	// BackupState is nil where there are no backups; Images, where there is
	// no image cache (the Sprites API's image routes answer 501).
	BackupState(id string) *BackupState
	SetBackupFilter(f func(store.Sprite) bool)
	Images() *ImageCache

	Events() *Bus
	Emit(rec store.Record, typ string, detail map[string]any)
	OnBoot(f BootHook)
	OnDelete(f func(store.Sprite))
	SetDescriber(f Describer)
	SetGuestAPI(f func(store.Record, *GuestChan) http.Handler)
}

var _ Backend = (*Engine)(nil)

// NewBus makes an event bus, for a Backend other than *Engine.
func NewBus() *Bus { return newBus() }
