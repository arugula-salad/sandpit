# Plan: sandpit on Kubernetes (agent-sandbox)

Goal: the API front ends sandpit already has (Sprites, E2B, Vercel, Daytona, Modal) running on
Kubernetes as well as on Firecracker. Sandboxes become [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
`Sandbox` objects: one pod each, with a stable identity and a persistent volume, isolated by
the cluster's RuntimeClass (gVisor, Kata). The bar is the same as for Firecracker: the
providers' unmodified SDKs pass their probe suites. Where a backend can't do something, the
docs say so. Written 2026-10-10, after a refactor and a spike on branch `k8s-backend`.

## Where it stands

**The seam (commit `702f2ec`, no behaviour change).** The front ends reached a sandbox as a
`*vmm.Machine`, but only ever to open a stream to its guest agent. That is now `engine.Guest`
(`engine/guest.go`): one `Dial(ctx)` method. `Acquire`, `Boot`, `DialPort`, `AgentDial` and
`AgentTransport` take or return it. Each front end now declares the slice of the engine it
uses (`e2b.Engine`, `vercel.Engine`, `daytona.Engine`, `modal.Engine`) instead of taking
`*engine.Engine`. Each checks at compile time that `*engine.Engine` still satisfies it.

| Front end | Engine methods it needs |
|---|---|
| E2B | Create, Delete, Acquire, Suspend, Cool, Peek, SetDeadline, OnBoot |
| Modal | Create, Delete, Acquire, Quitting, OnDelete |
| Daytona | Create, Delete, Acquire, Stop, Peek, BeginUse, SetPolicy, Events, OnDelete |
| Vercel | Create, Delete, Acquire, Stop, SetDeadline, Create/Delete/Hold/RestoreCheckpoint, OnDelete |
| Sprites (`internal/server`) | still `*engine.Engine`: about 25 methods, plus backups, images, network policy and checkpoints |

**The spike (commit `77d358c`): the E2B API on agent-sandbox.**
- `kube/` (`kube.Engine`) satisfies `e2b.Engine`.
- `cmd/sandpit-kube` serves the existing E2B front end on that engine.
- `images/kube` is the E2B userland with `sandpit-agent` as its entrypoint, built by
  `scripts/build-kube-image.sh`.

How sandpit concepts map onto agent-sandbox:

| sandpit | agent-sandbox / Kubernetes |
|---|---|
| Create | `Sandbox` with `operatingMode: Running`, a PVC template on `--persist` (default `/home/user`, seeded from the image on first start), and a token `Secret` the Sandbox owns |
| Acquire | patch `Running`, wait for the pod (from a watch, not polling), agent `/healthz`, boot hooks. A running sandbox is served from the cache with no API call |
| Suspend / Stop | patch `Suspended`: the pod is deleted and the PVC kept. Returns once the pod is gone |
| Delete | delete the Sandbox. Garbage collection takes the pod, the PVC (and its PV) and the Secret |
| Deadlines | a janitor over the store's `ExpiresAt` and deadline action (delete, or suspend for E2B's `autoPause`) |
| Guest transport | TCP to `sandpit-agent` in the pod, by pod IP (in-cluster) or through the API server's `pods/portforward` (from outside) |
| Guest auth | a per-sandbox token. Each stream opens with `AUTH <token>` / `OK`, the TCP counterpart of vsock's `CONNECT` line (`agent.TokenListener`) |

Result against kind (Kubernetes v1.34, agent-sandbox v1.0.4, runc, so no isolation), using
`e2e/providers/e2b/run.sh` unmodified with the official SDKs (Python `e2b` 2.52.0, JS 2.52.0):

| Step | Py | JS | |
|---|---|---|---|
| create | ✅ | ✅ | ~5–7 s cold, including PVC provisioning and pod start |
| exec_stream, exec_exit_code, background_kill, stdin_pty | ✅ | ✅ | |
| files, upload_download (incl. signed URLs) | ✅ | ✅ | |
| port (a user server on 8080 through the agent's port tunnel) | ✅ | ✅ | |
| set_timeout, metrics, list, kill | ✅ | ✅ | |
| pause_resume | ❌ | ❌ | the probe expects `/tmp` (and hosted's memory) to survive a pause. Only the volume does |

Checked by hand as well:
- pause takes 5 s and resume 1.8 s (cold).
- `/home/user` survives a pause, and its dotfiles are seeded and owned by `user`.
- Both timeout actions fire (kill, and auto-pause then connect).
- Kill leaves nothing behind: Sandbox, pod, PVC, PV and Secret all go.
- Restarting `sandpit-kube` adopts live pods in 26 ms.

### What the spike found

- **Memory doesn't survive a pause.** A suspended Sandbox has no pod. Every start here is
  cold (`Boot.Warm` is never true), and processes and anything off the volume are lost. This
  is the biggest semantic gap. Hosted E2B, Sprites and Daytona all keep running state across a
  pause, while Vercel's stop and Modal's terminate don't.
- **Pods must be Guaranteed QoS.** E2B's envd starts every command with
  `echo 100 > /proc/$$/oom_score_adj`. In a Burstable pod (`oom_score_adj` 996) that is a
  lowering, which needs `CAP_SYS_RESOURCE`, so *every* command failed with
  `echo: I/O error`. Requests now equal limits on every container, init included.
- **`sandpit-agent` as PID 1 in a container** took its VM-init path (mounts, then `serve` on
  vsock). It now spots `serve` as PID 1 and only supervises, reaps and forwards SIGTERM
  (`runContainerInit`). `--system-services` starts the image's daemons off vsock.
- **The agent on TCP needed authentication.** Anything on the pod network could otherwise
  exec in any sandbox. The token handshake is the minimum. NetworkPolicy and TLS are still to
  come (below).
- **Port-forward works but is the slow path.** Each dial is a new SPDY connection through the
  API server, and gVisor and Kata often can't port-forward at all. Production is sandpitd in
  the cluster dialing pod IPs.
- **The local store still holds records.** The cluster holds pods, so sandpitd is still a
  single stateful process.
- **Dependencies:** client-go v0.37 forces `gorilla/websocket` and `protobuf` onto upstream's
  untagged pseudo-versions in the shared `go.mod`. All tests pass with them. See decision 4.

## Capability matrix

What each engine can back, which decides what each front end can promise there.

| Capability | Firecracker (`engine`) | Kubernetes (`kube`) |
|---|---|---|
| Isolation | microVM | RuntimeClass: gVisor or Kata (runc is not a sandbox) |
| Persistent disk | whole ext4 root | a volume on chosen paths (see decision 5) |
| Warm suspend (memory and processes) | ✅ snapshot | ❌ (only with CRIU or vendor pod snapshots; GKE has them) |
| Idle suspend, wake on request | ✅ | possible: idle accounting in Acquire/release, then Suspend. Wake is a cold start |
| Checkpoints and restore | ✅ reflink copies | VolumeSnapshots (CSI): disk only, slower, driver-dependent |
| Instant clone | ✅ | a PVC from a VolumeSnapshot (`dataSource`) |
| Create from an OCI image | ✅ converted to ext4 | native: it *is* the pod image. The agent must be in the image, or injected by an init container |
| Network policy (egress) | ✅ netd and the guest | NetworkPolicy (L3/L4) or SandboxTemplate `networkPolicy`. FQDN rules need Cilium or a proxy |
| Public URLs | ✅ the URL proxy | the same proxy in sandpitd, which dials the pod. Or Gateway API |
| Backups to S3 | ✅ incremental | the volume's (Velero or snapshots). Not sandpit's to do |
| Fast create | reflink plus boot ~1 s | SandboxWarmPool plus SandboxClaim |
| CPU and memory limits | VM size, with a balloon | requests = limits (they must be: see above) |

## Decisions to make

| # | Question | Recommendation |
|---|---|---|
| 1 | New repo or a backend in sandpit? | **A backend in sandpit.** The ~12k lines of API translation are the product. A second repo would mean importing them, which `internal/` forbids, or keeping them in sync by hand. Ship `sandpitd --backend=kube`. A separate `sandpit-kube` image or chart can still be its own artifact |
| 2 | Where records live | **Phase 1–3: the local store, on a PVC, with sandpitd as one replica.** Phase 4: the cluster (a `SandpitRecord` CRD, or annotations on the Sandbox) so sandpitd is stateless and can run more than one replica |
| 3 | Isolation target | **gVisor first** (GKE Sandbox, or `runsc` on k3s), Kata second. Refuse to start without `--runtime-class` unless `--insecure-runc` is set |
| 4 | client-go's dependency bumps | **Accept them.** Avoiding them would mean a hand-written REST client (create, patch, delete, watch, and port-forward), which isn't worth the risk |
| 5 | What survives a pause | **More than `$HOME`:** the volume also mounted with subPaths on `/tmp`, `/root`, `/usr/local`, `/opt`, `/var/lib` (configurable), so a pause loses only processes and memory. Documented as the kube backend's difference. Revisit with CRIU or pod snapshots in phase 5 |

## Phases

**Phase 1: E2B on kube, production-shaped.**
- Run sandpitd in the cluster: Deployment (one replica), RBAC (Sandboxes, Secrets, pods/watch,
  plus pods/portforward only for dev), a PVC for the store, and a kustomize or Helm chart.
  `--dial pod-ip` by default.
- Lock down the agent: a NetworkPolicy so that only sandpitd's pods reach the agent port, and
  TLS on the agent stream (a per-sandbox certificate, or mTLS from a namespace CA) in place
  of, or on top of, the token.
- Validate gVisor and Kata on a real cluster and rerun the probe there: envd, PTY and
  `oom_score_adj` under `runsc`.
- Reconcile: watch Sandboxes as well as pods. A record whose Sandbox is gone is either
  deleted or marked broken. A Sandbox with no record is reaped after a grace period.
- Make deadlines survive sandpitd being down: set `spec.shutdownTime` and
  `shutdownPolicy: Delete` for delete deadlines, and reconcile the record when the Sandbox
  disappears. Run the janitor's suspends concurrently (today they're serial, 5 s each).
- Implement decision 5 (subPath persistence).
- Add `docs/kube.md` and a `providers/e2b-differences.md` section for the kube backend.

**Phase 2: the other front ends.**
- Promote `kube.Engine` to everything the four interfaces need: `Stop`, `BeginUse` and
  `SetPolicy` with real idle accounting, `Events` (export `engine.NewBus` so another engine
  can own one), `OnDelete`, `Quitting`.
- Port in the order Modal (smallest), Daytona, then Vercel. Vercel needs checkpoints, which
  means VolumeSnapshots behind `Create/Delete/Hold/RestoreCheckpoint`. Each gets its probe
  suite run on kube.
- Build one guest image per API (`images/kube` takes `USERLAND=`), pushed to a registry.

**Phase 3: fold into sandpitd.**
- `daemon.Env.Engine` becomes an interface, and `--backend=firecracker|kube` picks one.
  `cmd/sandpit-kube` goes away.
- The Sprites API (`internal/server`) is the last and largest piece. Either give it a kube
  engine for the core lifecycle and refuse the Firecracker-only routes with a clear error,
  or keep it Firecracker-only for now. Decide once phases 1–2 show how much of it maps.
- The dashboard shows the backend and pod status.

**Phase 4: stateless sandpitd.** Records move into the cluster (decision 2). Then run more
than one replica, with per-sandbox locks via Leases or optimistic concurrency on
resourceVersion.

**Phase 5: speed and fidelity.**
- Warm pools: SandboxTemplate per API image plus SandboxWarmPool, with create as a
  SandboxClaim. Targets sub-second creates.
- Warm pause where the platform has it (GKE pod snapshots, or CRIU via the kubelet
  checkpoint API), which sets `Boot.Warm` true and closes the pause_resume gap.

## Running the spike

```sh
kind create cluster --name sandpit
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml
kubectl create namespace sandpit
./scripts/build-kube-image.sh && kind load docker-image sandpit-kube-e2b:dev --name sandpit
go build -o bin/sandpit-kube ./cmd/sandpit-kube
SANDPIT_TOKEN=dev ./bin/sandpit-kube --context kind-sandpit --image sandpit-kube-e2b:dev \
  --image-pull-policy Never --dial port-forward --disk-size 1Gi

# in another shell: the E2B SDK probe, unmodified
cd e2e/providers/e2b && E2B_DOMAIN=e2b.localhost E2B_API_URL=http://127.0.0.1:7901 \
  E2B_SANDBOX_URL=http://127.0.0.1:7901 E2B_PROBE_PORT_SCHEME=http E2B_API_KEY=dev ./run.sh
```
