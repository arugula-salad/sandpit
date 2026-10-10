# Plan: sandpit on Kubernetes (Agent Substrate, agent-sandbox)

Goal: the API front ends sandpit already has (Sprites, E2B, Vercel, Daytona, Modal) running on
Kubernetes as well as on Firecracker, with the same bar: the providers' unmodified SDKs pass
their probe suites, and where a backend can't do something, the docs say so. Written
2026-10-10 on branch `k8s-backend`, after a refactor and two spikes, one per Kubernetes
substrate.

**Recommendation:** build the Kubernetes backend on
[Agent Substrate](https://github.com/agent-substrate/substrate), with
[agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) as the simple fallback.
Substrate is the Firecracker engine's model (sleep warm, wake on the next request) on
Kubernetes, and it passed every E2B probe step, including the one agent-sandbox can't. It is
also pre-1.0 and heavier to run, which is why the fallback stays.

## Where it stands

### The seam (commit `702f2ec`, no behaviour change)

The front ends reached a sandbox as a `*vmm.Machine`, but only ever to open a stream to its
guest agent. That is now `engine.Guest` (`engine/guest.go`): one `Dial(ctx)` method.
`Acquire`, `Boot`, `DialPort`, `AgentDial` and `AgentTransport` take or return it. A guest
that reaches ports some other way can also implement `engine.PortDialer`, which `DialPort`
prefers (added for Substrate).

Each front end now declares the slice of the engine it uses, instead of taking
`*engine.Engine`. Each checks at compile time that `*engine.Engine` still satisfies it.

| Front end | Engine methods it needs |
|---|---|
| E2B | Create, Delete, Acquire, Suspend, Cool, Peek, SetDeadline, OnBoot |
| Modal | Create, Delete, Acquire, Quitting, OnDelete |
| Daytona | Create, Delete, Acquire, Stop, Peek, BeginUse, SetPolicy, Events, OnDelete |
| Vercel | Create, Delete, Acquire, Stop, SetDeadline, Create/Delete/Hold/RestoreCheckpoint, OnDelete |
| Sprites (`internal/server`) | still `*engine.Engine`: about 25 methods, plus backups, images, network policy and checkpoints |

### Two spikes, one front end

`cmd/sandpit-kube` serves the unchanged E2B front end on either engine
(`--engine kube|substrate`). Both runs used `e2e/providers/e2b/run.sh` unmodified, with the
official SDKs (Python `e2b` 2.52.0, JS 2.52.0), on kind.

| | `kube`: agent-sandbox v1.0.4 (commit `77d358c`) | `substrate`: Substrate `f2006302` (commit `14830b1`) |
|---|---|---|
| A sandbox is | a `Sandbox`: one pod plus a PVC | an actor, restored onto one of a pool of warm gVisor workers |
| Probe, both SDKs | 12/13: everything but `pause_resume` | **13/13** |
| Create | ~5–7 s (PVC and pod start) | **~150 ms** (restore from the template's golden snapshot) |
| Pause, then resume | 5 s, then 1.8 s, **cold**: processes, `/tmp` and anything off the volume are lost | **~360 ms** for both: memory, processes and the whole filesystem kept |
| Idle sandboxes | hold a pod (or are stopped cold) | `--idle-suspend`: snapshotted when idle (~100 ms), and the next request wakes them in ~120 ms with processes running |
| Guest | E2B userland plus `sandpit-agent` (TCP, token handshake) | E2B userland with envd as the actor's process. No agent (see below) |
| Isolation in the spike | runc (kind has no gVisor) | **gVisor** (Substrate ships `runsc`) |
| Timeouts (kill, auto-pause), kill cleanup, restart adoption | ✅ | ✅ (restart not re-tested) |

### What the spikes found

**agent-sandbox (`kube`):**
- **Pods must be Guaranteed QoS.** envd starts every command with
  `echo 100 > /proc/$$/oom_score_adj`. In a Burstable pod (`oom_score_adj` 996) that is a
  lowering, which needs `CAP_SYS_RESOURCE`, so every command failed with
  `echo: I/O error`. Requests now equal limits on every container.
- **`sandpit-agent` as a container's PID 1** took its VM-init path. It now spots `serve` as
  PID 1 and only supervises and reaps (`runContainerInit`). `--system-services` starts the
  image's daemons off vsock.
- **The agent on TCP needed authentication.** Each stream opens with
  `AUTH <token>` / `OK` (`agent.TokenListener`), with the token in a Secret the Sandbox owns.
- **Port-forward is the slow, dev-only path.** gVisor and Kata often can't do it at all, so
  production means sandpitd in-cluster, dialing pod IPs.

**Substrate:**
- **Its router carries HTTP only, so there's no agent stream.** Ports are reached with
  `CONNECT <actor>:<port>` (`ate-target-actor` header, port 8081), and the router refuses
  upgrades: WebSocket refusal landed in `f2006302`, and raw TCP is unsupported.
  - `sandpit-agent`'s stream protocol (`Upgrade: tcp`, websocket exec) can't pass.
  - E2B didn't need it: envd is HTTP (Connect RPC), so the guest is an
    `engine.PortDialer` and nothing more.
  - This is the main constraint for the other front ends (phase 2).
- **The actor rootfs loses the image's metadata** (upstream bug, to report): everything
  becomes root-owned, `/` is 0700, and setuid, setgid and sticky bits are dropped.
  - `images/substrate/sandpit-fixperms` replays a list recorded at build time before envd
    starts, so every golden snapshot is already repaired.
- **The default capabilities are minimal** (`AUDIT_WRITE`, `KILL`, `NET_BIND_SERVICE`).
  envd needs `SETUID`/`SETGID` to run commands as the sandbox's user and `CHOWN`/`FOWNER`
  for files. The template grants a container runtime's usual set; gVisor stays the boundary.
- **The wakeup probe accepts only 200.** envd's open `/health` returns 204, and `/envs`
  needs the sandbox's token after `/init`, so the probe failed every resume after the
  first. The template has no probe, which costs a 20 s warm-up when a template is created,
  not on resume. Upstream fix: accept 2xx.
- **Per-sandbox secrets come after start.** Actors start from one golden snapshot per
  template, and env vars are literal values. envd's `/init` (the engine's boot hook, run on
  each actor's first resume) already hands each sandbox its token.
- **Substrate never suspends on its own.** The engine decides, via pause, deadlines or
  `--idle-suspend`, and the router resumes. That is the split sandpit already has.
- **Dependencies:**
  - client-go v0.37 forces `gorilla/websocket` and `protobuf` onto upstream pseudo-versions.
  - Importing Substrate's `ateapipb` module bumped about 25 more (prometheus, x/crypto, ...).
    All tests pass, but see decision 4.

## Capability matrix

| Capability | Firecracker (`engine`) | Substrate (`substrate`) | agent-sandbox (`kube`) |
|---|---|---|---|
| Isolation | microVM | gVisor, or Kata on Cloud Hypervisor (micro-VM workers) | RuntimeClass: gVisor or Kata (runc is not a sandbox) |
| Warm suspend (memory and processes) | ✅ snapshot | ✅ runsc checkpoint (~100 ms) | ❌ |
| Wake on request | ✅ | ✅ router resumes, ~120 ms | cold start, seconds |
| Idle density | suspended VMs cost disk | suspended actors cost object storage only. The project claims up to 30x oversubscription | one pod per sandbox |
| Filesystem across pause | ✅ whole disk | ✅ whole rootfs (in the snapshot) | the volume's paths only |
| Durable volumes | the disk | one `durableDir` on gVisor (several on micro-VM), CSI volumes | PVC |
| Checkpoints and restore | ✅ reflink copies | Tags: a suspend plus `CreateTag`, and actors created from a tag | VolumeSnapshots, disk only |
| Fast create | reflink plus boot ~1 s | ✅ golden snapshot, ~150 ms | warm pools (SandboxWarmPool) |
| Guest protocol | vsock, anything | **HTTP only** (CONNECT, no upgrades) | TCP, anything |
| Egress policy | ✅ netd | EgressPolicy RPCs and an egress gateway (GKE docs say unsupported there; untested) | NetworkPolicy, or a template's `networkPolicy` |
| Public URLs | ✅ the URL proxy | sandpitd's proxy, dialing through the router | sandpitd's proxy, dialing pod IPs |
| Runs on | one Linux host with KVM | Kubernetes 1.36+ (beta certificate APIs), Postgres, an object store. Pre-1.0 | any Kubernetes with the CRDs |
| Control-plane auth | sandpit's | authentication but **no authorization yet**: any accepted token controls every atespace | Kubernetes RBAC |

## Decisions to make

| # | Question | Recommendation |
|---|---|---|
| 1 | New repo or a backend in sandpit? | **A backend in sandpit.** The ~12k lines of API translation are the product. `sandpitd --backend=firecracker|substrate|kube` |
| 2 | Which Kubernetes backend leads? | **Substrate**, because warm pause, wake-on-request and density are what sandpit is. **agent-sandbox** stays as the fallback for clusters that can't run Substrate (older Kubernetes, no Postgres or object store, or a need for non-HTTP guests) |
| 3 | Where records live | **The local store, with one sandpitd replica, until phase 4.** On Substrate, actors already live in its Postgres, so sandpit's record could shrink to front-end metadata |
| 4 | Dependencies | **Don't import Substrate's module.** Vendor `ateapi.proto` (Apache-2.0) and generate stubs in sandpit, which drops the ~25 forced bumps. Accept client-go's pins, which are Kubernetes' own |
| 5 | Non-HTTP guests on Substrate | **Ask upstream first:** raw-TCP CONNECT is on their list of what isn't reachable yet. Meanwhile, give `sandpit-agent` an upgrade-free mode (HTTP/2 or chunked bidirectional streams for exec, no `Upgrade: tcp`) so the Daytona, Vercel, Modal and Sprites front ends can reach it through CONNECT |
| 6 | Substrate's missing authorization | **Run Substrate per tenant or per sandpit** (its control plane trusts every authenticated caller), so sandpit holds the only credential, until authorization lands |

## Phases

**Phase 1: E2B on Substrate, production-shaped.**
- Generate our own ateapi stubs (decision 4).
- Run sandpitd in the cluster, talking to `api.ate-system.svc` and the router directly, with
  a projected service-account token (audience `api.ate-system.svc`) and the CA from the
  ClusterTrustBundle. No port-forwards.
- An installer for sandpit's WorkerPool, atespace and ActorTemplate (today these are
  `images/substrate/*.tmpl`), with the image pinned by digest.
- Report the rootfs metadata bug and the 2xx wakeup probe upstream. Drop `sandpit-fixperms`
  and restore the wakeup probe once they're fixed.
- Handle crashed actors (`RevertActor`) and evictions (the actor's SIGTERM must lead to a
  suspend within 30 min, so envd needs a handler, or sandpit watches workers draining).
- Default `--idle-suspend` on, and document that background work pauses with an idle
  sandbox. The Firecracker engine avoids that by asking the agent about running tasks;
  here, with no agent, an E2B sandbox's only signal is traffic.
- Rerun the probe on GKE with Substrate's GKE installer, and on micro-VM workers.

**Phase 2: the other front ends.**
- Upgrade-free agent mode (decision 5) and `sandpit-agent` in Substrate guests:
  `Guest.Dial` becomes CONNECT to the agent's port.
- Promote both engines to the full front-end interfaces: `Stop`, `BeginUse` and
  `SetPolicy` with idle accounting, `Events` (export `engine.NewBus`), `OnDelete`,
  `Quitting`.
- Port Modal, then Daytona, then Vercel, whose checkpoints become Substrate tags. Each
  front end's probe suite runs on Substrate, and on agent-sandbox where it can.

**Phase 3: fold into sandpitd.**
- `daemon.Env.Engine` becomes an interface, chosen with `--backend`, and
  `cmd/sandpit-kube` goes away.
- The Sprites API comes last: its core lifecycle maps onto actors and tags, and the
  Firecracker-only routes (backups, network policy) answer with clear errors until they map.

**Phase 4: stateless sandpitd** for more than one replica. On Substrate, its Postgres
already holds the actor state. On agent-sandbox, records move into the cluster.

**agent-sandbox track (in parallel, smaller):**
- Keep the `kube` engine passing the E2B probe.
- Add a NetworkPolicy that only lets sandpitd reach the agent port, plus TLS on the agent
  stream.
- Persist more than `$HOME` across a stop (subPaths on `/tmp`, `/root`, `/usr/local`,
  `/opt`).
- Reconcile records against Sandboxes, set `shutdownTime` as a deadline backstop, and add
  warm pools.

## Running the spikes

agent-sandbox:

```sh
kind create cluster --name sandpit
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml
kubectl create namespace sandpit
./scripts/build-kube-image.sh && kind load docker-image sandpit-kube-e2b:dev --name sandpit
go build -o bin/sandpit-kube ./cmd/sandpit-kube
SANDPIT_TOKEN=dev ./bin/sandpit-kube --context kind-sandpit --image sandpit-kube-e2b:dev \
  --image-pull-policy Never --dial port-forward --disk-size 1Gi
```

Substrate (from a checkout of github.com/agent-substrate/substrate, which needs ko, Go and
Docker; its scripts build everything from source and run a registry on `localhost:5001`):

```sh
# in the substrate checkout
KIND_CLUSTER_NAME=substrate hack/create-kind-cluster.sh
KIND_CLUSTER_NAME=substrate hack/install-ate-kind.sh --deploy-ate-system --credential-provider='{"name":"k8s.io"}'
KIND_CLUSTER_NAME=substrate hack/install-ate-kind.sh --deploy-demo-sandbox   # builds ateom-gvisor
go install ./cmd/kubectl-ate

# in sandpit
img=$(./scripts/build-substrate-image.sh)
WORKER_IMAGE=$(kubectl -n ate-demo-sandbox get workerpool sandbox-workerpool -o jsonpath='{.spec.workerImage}') \
  envsubst < images/substrate/workerpool.yaml.tmpl | kubectl apply -f -
kubectl ate create atespace sandpit
IMAGE=$img SNAPSHOTS=gs://ate-snapshots/sandpit/ envsubst < images/substrate/actortemplate.yaml.tmpl \
  | kubectl ate create actor-template -f -          # golden snapshot in ~30 s
kubectl get clustertrustbundles -l podcert.ate.dev/canarying=live \
  -o jsonpath='{range .items[?(@.spec.signerName=="servicedns.podcert.ate.dev/identity")]}{.spec.trustBundle}{end}' > ate-ca.pem
kubectl -n ate-system create token ate-client --audience api.ate-system.svc --duration 24h > ate-token
kubectl -n ate-system port-forward svc/api 18443:443 &
kubectl -n ate-system port-forward svc/atenet-router 18081:8081 &
SANDPIT_TOKEN=dev ./bin/sandpit-kube --engine substrate --ate-api 127.0.0.1:18443 --ate-ca ate-ca.pem \
  --ate-token ate-token --ate-router 127.0.0.1:18081 --idle-suspend 30s
```

Then, for either: `cd e2e/providers/e2b && E2B_DOMAIN=e2b.localhost
E2B_API_URL=http://127.0.0.1:7901 E2B_SANDBOX_URL=http://127.0.0.1:7901
E2B_PROBE_PORT_SCHEME=http E2B_API_KEY=dev ./run.sh`.
