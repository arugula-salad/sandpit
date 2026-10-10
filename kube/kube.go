// Package kube is a sandpit engine on Kubernetes: sandboxes are
// agent-sandbox Sandbox objects (agents.x-k8s.io/v1beta1,
// github.com/kubernetes-sigs/agent-sandbox), each a single pod with a stable
// identity and a persistent volume, isolated by whatever RuntimeClass the
// cluster offers (gVisor, Kata), instead of Firecracker VMs on this host.
//
// It stands behind the same API front ends the Firecracker engine does:
// Engine satisfies e2b.Engine, the slice of an engine the E2B front end uses,
// so the unmodified E2B SDKs work against pods. The guest contract is the
// same too. The pod's entrypoint is sandpit-agent (serve --system-services
// --listen tcp:...), which runs the image's own daemons (E2B's envd) and serves
// the agent API the front ends reach through engine.Guest; only the transport
// differs, TCP behind a per-sandbox token (agent.TokenListener) where a VM has
// vsock.
//
// What does not carry over is memory. A Firecracker sandbox suspends warm, to
// a snapshot it resumes from with its processes; a Sandbox suspends by having
// its pod deleted (spec.operatingMode Suspended), keeping its volume, so every
// start here is cold (Boot.Warm is never true) and only what is on the volume
// survives a pause. docs/plans/kubernetes-backend.md is the plan this is the
// spike for.
//
// sandpit's store still holds the records: the cluster holds the pods. A
// record's Sandbox is named after its ID (sandboxName).
package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/store"
)

// Dial modes: how sandpitd reaches a pod's agent.
const (
	// DialPodIP connects to the pod's IP: sandpitd runs in the cluster (or on
	// a host that routes the pod network), which is how it should be deployed.
	DialPodIP = "pod-ip"
	// DialPortForward goes through the API server's pods/portforward, one
	// stream per dial: for running sandpitd outside the cluster (development,
	// a kind cluster). Runtimes that cannot port-forward (some gVisor and Kata
	// setups) need DialPodIP.
	DialPortForward = "port-forward"
)

// Options configures the engine.
type Options struct {
	// Namespace is where the Sandbox objects, their pods, volumes and token
	// Secrets are made.
	Namespace string
	// Image is the guest image: a userland whose entrypoint is sandpit-agent
	// (images/kube/Containerfile builds one from the E2B userland).
	Image string
	// ImagePullPolicy is the container's; "" is Kubernetes' default for the tag.
	ImagePullPolicy corev1.PullPolicy
	// RuntimeClass is the pod's runtimeClassName ("gvisor", "kata-qemu", ...);
	// "" is the cluster's default runtime, which is a container, not a sandbox.
	RuntimeClass string
	// AgentPort is where sandpit-agent listens in the pod. 0 is 2024.
	AgentPort int
	// Dial is DialPodIP (the default) or DialPortForward.
	Dial string
	// PersistPath is the directory a sandbox's volume is mounted on, seeded
	// from the image's copy on first start: what survives a pause. "" is no
	// volume at all, so nothing does.
	PersistPath string
	// DiskSize is the volume's size ("10Gi"); "" is 10Gi.
	DiskSize string
	// StorageClass is the volume's; "" is the cluster's default.
	StorageClass string
	// StartTimeout bounds a start, from Running to an agent that answers.
	// 0 is 5 minutes (an image pull on a cold node can take most of that).
	StartTimeout time.Duration
}

// Engine runs sandboxes as agent-sandbox Sandboxes. It satisfies e2b.Engine.
type Engine struct {
	opts  Options
	store *store.Store
	log   *slog.Logger
	rest  *rest.Config
	kube  kubernetes.Interface
	dyn   dynamic.ResourceInterface // Sandboxes in opts.Namespace
	pods  listersv1.PodNamespaceLister

	mu      sync.Mutex
	locks   map[string]*sync.Mutex // by record ID: one transition at a time
	running map[string]*guest      // by record ID: started, with boot hooks run
	onBoot  []engine.BootHook

	quit chan struct{}
}

// New connects to the cluster cfg names and starts watching sandpit's pods
// and the deadlines of st's records. Shutdown stops both; the pods carry on.
func New(opts Options, cfg *rest.Config, st *store.Store, log *slog.Logger) (*Engine, error) {
	if opts.Namespace == "" || opts.Image == "" {
		return nil, errors.New("kube: a namespace and an image are required")
	}
	if opts.AgentPort == 0 {
		opts.AgentPort = 2024
	}
	if opts.Dial == "" {
		opts.Dial = DialPodIP
	}
	if opts.Dial != DialPodIP && opts.Dial != DialPortForward {
		return nil, fmt.Errorf("kube: unknown dial mode %q", opts.Dial)
	}
	if opts.DiskSize == "" {
		opts.DiskSize = "10Gi"
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 5 * time.Minute
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	e := &Engine{opts: opts, store: st, log: log.With("engine", "kube"), rest: cfg, kube: kc,
		dyn: dc.Resource(sandboxGVR).Namespace(opts.Namespace), locks: map[string]*sync.Mutex{},
		running: map[string]*guest{}, quit: make(chan struct{})}

	// The pods are watched, not polled: Acquire, which every request into a
	// sandbox goes through, answers from the cache, and a pod that goes away
	// (a suspend, an eviction, a node lost) takes its guest with it here.
	f := informers.NewSharedInformerFactoryWithOptions(kc, 10*time.Minute, informers.WithNamespace(opts.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = managedBy + "=sandpit" }))
	pi := f.Core().V1().Pods()
	pi.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, obj any) { e.podChanged(obj) },
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if p, ok := obj.(*corev1.Pod); ok {
				e.forgetPod(p.Labels[labelID], p.UID)
			}
		},
	})
	e.pods = pi.Lister().Pods(opts.Namespace)
	f.Start(e.quit)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for typ, ok := range f.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return nil, fmt.Errorf("kube: watching %v in %s did not sync (is the API server reachable, and may this identity list pods?)", typ, opts.Namespace)
		}
	}
	if _, err := e.dyn.List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return nil, fmt.Errorf("kube: listing Sandboxes in %s: %w (is agent-sandbox installed?)", opts.Namespace, err)
	}
	go e.janitor()
	return e, nil
}

// Shutdown stops the engine's watches and its deadline janitor.
func (e *Engine) Shutdown() { close(e.quit) }

// OnBoot registers a hook run every time a sandbox's pod starts, after its
// agent answers and before Acquire hands it to anyone (engine.Engine.OnBoot).
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

// kubeExt is what the engine keeps in a record about its Sandbox.
type kubeExt struct {
	// Token is what the pod's agent asks every stream for. The pod reads it
	// from the Secret of its own (secretName).
	Token string `json:"token"`
}

const extKey = "kube"

func extOf(rec store.Record) (kubeExt, error) {
	var x kubeExt
	raw, ok := rec.Ext[extKey]
	if !ok {
		return x, fmt.Errorf("kube: record %s is not this engine's", rec.ID)
	}
	return x, json.Unmarshal(raw, &x)
}

// Create stores the record and makes its Sandbox, which starts a pod at once
// (a Sandbox is made Running). spec.ImageDisk and spec.Checkpoint are the
// Firecracker engine's: every sandbox here is Options.Image, and there are no
// checkpoints to clone.
func (e *Engine) Create(ctx context.Context, spec engine.CreateSpec) (store.Sprite, error) {
	sp := spec.Sprite
	if spec.Checkpoint != nil {
		return sp, errors.New("kube: creating from a checkpoint is not supported")
	}
	tok := make([]byte, 32)
	rand.Read(tok)
	x, _ := json.Marshal(kubeExt{Token: hex.EncodeToString(tok)})
	if sp.Ext == nil {
		sp.Ext = map[string]json.RawMessage{}
	}
	sp.Ext[extKey] = x
	if err := e.store.Create(&sp); err != nil {
		return sp, err
	}
	sb, err := e.dyn.Create(ctx, e.sandboxObject(sp.Record), metav1.CreateOptions{})
	if err != nil {
		e.store.Delete(sp.ID)
		return sp, fmt.Errorf("kube: create Sandbox: %w", err)
	}
	// The token's Secret is the Sandbox's, so it goes when the Sandbox does.
	// Made second, for the owner's UID; the kubelet retries the pod until it exists.
	if _, err := e.kube.CoreV1().Secrets(e.opts.Namespace).Create(ctx, e.tokenSecret(sp.Record, sb.GetUID()), metav1.CreateOptions{}); err != nil {
		e.dyn.Delete(context.Background(), sb.GetName(), metav1.DeleteOptions{})
		e.store.Delete(sp.ID)
		return sp, fmt.Errorf("kube: create token Secret: %w", err)
	}
	e.log.Info("sandbox created", "id", sp.ID, "api", sp.API, "sandbox", sb.GetName())
	return sp, nil
}

// Delete deletes the Sandbox (and with it its pod, volume and Secret, which
// it owns) and the record.
func (e *Engine) Delete(rec store.Record) error {
	defer e.lock(rec.ID)()
	if _, err := e.store.GetRecord(rec.ID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bg := metav1.DeletePropagationBackground
	if err := e.dyn.Delete(ctx, sandboxName(rec.ID), metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: delete Sandbox: %w", err)
	}
	e.mu.Lock()
	delete(e.running, rec.ID)
	e.mu.Unlock()
	if err := e.store.Delete(rec.ID); err != nil {
		return err
	}
	e.log.Info("sandbox deleted", "id", rec.ID)
	return nil
}

// Acquire makes sure the sandbox's pod is running, its agent answers and its
// boot hooks have run, and returns it. A running sandbox is answered from
// the pod cache without a call to the API server. There is no idle rule here
// yet, so release only marks the end of a use.
func (e *Engine) Acquire(ctx context.Context, rec store.Record) (engine.Guest, func(), error) {
	if g := e.runningGuest(rec.ID); g != nil {
		return g, func() {}, nil
	}
	defer e.lock(rec.ID)()
	if g := e.runningGuest(rec.ID); g != nil {
		return g, func() {}, nil
	}
	cur, err := e.store.GetRecord(rec.ID)
	if err != nil {
		return nil, nil, err
	}
	x, err := extOf(cur)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.opts.StartTimeout)
	defer cancel()
	start := time.Now()
	if err := e.setMode(ctx, rec.ID, modeRunning); err != nil {
		return nil, nil, err
	}
	pod, err := e.waitPod(ctx, rec.ID)
	if err != nil {
		return nil, nil, err
	}
	g := e.guestFor(pod, x.Token)
	if err := waitAgent(ctx, g); err != nil {
		return nil, nil, fmt.Errorf("kube: agent in pod %s: %w", pod.Name, err)
	}
	e.mu.Lock()
	hooks := e.onBoot
	e.mu.Unlock()
	for _, f := range hooks {
		if err := f(ctx, engine.Boot{Record: cur, Guest: g}); err != nil {
			return nil, nil, err
		}
	}
	e.mu.Lock()
	e.running[rec.ID] = g
	e.mu.Unlock()
	e.log.Info("sandbox started", "id", rec.ID, "pod", pod.Name, "node", pod.Spec.NodeName, "took", time.Since(start).Round(time.Millisecond))
	return g, func() {}, nil
}

func (e *Engine) runningGuest(id string) *guest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}

// Suspend suspends the Sandbox: its pod is deleted, its volume kept. It
// returns once the pod is gone, so that the next Acquire starts a new one
// rather than finding the old one on its way out.
func (e *Engine) Suspend(rec store.Record) error {
	defer e.lock(rec.ID)()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e.mu.Lock()
	delete(e.running, rec.ID)
	e.mu.Unlock()
	if err := e.setMode(ctx, rec.ID, modeSuspended); err != nil {
		return err
	}
	for {
		if _, err := e.pods.Get(sandboxName(rec.ID)); apierrors.IsNotFound(err) {
			e.log.Info("sandbox suspended", "id", rec.ID)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("kube: the pod of %s did not go away: %w", rec.ID, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Stop is Suspend: here every suspend is cold.
func (e *Engine) Stop(rec store.Record) error { return e.Suspend(rec) }

// Cool discards a warm snapshot. There are none here: a suspended Sandbox
// has no memory to keep.
func (e *Engine) Cool(store.Record) bool { return false }

// Peek is whether the sandbox is running, as far as the engine knows.
func (e *Engine) Peek(id string) engine.VMView {
	if g := e.runningGuest(id); g != nil {
		return engine.GuestView(g)
	}
	return engine.VMView{}
}

// SetDeadline sets when the sandbox's deadline action (delete, by default) is
// taken; at nil clears it. The janitor takes it.
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

// janitor takes the deadlines that have passed: a delete deletes; anything
// else suspends, which spends the deadline.
func (e *Engine) janitor() {
	t := time.NewTicker(2 * time.Second)
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
			e.log.Info("deadline passed; suspending", "id", rec.ID, "action", rec.Lifecycle.OnDeadline())
			if err := e.Suspend(rec); err != nil {
				e.log.Warn("suspend at the deadline", "id", rec.ID, "err", err)
			}
			e.store.UpdateRecord(rec.ID, func(r *store.Record) { r.ExpiresAt = nil })
		}
	}
}

// podChanged forgets a guest whose pod stopped being ready: the next Acquire
// waits for it (or its replacement) and runs the boot hooks again.
func (e *Engine) podChanged(obj any) {
	p, ok := obj.(*corev1.Pod)
	if !ok || podReady(p) {
		return
	}
	e.forgetPod(p.Labels[labelID], p.UID)
}

func (e *Engine) forgetPod(id string, uid types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if g := e.running[id]; g != nil && g.podUID == uid {
		delete(e.running, id)
		e.log.Info("sandbox pod went away", "id", id, "pod", g.pod)
	}
}

// waitPod waits for the sandbox's pod to be ready with an IP.
func (e *Engine) waitPod(ctx context.Context, id string) (*corev1.Pod, error) {
	sel := labels.SelectorFromSet(labels.Set{labelID: id})
	for {
		pods, _ := e.pods.List(sel)
		for _, p := range pods {
			if p.DeletionTimestamp == nil && podReady(p) && p.Status.PodIP != "" {
				return p, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kube: sandbox %s's pod is not ready: %w%s", id, ctx.Err(), e.podProblem(pods))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// podProblem is why a pod is not starting, as far as its status says.
func (e *Engine) podProblem(pods []*corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			if w := cs.State.Waiting; w != nil && w.Reason != "" {
				return fmt.Sprintf(" (%s: %s %s)", cs.Name, w.Reason, w.Message)
			}
		}
	}
	if len(pods) == 0 {
		return " (no pod yet)"
	}
	return ""
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitAgent waits for the agent's /healthz.
func waitAgent(ctx context.Context, g engine.Guest) error {
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, "http://agent/healthz", nil)
		resp, err := engine.AgentTransport(g).RoundTrip(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("healthz: %s", resp.Status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last: %v)", ctx.Err(), err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// setMode sets the Sandbox's spec.operatingMode.
func (e *Engine) setMode(ctx context.Context, id, mode string) error {
	patch := fmt.Sprintf(`{"spec":{"operatingMode":%q}}`, mode)
	_, err := e.dyn.Patch(ctx, sandboxName(id), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: sandbox %s has no Sandbox object (deleted from the cluster?): %w", id, store.ErrNotFound)
	}
	return err
}

// sandboxName is the name of a record's Sandbox, its pod and its Secret's
// prefix. Not lowercased: an ID that is not a DNS label already (E2B's are)
// is refused by the API server rather than risk two sharing a name.
func sandboxName(id string) string { return "sandpit-" + id }
