# Operating it

## Run it as a service

`make run` is for trying things out: the API is gone when the terminal is. To have it come
back after a reboot, like the network and the volume already do:

```sh
make install-service                       # builds, installs a systemd *user* unit, (re)starts it
make install-service FLAGS='--e2b-listen 127.0.0.1:7901 --max-running 8'   # with sandpitd flags
make install-service DATA=/srv/sandpit/data FORCE_PAIR=1   # the one install, its data on a volume of its own
./scripts/install-service.sh --uninstall   # suspends the sprites and removes the unit; data stays
```

It runs as you, with no root, exactly like `make run`. The unit is `sandpit.service`; the
binary is copied to `~/.local/lib/sandpit/sandpit/`, so the unit does not depend on this
checkout; the flags live as `SANDPIT_FLAGS` in `~/.config/sandpit/sandpit.env` and survive a
re-install (one without `FLAGS` keeps what is there). Stop the `make run` daemon first (`^C`
suspends its sprites, and the service resumes them).

```sh
journalctl --user -u sandpit -f            # the log: wakes with latency, suspends, egress denied, ...
journalctl --user -u sandpit -b -p warning # this boot, trouble only
systemctl --user restart sandpit           # suspends every running sprite, starts, resumes on demand
```

A stop sends SIGTERM to sandpitd alone (`KillMode=mixed`), which writes every running sprite's
RAM to disk before exiting: about 0.6 s for three 2 GiB guests at once. Measured on a
restart with three sprites running: all three came back with the same kernel `boot_id`.

Two things need root, once, and the installer tells you when they are missing rather than
doing them:

- `sudo loginctl enable-linger $USER`: without lingering, your user manager (and sandpitd in
  it) starts at your first login and stops at your last logout.
- `sudo ./scripts/install-service.sh --system-dropin`: a drop-in for `user@<uid>.service` that
  orders your user manager after the network, helper and volume units (`wisp-net`,
  `wisp-netd`, `wisp-storage`: see [host setup](host-setup.md) for why those names are still
  wisp's), and so stops it before them, and raises its stop timeout. **Ubuntu ships that
  timeout at 5 seconds** (`user@.service.d/timeout.conf`): at reboot every user service is
  killed 5 s after being asked to stop, whatever its own `TimeoutStopSec` says. A sprite cut
  off mid-snapshot is intact (an incomplete snapshot is never published) but comes back cold.

Until the drop-in is there the unit still waits, for up to 90 s, for the volume to be mounted
and the bridge to exist: started early, sandpitd would see an empty sprite directory, or boot
every sprite without a NIC.

### More than one daemon on a host

`NAME` and `DATA` install another daemon as a unit of its own, with its own data directory,
flags and lib directory (`~/.local/lib/sandpit/<name>`, `~/.config/sandpit/<name>.env`),
without touching `sandpit.service`:

```sh
make images DATA=~/.local/share/sandpit-test     # its own firecracker, kernel and disks
make install-service NAME=sandpit-test DATA=~/.local/share/sandpit-test \
  FLAGS='--listen 127.0.0.1:7910 --net-pool 2 --cgroup-memory-max 32G --cgroup-cpu-weight 50'
```

Give each daemon its own `--listen` (and other listeners), and either its own network pool
(`--net-pool`, made once with [`WISP_POOL=N`](host-setup.md#a-second-network-pool)) or
`--net=false`. Copy `bin/firecracker`, `kernel/` and `images/` into `DATA` rather than
symlinking another install's (`make images DATA=...` does that), or an upgrade of one swaps
them under the other. The installer never repoints an existing unit at another binary or data
directory: `--uninstall` it first. `make test-scripts` checks the installer against a
throwaway `HOME`.

### Beside wisp

sandpit was cut from wisp, and the two can share a host: sandpitd and a `wispd` or `sandboxd`
each on their own data directory, listeners and network pool, under their own units
(`sandpit.service` and `wisp.service` do not collide). `sandpitd status` recognises wisp's
daemons and lists them with the other instances. The host-level names sandpitd uses are still
wisp's ([host setup](host-setup.md)), so the pools are shared ground: whichever daemon owns
pool 0, give the other `--net-pool 1` or more. The Makefile refuses to build into wisp's own
data directory (`~/.local/share/wisp`), since an initrd or disk written there would replace
what wispd boots.

A wisp `sandboxd` install can be taken over in place: `make install-service NAME=<its unit>
DATA=<its data directory> TAKEOVER=1` replaces that unit with one running sandpitd on the same
data directory, flags and sprites. The host names, the root token's `wisproot_` prefix and the
API keys' `wisp_` prefix are kept for exactly this, so its tokens and keys keep working; a
later migration renames them.

wispd itself can be taken over the same way, keeping `wisp.service` and its data, sprites, URLs and
tokens. The Makefile refuses wisp's data directory, so this goes through the scripts directly:

```sh
make build netd
SANDPIT_DATA=~/.local/share/wisp ./scripts/build-initrd.sh   # sandpit-agent in place of wisp-agent
./scripts/install-service.sh --name wisp --data ~/.local/share/wisp --takeover
```

`WISPD_FLAGS` carry over, with `--listen 127.0.0.1:7788` made explicit (wispd's default listener;
sandpitd's is 7900), so the tailnet socket and anything that reaches wispd on 7788 keep working.
The takeover refuses unless `wisp.service` runs wisp's own `~/.local/lib/wisp/wisp/wispd` on that
data directory. wispd and `~/.config/wisp/wisp.env` stay for a rollback. Afterwards
`wisp.service` is sandpit's: re-run the same command without `--takeover` to upgrade it.

To publish the Sprites API behind a reverse proxy beside the other APIs, see
[`--sprites-public-url`](public-urls.md#behind-a-tls-terminating-proxy).

## See what is running

```
$ sandpitd status
sandpitd   pid 1140375, up 3h12m, API on 127.0.0.1:7900
data       /home/you/.local/share/sandpit
volume     5.9G used, 33G free of 39G, reflink clones; 2.0G kept in reserve
image      /home/you/.local/share/sandpit/sprites.xfs; 66G free on its filesystem
sprites    1 running (limit 8), 1 warm, 0 cold; 2 in all (no limit)
network    1 of 32 taps in use
policy     helper reachable

NAME   ID            STATE    PID      RSS   DISK  OWN   SNAP  CKPTS             HOLDS  IP          POLICY
build  09dc6ed0dfbd  running  1140792  412M  1.9G  1.3G  0     3 (mounted 0=v2)  1      10.209.0.2  restricted
hello  19610039ea00  warm     -        -     589M  7M    2.0G  0                 -      10.209.0.3  open

ORPHANED VMs: 1 firecracker process(es) of yours that no running daemon started. Not touched; ...
  PID      RSS   PARENT          CWD
  1083557  915M  systemd (6354)  /tmp/sandpit-mem/vm/e7a5da5ef31f
```

It needs no token: the running daemon answers on `<data>/sandpitd.sock` (mode 0600, so the
filesystem permission is the authentication), and with no daemon up the same command answers
from the files. That socket is also the lock on the data directory: a second sandpitd on the
same one refuses to start. `--json` prints everything, under field names that are meant to be
scripted against (`internal/server/status.go`).

- **DISK / OWN**: a 20 GB apparent size says nothing, and on a reflink volume neither does
  `du`, which counts a shared block once per clone. These come from the files' extent maps:
  DISK is what the sprite's disk and checkpoints occupy with every shared block counted once,
  OWN the part nothing else shares, i.e. what deleting the sprite gives back.
- **images** (shown when there are any) counts the disks cached from container images and
  what they occupy; `sandpitd images list` has the detail. See [images](images.md).
- **HOLDS** is the number of live tasks keeping a sprite awake: the answer to "why is this
  VM still running".
- **Orphans** are Firecracker processes of your user whose parent is not a running sandpitd:
  a VM started by hand, or left by a daemon that was killed. They are reported, never killed,
  since another data directory or someone's experiment may own them. (A sandpitd does reap
  stale VMs in *its own* data directory when it starts.) Other daemon instances (sandpitd, or wisp's
  `wispd` and `sandboxd`) and their VM counts are listed separately.

## Limits

`--max-sprites` and `--max-running` (both 0 = none) are enforced at create and at wake. The
errors have upstream's shape, which the SDKs parse into their `APIError`
(`limit`, `current_count`, `retry_after_seconds`): a wake past `--max-running` is a `429
concurrent_sprite_limit_exceeded` with `Retry-After` set to the idle timeout, which is when a
slot can free up; a create past `--max-sprites` is a `403 sprite_limit_exceeded`, since
waiting does not help. `GET /v1/sprites` carries upstream's `org` block
(`running`/`warm`/`cold` over all sprites, `running_limit`; `warm_limit` is always 0, because
what bounds warm sprites here is the disk, below).

## Host memory admission and the boot cap

A count is the wrong unit for memory: ten 256 MiB sprites and ten 4 GiB ones are not the same
load on the host. Two more flags, both 0 = none by default, so nothing changes until you set
them:

- **`--max-running-memory-mib`** is an aggregate budget every boot and every resume is
  measured against. A sprite counts for its *ceiling*: `resources.memory.limit_mb` + 128 MiB
  of VM headroom, or `--mem-mib` when it has no memory policy.
- **`--max-concurrent-boots`** caps the cold boots in flight. That is the expensive moment
  — Firecracker start, guest init, services coming up — and a host that starts thirty at once
  makes all thirty slow. Resumes are not capped: they cost a fraction of a boot, and a warm
  sprite you cannot resume on demand is a sprite nobody can use.

Both refuse with the same retryable `429 concurrent_sprite_limit_exceeded` the SDKs already
parse (`limit` and `current_count` are MiB for the budget, boots for the cap), with
`Retry-After` set to the idle timeout and to 5 seconds respectively. Neither queues: you are
told to come back, not made to wait. A refusal is also a `limit.refused`
[event](events.md), with `limit` naming which one said no (`max_running_memory`,
`max_concurrent_boots`, `max_running`). `sandpitd status --json` reports
`max_running_memory_mib` / `reserved_memory_mib` and `max_concurrent_boots` /
`boots_in_flight`; reserved memory is counted even with no budget set, so you can see what a
budget would have to be to hold what you run today.

**Why the ceiling and not the grant.** With `resources.memory.autoscale` a guest's balloon
holds back everything above its current grant, so what a sprite costs the host right now is
usually far below its ceiling. Reserving the grant would still be wrong: the guest may
deflate at will (`deflate_on_oom` hands pages back faster than the controller ticks), which is
why the host cgroup is sized for the ceiling too — see
[lifecycle](lifecycle.md). A budget that admitted against grants would admit sprites the host
cannot hold the moment they get busy. The cost of reserving the ceiling is honest
under-subscription: a host full of idle autoscaled sprites refuses wakes while it still has
free RAM. Want the density, set the budget above physical RAM and accept the swap.

**What this is not.** It is admission accounting, not a guarantee against host memory
pressure. Host page cache for sprite disks is not counted, nor Firecracker's own overhead
beyond guest RAM, nor the snapshot a suspend writes, nor sandpitd itself, nor anything else on
the machine. Reservations are taken before a VM starts and released when it stops or the start
fails (a shutdown suspends every sprite, which releases them), and concurrent wakes are
accounted against each other under one lock — so two simultaneous wakes cannot both be
admitted into room for one — but nothing already running is ever evicted to fit an arrival.
Set the budget below physical RAM with room for all of the above, and treat it as a brake on
overcommit rather than a promise.

For a promise, add a kernel bound on top: `--cgroup-memory-max 32G` and
`--cgroup-cpu-weight 50` write `memory.max` / `cpu.weight` on the cgroup subtree holding all of
this daemon's VMs, which a systemd `MemoryMax=` on the unit does not reach. Useful when a second
daemon shares the host with one that matters more. Keep the budget below the cap so admission
refuses before the kernel OOM-kills; details in [security](security.md#capping-all-of-a-daemons-vms-together).

## Disk pressure

The volume holds sprite disks, checkpoints, one memory snapshot per warm sprite, and the
disks cached from [container images](images.md) (building one is refused like a create when
it would eat into the reserve; `sandpitd images rm` gives the space back). A snapshot takes
what that guest was using (see [lifecycle](lifecycle.md#what-a-sprite-costs-in-memory-and-disk))
but needs up to one and a half times its RAM free while it is written.
When it is the loop-mounted image from `setup-storage.sh`, filling it, or the host filesystem
under the sparse image, does not produce a clean ENOSPC but I/O errors inside guests. So:

- A create, checkpoint or restore that would leave less than `--disk-reserve-mib` (2048) free
  is refused with `507 insufficient_storage`. On a reflink volume a clone costs nothing up
  front, so this is the reserve being defended; elsewhere the full copy is counted.
- A suspend must not fail, or the VM would run forever. If its snapshot does not fit, the
  longest-suspended warm sprites are turned cold first (they lose only memory state), and if
  even that cannot make room the sprite is synced and stopped cold instead. Concurrent
  suspends (a shutdown) are not promised the same free bytes twice.
- Free space is the smaller of the volume's and, for a sparse image, its host filesystem's;
  `sandpitd status` shows both. The log warns while either is below `--disk-warn-percent` (10).

What the guard cannot do is stop running guests from growing their own disks, which are
sparse too; the warning and the reserve are the margin for that.
`scripts/verify-disk-guard.sh` fills a real 3 GB filesystem under real VMs and checks all of
the above (22 checks), including that nothing is corrupted afterwards; it needs no root where
`udisksctl` can set up a loop device.

## Not built yet

- **Metrics**: no Prometheus endpoint. The same events the log has (wake mode and latency,
  suspends, going cold, policy denials, limit refusals, disk warnings) are available as a
  stream and as webhooks ([events](events.md)), and `sandpitd status --json` has the gauges.
- **Tokens**: the root token in `<data>/token` is rotated by replacing the file and
  restarting; named, revocable keys are in [API keys](api-keys.md), which lists what they lack.
- **Listening beyond localhost**: the API binds `127.0.0.1` in plaintext, and the supported
  way to expose it is a TLS-terminating reverse proxy in front (it must pass WebSocket
  upgrades and the `Host` header, which routes sprite URLs), or one on `--api-listen`
  ([API keys](api-keys.md#serving-the-api-in-public)). There is no built-in TLS for the API. Sprite URLs are separate: `--public-listen` serves them, and only them, over HTTPS; see
  [public sprite URLs](public-urls.md).
