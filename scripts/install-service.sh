#!/usr/bin/env bash
# Runs sandpitd as a systemd *user* service, so the API comes back after a
# reboot the way the network and the storage volume already do. No root: the
# daemon runs as you, exactly as `make run` does.
#
#   make install-service                         # build, install, (re)start sandpit.service
#   make install-service NAME=sb DATA=/bulk/sb FLAGS='--listen 127.0.0.1:7910 --net-pool 1'
#   ./scripts/install-service.sh -- --url-domain sprites.example.com --e2b-listen 127.0.0.1:7901
#   ./scripts/install-service.sh --uninstall [--name NAME]
#
# Flags after `--` are sandpitd's and are remembered as SANDPIT_FLAGS in
# ~/.config/sandpit/<name>.env; without them a re-install keeps what is there.
#
#   --name NAME   unit name, default sandpit (a second stack needs its own)
#   --data DIR    data directory, default $SANDPIT_DATA or ~/.local/share/sandpit
#   --check       only run the checks below and exit (make install-service runs
#                 it before building anything into --data)
#   --force-pair  allow a name other than sandpit on sandpit's default data
#                 directory, or the name sandpit on another one (refused by
#                 default: either is usually a typo that would repoint or fight
#                 the default install)
#   --takeover    replace an existing <name>.service that runs one of wisp's
#                 daemons on this same --data, keeping the unit name and data:
#                 - wisp's sandboxd (installed by wisp's make install-sandboxd):
#                   its SANDBOXD_FLAGS carry over;
#                 - wispd itself: --name wisp --data ~/.local/share/wisp, and only
#                   when wisp.service runs wisp's own wispd there. WISPD_FLAGS carry
#                   over, with --listen 127.0.0.1:7788 (wispd's default, not
#                   sandpitd's) added when they have no --listen.
#                 Flags given after -- replace what would carry over. wisp's old
#                 lib dir and env file are left for a rollback.
#
# Never touched without --takeover of wispd: the unit name wisp (wisp.service is
# wispd) and wisp's data directory (~/.local/share/wisp). Once taken over they are
# sandpit's, and a plain re-install upgrades them. An existing <name>.service whose
# binary or data directory differs from what is being installed is never
# repointed (bar --takeover above): uninstall it first.
#
# Two things need root, once, and this script only tells you about them:
#
#   sudo loginctl enable-linger $USER
#       Without lingering your user manager, and sandpitd in it, starts at your
#       first login and stops at your last logout.
#   sudo ./scripts/install-service.sh --system-dropin
#       Installs /etc/systemd/system/user@<uid>.service.d/wisp.conf, which
#       (a) orders your user manager after the sprite network and volume units at
#       boot, and so before them at shutdown, and (b) lifts its stop timeout.
#       Ubuntu ships that at 5 seconds: at reboot everything you run is killed 5 s
#       after being asked to stop, which is not enough to write several guests'
#       RAM to disk. Sprites cut off that way are intact but come back cold.
set -euo pipefail
trap 'echo "install-service.sh: failed at line $LINENO: $BASH_COMMAND" >&2' ERR

DEFAULT_NAME=sandpit
NAME=$DEFAULT_NAME
DATA="${SANDPIT_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/sandpit}"
BIN=sandpitd
FLAGS_VAR=SANDPIT_FLAGS
FORCE_PAIR=0
TAKEOVER=0
ACTION=install
FLAGS=()
HAVE_FLAGS=0
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2 ;;
    --data) DATA="$2"; shift 2 ;;
    --force-pair) FORCE_PAIR=1; shift ;;
    --takeover) TAKEOVER=1; shift ;;
    --check) ACTION=check; shift ;;
    --uninstall) ACTION=uninstall; shift ;;
    --system-dropin) ACTION=dropin; shift ;;
    --remove-system-dropin) ACTION=rmdropin; shift ;;
    --) shift; FLAGS=("$@"); HAVE_FLAGS=1; break ;;
    *) sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
done

# Generous on purpose: a stop is N parallel snapshot writes of guest RAM each.
STOP_TIMEOUT=600

# The drop-in keeps wisp's file name on purpose: it configures the user manager,
# not one daemon, and on a host shared with wisp (or taken over from it) the one
# wisp installed already does exactly this job for every daemon in that manager.
if [ "$ACTION" = dropin ] || [ "$ACTION" = rmdropin ]; then
  [ "$(id -u)" = 0 ] || { echo "run this one with sudo" >&2; exit 1; }
  OWNER="${WISP_OWNER:-${SUDO_USER:-}}"
  [ -n "$OWNER" ] && id "$OWNER" >/dev/null 2>&1 || { echo "cannot determine the owning user; run via sudo from your account" >&2; exit 1; }
  DIR="/etc/systemd/system/user@$(id -u "$OWNER").service.d"
  if [ "$ACTION" = rmdropin ]; then
    rm -f "$DIR/wisp.conf"; rmdir "$DIR" 2>/dev/null || true
    systemctl daemon-reload
    echo "ok: removed $DIR/wisp.conf (it served every sandpitd and wispd in $OWNER's user manager)"
    exit 0
  fi
  mkdir -p "$DIR"
  # Every network pool's boot units, since this manager may run a sandpitd on any
  # of them (--net-pool); pool 0's are named even before setup-host.sh has made
  # them. (wisp-net*/wisp-storage are host-level names sandpit keeps from wisp.)
  AFTER="wisp-net.service wisp-netd.service"
  for u in /etc/systemd/system/wisp-net[0-9]*.service /etc/systemd/system/wisp-netd[0-9]*.service; do
    [ -e "$u" ] && AFTER+=" $(basename "$u")"
  done
  cat > "$DIR/wisp.conf" <<EOF
# Installed by sandpit scripts/install-service.sh --system-dropin.
# $OWNER's user manager runs sandpitd (and perhaps wispd). Start it after the
# sprite network and volume exist; stop it before they go; and let it finish
# suspending sprites (the distribution default gives user services 5 seconds at
# shutdown).
[Unit]
After=$AFTER wisp-storage.service

[Service]
TimeoutStopSec=$((STOP_TIMEOUT + 30))
EOF
  systemctl daemon-reload
  echo "ok: $DIR/wisp.conf installed (ordering + a $((STOP_TIMEOUT + 30)) s stop timeout for $OWNER's user manager); takes effect the next time that manager starts, i.e. at the next boot"
  exit 0
fi

[ "$(id -u)" != 0 ] || { echo "run this as yourself, not root: it installs a user service" >&2; exit 1; }
refuse() { echo "install-service.sh: refusing: $*" >&2; exit 1; }
# Before anything, uninstall included: an empty or path-like name would make
# LIB below a parent of every stack's directory, and uninstall rm -rf's LIB.
case "$NAME" in
  ''|*/*|.*) refuse "bad --name '$NAME'" ;;
esac
CONF_DIR="${XDG_CONFIG_HOME:-$HOME/.config}"
UNIT_DIR="$CONF_DIR/systemd/user"
UNIT="$UNIT_DIR/$NAME.service"
ENV_FILE="$CONF_DIR/sandpit/$NAME.env"
LIB="$HOME/.local/lib/sandpit/$NAME"
# Where wisp's install-service.sh put a daemon of this name (for --takeover).
WISP_LIB="$HOME/.local/lib/wisp/$NAME"
WISP_ENV_FILE="$CONF_DIR/wisp/$NAME.env"

# unit_exec: the existing unit's binary and --data, as OLD_BIN and OLD_DATA.
OLD_BIN="" OLD_DATA=""
if [ -e "$UNIT" ]; then
  EXEC=$(sed -n 's/^ExecStart=//p' "$UNIT" | head -n1)
  read -r OLD_BIN _ <<<"$EXEC" || true
  OLD_DATA=$(grep -oE -- '--data [^ ]+' <<<"$EXEC" | head -n1 | cut -d' ' -f2 || true)
fi
# wisp.service is wispd's, on a host shared with wisp: sandpit leaves it alone
# unless it is taking wispd over (--takeover), or already has (sandpitd runs it).
if [ "$NAME" = wisp ] && [ "$TAKEOVER" = 0 ] && [ "$OLD_BIN" != "$LIB/$BIN" ]; then
  refuse "--name wisp is wisp's own service (wispd); sandpit installs it only with --takeover, replacing wispd in place"
fi

if [ "$ACTION" = uninstall ]; then
  # Only ever a unit this script installed: a same-named one from wisp's script
  # (a sandboxd not yet taken over) is wisp's to remove.
  if [ -e "$UNIT" ] && [ "$OLD_BIN" != "$LIB/$BIN" ]; then
    refuse "$UNIT runs '${OLD_BIN:-?}', not sandpit's $LIB/$BIN; it is not sandpit's to remove"
  fi
  # Stopping suspends every running sprite, so they are warm for whatever runs next.
  systemctl --user disable --now "$NAME.service" 2>/dev/null || true
  rm -f "$UNIT"; rm -rf "$LIB"
  systemctl --user daemon-reload
  echo "ok: $NAME.service removed (kept: $ENV_FILE, and all data${OLD_DATA:+ in $OLD_DATA})"
  exit 0
fi

# Guard rails, all checked before anything is written.
DATA="$(realpath -m "$DATA")"
# A data directory under both spellings the tooling uses (XDG_DATA_HOME or not).
is_dir_of() { # app
  local d
  for d in "${XDG_DATA_HOME:-$HOME/.local/share}/$1" "$HOME/.local/share/$1"; do
    [ "$DATA" = "$(realpath -m "$d")" ] && return 0
  done
  return 1
}
# wisp's data directory belongs to wisp.service alone, as wispd or, taken over,
# as sandpitd; the name check above already keeps wisp.service to a takeover.
if is_dir_of wisp && [ "$NAME" != wisp ]; then
  refuse "$DATA is wisp's data directory (wispd's sprites); only wisp.service runs on it (--name wisp --takeover)"
fi
[ "$NAME" != wisp ] || is_dir_of wisp || refuse "--name wisp runs on wisp's data directory, not $DATA"
if [ "$FORCE_PAIR" = 0 ]; then
  IS_DEFAULT_DATA=0; is_dir_of sandpit && IS_DEFAULT_DATA=1
  [ "$NAME" = "$DEFAULT_NAME" ] || [ "$IS_DEFAULT_DATA" = 0 ] \
    || refuse "--name $NAME on sandpit's default data directory $DATA would run a second daemon on the default install's sprites; pass --data, or --force-pair if you mean it"
  [ "$NAME" != "$DEFAULT_NAME" ] || [ "$IS_DEFAULT_DATA" = 1 ] \
    || refuse "--name $DEFAULT_NAME is the default install, but --data is $DATA; pass another --name, or --force-pair if you mean to move it"
fi

# Never silently repoint an existing service at another binary or data
# directory. The one exception is --takeover of wisp's sandboxd on this data.
IS_TAKEOVER=0
if [ -e "$UNIT" ] && { [ "$OLD_BIN" != "$LIB/$BIN" ] || [ "$OLD_DATA" != "$DATA" ]; }; then
  if [ "$TAKEOVER" = 1 ]; then
    if [ "$NAME" = wisp ]; then
      [ "$OLD_BIN" = "$WISP_LIB/wispd" ] \
        || refuse "--takeover of wisp.service replaces wisp's wispd ($WISP_LIB/wispd), but it runs '${OLD_BIN:-?}'"
    else
      [ "$OLD_BIN" = "$WISP_LIB/sandboxd" ] \
        || refuse "--takeover replaces wisp's sandboxd ($WISP_LIB/sandboxd), but $UNIT runs '${OLD_BIN:-?}'"
    fi
    [ "$OLD_DATA" = "$DATA" ] \
      || refuse "--takeover keeps the data directory, but $UNIT runs on '${OLD_DATA:-?}', not $DATA"
    IS_TAKEOVER=1
  else
    refuse "$UNIT exists and runs '${OLD_BIN:-?}' on '${OLD_DATA:-?}', not $LIB/$BIN on $DATA; uninstall it first (--name $NAME --uninstall)$([ "$OLD_BIN" = "$WISP_LIB/sandboxd" ] && [ "$OLD_DATA" = "$DATA" ] && echo ", or pass --takeover to replace wisp's sandboxd with sandpitd") if you mean to replace it"
  fi
elif [ "$TAKEOVER" = 1 ] && [ ! -e "$UNIT" ]; then
  refuse "--takeover: there is no $UNIT to take over"
fi

# The flags the unit will run with: given after --, else those remembered in the
# env file, else (taking over) sandboxd's from wisp's env file.
WRITE_ENV=0
if [ "$HAVE_FLAGS" = 1 ]; then
  NEW_FLAGS="${FLAGS[*]}"; WRITE_ENV=1
elif [ -e "$ENV_FILE" ]; then
  # An env file without the variable (hand-edited, or not ours) would have the
  # unit expand an unset one and start sandpitd on its defaults.
  grep -q "^$FLAGS_VAR=" "$ENV_FILE" \
    || refuse "$ENV_FILE has no $FLAGS_VAR= line; pass flags after -- to rewrite it"
  NEW_FLAGS="$(sed -n "s/^$FLAGS_VAR=//p" "$ENV_FILE")"
elif [ "$IS_TAKEOVER" = 1 ] && [ -e "$WISP_ENV_FILE" ]; then
  OLD_VAR=SANDBOXD_FLAGS; [ "$NAME" = wisp ] && OLD_VAR=WISPD_FLAGS
  grep -q "^$OLD_VAR=" "$WISP_ENV_FILE" \
    || refuse "$WISP_ENV_FILE has no $OLD_VAR= line to carry over; pass flags after --"
  NEW_FLAGS="$(sed -n "s/^$OLD_VAR=//p" "$WISP_ENV_FILE")"; WRITE_ENV=1
  # wispd's --listen default was 127.0.0.1:7788, sandpitd's is 7900: keep wispd's.
  if [ "$NAME" = wisp ] && ! grep -qE -- '(^|[[:space:]])--?listen[= ]' <<<"$NEW_FLAGS"; then
    NEW_FLAGS="--listen 127.0.0.1:7788${NEW_FLAGS:+ $NEW_FLAGS}"
  fi
else
  NEW_FLAGS=""; WRITE_ENV=1
fi
# sandpitd's default (127.0.0.1:7900) is clear of wispd, but 7788/7789 are
# wispd's: a sandpitd there would fail to start, or win the port from wispd.
# Go's flag package takes -listen as well as --listen, and = or a space.
# Taken over, wisp.service is wispd's successor and keeps them.
if [ "$NAME" != wisp ] && grep -qE -- '(^|[[:space:]])--?[a-z0-9-]*listen[= ]+[^[:space:]]*:(7788|7789)([[:space:]]|$)' <<<"$NEW_FLAGS"; then
  refuse "a listen address on port 7788 or 7789 is wispd's ($NEW_FLAGS); pick another (sandpit's block is 7900-7904)"
fi
WHAT=sandboxd; [ "$NAME" = wisp ] && WHAT=wispd
[ "$ACTION" != check ] || { echo "ok: $NAME.service may be installed ($BIN on $DATA$([ "$IS_TAKEOVER" = 1 ] && echo ", taking over wisp's $WHAT"))"; exit 0; }

REPO="$(cd "$(dirname "$0")/.." && pwd)"
[ -x "$REPO/bin/$BIN" ] || { echo "no $REPO/bin/$BIN: run make build (or make install-service)" >&2; exit 1; }
for need in bin/firecracker kernel/vmlinux images/base.ext4 initrd.cpio; do
  [ -e "$DATA/$need" ] || { echo "missing $DATA/$need: run make deps image initrd first" >&2; exit 1; }
done

# A daemon started by hand on this data directory has to go first; two would
# fight over every sprite. (It suspends its sprites on ^C, and they resume here.)
if ! systemctl --user is-active -q "$NAME.service" \
   && "$REPO/bin/$BIN" status --data "$DATA" --json 2>/dev/null | grep -q '"daemon": {'; then
  echo "a daemon that is not $NAME.service is running on $DATA; stop it (^C suspends its sprites) and run this again" >&2
  exit 1
fi

mkdir -p "$UNIT_DIR" "$LIB" "$(dirname "$ENV_FILE")"
if [ "$WRITE_ENV" = 1 ]; then
  { echo "# $BIN flags for $NAME.service; edit, then: systemctl --user restart $NAME"
    echo "$FLAGS_VAR=$NEW_FLAGS"; } > "$ENV_FILE"
fi
CUR_FLAGS="$(sed -n "s/^$FLAGS_VAR=//p" "$ENV_FILE")"

# The unit must not depend on a git checkout staying where it is. sandpitd is
# also the operator CLI (status, keys, images, restore, backups).
install -m 0755 "$REPO/bin/$BIN" "$LIB/$BIN.new"
mv -f "$LIB/$BIN.new" "$LIB/$BIN"

# A user unit cannot be ordered after system units, so wait for what the boot
# units provide. Starting early would be worse than starting late: without the
# volume mounted sandpitd would see an empty sprite directory, and without the
# bridge it would cold-boot every sprite with no NIC. The unit names are still
# wisp's (wisp-storage, wisp-net*): they are host-level, shared with wisp, and
# a later migration renames them.
WAIT=""
SYSUNITS="${WISP_SYSTEM_UNIT_DIR:-/etc/systemd/system}" # overridable for scripts/test-install-service.sh
if grep -qs -- " $DATA/vm\$" "$SYSUNITS/wisp-storage.service"; then
  WAIT+="mountpoint -q '$DATA/vm' && "
fi
# A data directory on a filesystem of its own (a nofail fstab entry, say) has to
# be mounted first, or the daemon would start on an empty directory on whatever
# is beneath it.
DATA_MNT=$(findmnt -no TARGET -T "$DATA" 2>/dev/null || true)
if [ -n "$DATA_MNT" ] && [ "$DATA_MNT" != / ]; then
  WAIT+="mountpoint -q '$DATA_MNT' && "
fi
# The network pool (--net-pool) names the bridge and its boot unit: pool 0 is
# msbr0 and wisp-net, pool N msbrN and wisp-netN.
# Go's flag package takes -net-pool as well as --net-pool, and the last one wins.
POOL=$(grep -oE -- '(^|[[:space:]])--?net-pool[= ]+[0-9]+' <<<"$CUR_FLAGS" | tail -n1 | grep -oE '[0-9]+$' || true)
POOL=${POOL:-0}
if [ -e "$SYSUNITS/wisp-net$([ "$POOL" = 0 ] || echo "$POOL").service" ] && ! grep -q -- '--net=false' <<<"$CUR_FLAGS"; then
  WAIT+="[ -e /sys/class/net/msbr$POOL ] && "
fi
cat > "$LIB/wait-host.sh" <<EOF
#!/bin/sh
# Generated by install-service.sh: wait (up to 90 s) for the boot units' work.
for i in \$(seq 90); do
  ${WAIT}exit 0
  sleep 1
done
echo "still waiting for the sprite volume, data mount or bridge (wisp-storage / wisp-net units)" >&2
exit 1
EOF
chmod 0755 "$LIB/wait-host.sh"

# On a host shared with wisp, start after wispd, so after a reboot wisp's
# sprites come back before this daemon's cold boots compete with them.
ORDER=""
if [ "$NAME" != wisp ] && [ -e "$UNIT_DIR/wisp.service" ]; then
  ORDER="After=wisp.service
"
fi
cat > "$UNIT" <<EOF
# Generated by sandpit scripts/install-service.sh; re-run it rather than editing.
[Unit]
Description=sandpit sandbox daemon ($NAME, data in $DATA)
${ORDER}
[Service]
Type=exec
EnvironmentFile=$ENV_FILE
ExecStartPre=$LIB/wait-host.sh
ExecStart=$LIB/$BIN --data $DATA \$$FLAGS_VAR
# SIGTERM goes to sandpitd alone, which suspends every running sprite to disk
# before it exits. The default (signal the whole cgroup) would kill the VMs
# under it first and lose their memory.
KillMode=mixed
TimeoutStopSec=$STOP_TIMEOUT
Restart=on-failure
RestartSec=5
# One guest dying of memory pressure is that sprite's problem, not the service's.
OOMPolicy=continue
ManagedOOMPreference=avoid
# A snapshot restore maps guest RAM from a file, and every VM holds a few fds.
LimitNOFILE=65536

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable -q "$NAME.service"
# Taking over, this stops sandboxd (which suspends its sprites) and starts
# sandpitd on the same data, which resumes them.
systemctl --user restart "$NAME.service"
for i in $(seq 100); do
  "$LIB/$BIN" status --data "$DATA" --json 2>/dev/null | grep -q '"daemon": {' && break
  systemctl --user is-failed -q "$NAME.service" && break
  sleep 0.1
done
if ! systemctl --user is-active -q "$NAME.service"; then
  echo "$NAME.service did not come up:" >&2
  journalctl --user -u "$NAME.service" -n 15 --no-pager >&2 || true
  exit 1
fi

echo "ok: $NAME.service is running and enabled ($BIN --data $DATA ${CUR_FLAGS})"
echo "    status:  $LIB/$BIN status --data $DATA"
echo "    logs:    journalctl --user -u $NAME -f        (add -b for this boot, -p warning for trouble only)"
echo "    flags:   $ENV_FILE, then systemctl --user restart $NAME"
echo "    stop:    systemctl --user stop $NAME          (suspends every running sprite first; they resume warm)"
if [ "$IS_TAKEOVER" = 1 ]; then
  echo
  echo "took over from wisp's $WHAT; kept for a rollback: $WISP_LIB and $WISP_ENV_FILE"
fi
if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
  echo
  echo "NOT DONE, needs you: lingering is off for $USER, so this service only runs while you are logged in."
  echo "    sudo loginctl enable-linger $USER"
fi
if [ ! -e "/etc/systemd/system/user@$(id -u).service.d/wisp.conf" ]; then
  echo
  echo "NOT DONE, needs root once: boot/shutdown ordering and a stop timeout long enough to suspend sprites at"
  echo "reboot (this distribution kills user services after $(systemctl show "user@$(id -u).service" -p TimeoutStopUSec --value 2>/dev/null || echo '?')). Until then a reboot is safe but sprites may come back cold."
  echo "    sudo $REPO/scripts/install-service.sh --system-dropin"
fi
