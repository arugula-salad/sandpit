// Package substrate is a sandpit engine on Agent Substrate
// (github.com/agent-substrate/substrate): a sandbox is an actor, restored
// onto one of a pool of warm gVisor (or micro-VM) workers when a request needs
// it and snapshotted, memory and all, when it is suspended. That is the
// Firecracker engine's model, a sandbox that sleeps warm and wakes on the next
// request, on Kubernetes; the kube engine (agent-sandbox) can only stop a pod
// and keep its volume.
//
// Engine satisfies e2b.Engine. docs/plans/kubernetes-backend.md is the plan
// this is a spike for. The guest is the E2B userland with envd as the
// actor's process (images/substrate); there is no sandpit-agent in it,
// because Substrate's router carries HTTP only (an actor's ports are reached
// with HTTP CONNECT through it, and upgrades are refused), which rules out
// the agent's stream protocol. So a guest here is an engine.PortDialer and
// nothing else: E2B, whose guest daemon speaks HTTP, needs no more.
//
// Substrate never suspends an actor by itself. Suspend (E2B's pause) does,
// and so does the engine's idle rule when Options.IdleSuspend is set; either
// way the next request through the router resumes it with its processes.
package substrate

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/agent"
	"github.com/arugula-salad/sandpit/internal/store"
)

// Options configures the engine.
type Options struct {
	// API is the ate-api-server's gRPC address (host:port).
	API string
	// APIServerName is the name its certificate is for: api.ate-system.svc.
	APIServerName string
	// CAFile holds the CA that signed it (the cluster's service-DNS signer,
	// from its ClusterTrustBundle).
	CAFile string
	// TokenFile is a bearer token ate-api accepts: a Kubernetes service
	// account token with the API's audience. It is re-read for every call.
	TokenFile string
	// Router is atenet-router's CONNECT listener (host:port).
	Router string
	// Atespace and Template are where actors are made and what from: an
	// ActorTemplate running the guest image (images/substrate).
	Atespace, Template string
	// TemplateFile, if set, is an ActorTemplate (YAML) the engine makes
	// itself at startup, with the atespace, as Template-<hash of the file>
	// (EnsureTemplate), and waits for its golden snapshot. It is E2B's.
	TemplateFile string
	// APITemplates are the other front ends' templates, by API ("sprites"
	// for the Sprites API's own sandboxes, "vercel", "daytona", ...): files
	// as TemplateFile, each made as <api>-<hash>. Their guests run
	// sandpit-agent on AgentPort behind an upgrade, unclaimed until the
	// sandbox's first resume (images/substrate-agent).
	APITemplates map[string]string
	// IdleSuspend suspends a sandbox nothing has used for this long; the next
	// request resumes it. 0 never does. A suspended sandbox's background
	// processes stop with it, which is why E2B's sandboxes are never idled
	// on the Firecracker engine; here it is the operator's choice.
	IdleSuspend time.Duration
}

// Engine runs sandboxes as Substrate actors. It satisfies e2b.Engine.
type Engine struct {
	opts  Options
	store *store.Store
	log   *slog.Logger
	api   ateapipb.ControlClient
	conn  *grpc.ClientConn

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	running map[string]*guest // resumed by us, boot hooks run
	onBoot  []engine.BootHook
	quit    chan struct{}

	templates map[string]string // API -> ActorTemplate, once it is ready; guarded by mu
	events    *engine.Bus
	onDelete  []func(store.Sprite)
	describer engine.Describer
	quitting  bool
}

// New connects to ate-api.
func New(opts Options, st *store.Store, log *slog.Logger) (*Engine, error) {
	if opts.API == "" || opts.Router == "" || opts.Atespace == "" || opts.Template == "" {
		return nil, errors.New("substrate: the API and router addresses, an atespace and a template are required")
	}
	if opts.APIServerName == "" {
		opts.APIServerName = "api.ate-system.svc"
	}
	pool := x509.NewCertPool()
	if pem, err := os.ReadFile(opts.CAFile); err != nil || !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("substrate: CA file %q: %v", opts.CAFile, err)
	}
	conn, err := grpc.NewClient(opts.API,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: opts.APIServerName})),
		grpc.WithPerRPCCredentials(tokenFile(opts.TokenFile)))
	if err != nil {
		return nil, err
	}
	e := &Engine{opts: opts, store: st, log: log.With("engine", "substrate"), api: ateapipb.NewControlClient(conn), conn: conn,
		locks: map[string]*sync.Mutex{}, running: map[string]*guest{}, quit: make(chan struct{}),
		events: engine.NewBus()}
	e.templates = map[string]string{"e2b": opts.Template}
	ensure := func(api, base, file string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		name, err := EnsureTemplate(ctx, e.api, opts.Atespace, base, file)
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.templates[api] = name
		e.mu.Unlock()
		e.log.Info("actor template ready", "api", api, "atespace", opts.Atespace, "template", name)
		return nil
	}
	if opts.TemplateFile != "" {
		if err := ensure("e2b", opts.Template, opts.TemplateFile); err != nil {
			conn.Close()
			return nil, fmt.Errorf("substrate: %w", err)
		}
		e.opts.Template = e.templates["e2b"]
	}
	// The other APIs' templates don't hold up the daemon: each is made in the
	// background, retried until it is ready, and until then that API's creates
	// are refused (Create).
	for api, file := range opts.APITemplates {
		go func() {
			for {
				err := ensure(api, api, file)
				if err == nil {
					return
				}
				e.log.Warn("actor template not ready; retrying in a minute", "api", api, "err", err)
				select {
				case <-e.quit:
					return
				case <-time.After(time.Minute):
				}
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := e.api.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: opts.Atespace}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("substrate: ate-api at %s: %w", opts.API, err)
	}
	go e.janitor()
	return e, nil
}

// Shutdown stops the janitor and closes the API connection. Actors carry on.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.quitting = true
	e.mu.Unlock()
	close(e.quit)
	e.conn.Close()
}

// OnBoot registers a hook run after an actor is resumed by this engine and
// before Acquire returns it. Boot.Warm is true unless the actor is starting
// from its template's golden snapshot, which every actor's first resume is.
func (e *Engine) OnBoot(f engine.BootHook) {
	e.mu.Lock()
	e.onBoot = append(e.onBoot, f)
	e.mu.Unlock()
}

func (e *Engine) lock(id string) func() {
	e.mu.Lock()
	l, ok := e.locks[id]
	if !ok {
		l = &sync.Mutex{}
		e.locks[id] = l
	}
	e.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (e *Engine) ref(id string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: e.opts.Atespace, Name: id}
}

// ext is what the engine keeps in a record.
type ext struct {
	// Started: the actor has been resumed (and booted) once, so it has a
	// snapshot of its own, and every later resume is warm.
	Started bool `json:"started,omitempty"`
	// AgentToken is what this sandbox's sandpit-agent is claimed with on its
	// first resume and asked for on every stream after; empty for a guest
	// with no agent (E2B's).
	AgentToken string `json:"agent_token,omitempty"`
}

// AgentPort is where sandpit-agent listens in an agent guest
// (images/substrate-agent). Never reachable through a sandbox's public ports.
const AgentPort = "2024"

// apiOf is the key a record's template is found under.
func apiOf(api string) string {
	if api == "" {
		return "sprites"
	}
	return api
}

const extKey = "substrate"

func extOf(rec store.Record) ext {
	var x ext
	json.Unmarshal(rec.Ext[extKey], &x)
	return x
}

// Create stores the record and creates its actor from the template. The actor
// starts suspended, at the template's golden snapshot.
func (e *Engine) Create(ctx context.Context, spec engine.CreateSpec) (store.Sprite, error) {
	sp := spec.Sprite
	if spec.Checkpoint != nil {
		return sp, errors.New("substrate: creating from a checkpoint is not supported yet")
	}
	api := apiOf(sp.API)
	e.mu.Lock()
	tmpl, ok := e.templates[api]
	e.mu.Unlock()
	if !ok {
		if _, configured := e.opts.APITemplates[api]; configured {
			return sp, fmt.Errorf("substrate: the %s API's actor template is not ready yet", api)
		}
		return sp, fmt.Errorf("%w: no actor template for the %s API (--api-template)", engine.ErrUnsupported, api)
	}
	if sp.Ext == nil {
		sp.Ext = map[string]json.RawMessage{}
	}
	x := ext{}
	if api != "e2b" {
		b := make([]byte, 24)
		rand.Read(b)
		x.AgentToken = hex.EncodeToString(b)
	}
	raw, _ := json.Marshal(x)
	sp.Ext[extKey] = raw
	if err := e.store.Create(&sp); err != nil {
		return sp, err
	}
	_, err := e.api.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: e.opts.Atespace, Name: sp.ID},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: e.opts.Atespace, Name: tmpl},
	}})
	if err != nil {
		e.store.Delete(sp.ID)
		return sp, fmt.Errorf("substrate: CreateActor: %w", err)
	}
	e.log.Info("sandbox created", "id", sp.ID, "api", sp.API)
	return sp, nil
}

// Delete deletes the actor, in whatever state, and its snapshot, and the record.
func (e *Engine) Delete(rec store.Record) error {
	defer e.lock(rec.ID)()
	sp, err := e.store.Get(rec.ID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := e.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: e.ref(rec.ID), AnyState: true}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("substrate: DeleteActor: %w", err)
	}
	e.mu.Lock()
	delete(e.running, rec.ID)
	e.mu.Unlock()
	if err := e.store.Delete(rec.ID); err != nil {
		return err
	}
	e.log.Info("sandbox deleted", "id", rec.ID)
	e.mu.Lock()
	hooks := e.onDelete
	e.mu.Unlock()
	for _, f := range hooks {
		f(sp)
	}
	return nil
}

// Acquire resumes the actor if this engine has not, runs the boot hooks, and
// returns it, counting the use until release for the idle rule. A running
// actor costs no API call: the router resumes one Substrate itself suspended.
func (e *Engine) Acquire(ctx context.Context, rec store.Record) (engine.Guest, func(), error) {
	if g := e.use(rec.ID); g != nil {
		return g, g.release, nil
	}
	unlock := e.lock(rec.ID)
	defer unlock()
	if g := e.use(rec.ID); g != nil {
		return g, g.release, nil
	}
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		return nil, nil, err
	}
	start := time.Now()
	resp, err := e.api.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: e.ref(rec.ID)})
	if err != nil {
		return nil, nil, fmt.Errorf("substrate: ResumeActor: %w", err)
	}
	x := extOf(cur)
	g := &guest{e: e, id: rec.ID, lastUse: time.Now(), agentToken: x.AgentToken}
	warm := x.Started
	if !warm && x.AgentToken != "" {
		if err := g.claim(ctx); err != nil {
			return nil, nil, err
		}
	}
	e.mu.Lock()
	hooks := e.onBoot
	e.mu.Unlock()
	for _, f := range hooks {
		if err := f(ctx, engine.Boot{Record: cur, Warm: warm, Guest: g}); err != nil {
			return nil, nil, err
		}
	}
	if !warm {
		x.Started = true
		raw, _ := json.Marshal(x)
		e.store.UpdateRecord(rec.ID, func(r *store.Record) { r.Ext[extKey] = raw })
	}
	e.mu.Lock()
	e.running[rec.ID] = g
	e.mu.Unlock()
	e.log.Info("sandbox resumed", "id", rec.ID, "warm", warm, "resumed", resp.GetResumed(),
		"worker", resp.GetActor().GetStatus().GetWorkerAssignment().String(), "took", time.Since(start).Round(time.Millisecond))
	g.inflight++
	return g, g.release, nil
}

// use counts a use of a sandbox this engine has running.
func (e *Engine) use(id string) *guest {
	e.mu.Lock()
	defer e.mu.Unlock()
	g := e.running[id]
	if g != nil {
		g.inflight++
		g.lastUse = time.Now()
	}
	return g
}

// Suspend snapshots the actor, memory included, and frees its worker.
func (e *Engine) Suspend(rec store.Record) error {
	defer e.lock(rec.ID)()
	return e.suspendLocked(rec.ID, "pause")
}

func (e *Engine) suspendLocked(id, why string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	_, err := e.api.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: e.ref(id)})
	if err != nil && status.Code(err) != codes.FailedPrecondition { // already suspended
		return fmt.Errorf("substrate: SuspendActor: %w", err)
	}
	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
	e.log.Info("sandbox suspended", "id", id, "why", why, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// Stop is Suspend: a suspended actor keeps its memory, but nothing here can
// drop it (E2B's {"memory": false}) yet.
func (e *Engine) Stop(rec store.Record) error { return e.Suspend(rec) }

// Cool would discard the memory of a suspended actor; Substrate keeps one
// snapshot, memory and disk together, so there is nothing to discard alone.
func (e *Engine) Cool(store.Record) bool { return false }

// Peek is whether this engine has the sandbox running.
func (e *Engine) Peek(id string) engine.VMView {
	e.mu.Lock()
	defer e.mu.Unlock()
	if g := e.running[id]; g != nil {
		return engine.GuestView(g)
	}
	return engine.VMView{}
}

// SetDeadline sets when the sandbox's deadline action is taken (kube.Engine's).
func (e *Engine) SetDeadline(id string, at *time.Time, action store.DeadlineAction) (store.Record, error) {
	return e.store.UpdateRecord(id, func(r *store.Record) {
		r.ExpiresAt = at
		p := store.LifecyclePolicy{}
		if r.Lifecycle != nil {
			p = *r.Lifecycle
		}
		if action == store.DeadlineDelete {
			action = ""
		}
		p.DeadlineAction = action
		r.Lifecycle = &p
		r.UpdatedAt = time.Now().UTC()
	})
}

// janitor takes deadlines (as kube.Engine's does) and the idle rule.
func (e *Engine) janitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.quit:
			return
		case <-t.C:
		}
		now := time.Now()
		for _, rec := range e.store.Records() {
			if _, ok := rec.Ext[extKey]; !ok || rec.ExpiresAt == nil || rec.Protected || now.Before(*rec.ExpiresAt) {
				continue
			}
			if rec.Lifecycle.OnDeadline() == store.DeadlineDelete {
				e.log.Info("deadline passed; deleting", "id", rec.ID)
				if err := e.Delete(rec); err != nil && !errors.Is(err, store.ErrNotFound) {
					e.log.Warn("delete at the deadline", "id", rec.ID, "err", err)
				}
				continue
			}
			if err := e.Suspend(rec); err != nil {
				e.log.Warn("suspend at the deadline", "id", rec.ID, "err", err)
			}
			e.store.UpdateRecord(rec.ID, func(r *store.Record) { r.ExpiresAt = nil })
		}
		if e.opts.IdleSuspend > 0 {
			e.mu.Lock()
			var idle []string
			for id, g := range e.running {
				if g.inflight == 0 && now.Sub(g.lastUse) >= e.opts.IdleSuspend {
					idle = append(idle, id)
				}
			}
			e.mu.Unlock()
			for _, id := range idle {
				go e.idleSuspend(id)
			}
		}
	}
}

func (e *Engine) idleSuspend(id string) {
	defer e.lock(id)()
	e.mu.Lock()
	g := e.running[id]
	busy := g == nil || g.inflight > 0 || time.Since(g.lastUse) < e.opts.IdleSuspend
	e.mu.Unlock()
	if busy {
		return
	}
	if err := e.suspendLocked(id, "idle"); err != nil {
		e.log.Warn("idle suspend", "id", id, "err", err)
	}
}

// guest is an actor as the front end reaches it: its ports, through the router.
type guest struct {
	e          *Engine
	id         string
	agentToken string    // empty: no agent in this guest
	inflight   int       // guarded by e.mu
	lastUse    time.Time // guarded by e.mu
}

func (g *guest) release() {
	g.e.mu.Lock()
	g.inflight--
	g.lastUse = time.Now()
	g.e.mu.Unlock()
}

// Dial is a stream to the guest's sandpit-agent: a tunnel to AgentPort, an
// upgrade the router lets through, and the agent's token handshake.
func (g *guest) Dial(ctx context.Context) (net.Conn, error) {
	if g.agentToken == "" {
		return nil, errors.New("substrate: this sandbox's guest has no agent, only its HTTP ports")
	}
	c, err := g.agentStream(ctx)
	if err != nil {
		return nil, err
	}
	if err := agent.Handshake(c, g.agentToken); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// claim gives an unclaimed agent this sandbox's token. A restored actor's
// agent may take a moment to be reached through the router, so it is tried
// for a while.
func (g *guest) claim(ctx context.Context) error {
	var last error
	for i := 0; i < 20; i++ {
		c, err := g.agentStream(ctx)
		if err == nil {
			err = agent.Claim(c, g.agentToken)
			c.Close()
			if err == nil {
				return nil
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("substrate: claiming the agent: %w", last)
}

// agentStream is a tunnel to the agent, through the upgrade, with a deadline
// for the handshake that follows (the caller clears it).
func (g *guest) agentStream(ctx context.Context) (net.Conn, error) {
	c, err := g.dialPort(ctx, AgentPort)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	uc, err := agent.DialUpgrade(c, g.id)
	if err != nil {
		c.Close()
		return nil, err
	}
	return uc, nil
}

// DialPort opens a stream to a user's port on the actor. With an agent in the
// guest it goes through the agent (engine.AgentDialPort), as on Firecracker:
// any TCP, and "http" for the sprite's own HTTP service, which the agent starts
// on demand. Without one (E2B's guest) it is a tunnel through the router
// (dialPort), which carries HTTP only. Never to the agent's own port.
func (g *guest) DialPort(ctx context.Context, port string) (net.Conn, error) {
	if g.agentToken != "" {
		if port == AgentPort {
			return nil, fmt.Errorf("substrate: port %s is sandpit-agent's", port)
		}
		return engine.AgentDialPort(ctx, g, port)
	}
	return g.dialPort(ctx, port)
}

// dialPort opens a tunnel to port on the actor: an HTTP CONNECT to the
// router, which resumes the actor if it has to. What is sent through it must
// be HTTP (or an upgrade to websocket); the router re-routes every request in it.
func (g *guest) dialPort(ctx context.Context, port string) (net.Conn, error) {
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("substrate: invalid port %q", port)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", g.e.opts.Router)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(30 * time.Second)) // long enough for a resume
	}
	authority := g.id + ":" + port
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nate-target-actor: %s/%s\r\n\r\n", authority, authority, g.e.opts.Atespace, g.id)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("substrate: CONNECT %s: %w", authority, err)
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("substrate: CONNECT %s: %s", authority, resp.Status)
	}
	c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// tokenFile is a bearer token read from a file on every call, so a rotated
// projected token is picked up.
type tokenFile string

func (f tokenFile) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(string(f))
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

func (tokenFile) RequireTransportSecurity() bool { return true }
