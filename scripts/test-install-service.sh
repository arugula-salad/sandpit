#!/usr/bin/env bash
# Exercises scripts/install-service.sh without installing anything: a throwaway
# HOME, a copy of the script in a fake checkout with a stub sandpitd binary, and
# stub systemctl/loginctl/journalctl/findmnt first on PATH. Nothing outside the
# temporary directory is written, and no real service is touched.
#
#   ./scripts/test-install-service.sh               # or: make test-scripts
#   BASE_REF=v1.2 ./scripts/test-install-service.sh # compare the default install to another ref
#
# The default install (name sandpit, the default data directory) must render
# byte-for-byte as BASE_REF's script renders it (default origin/main; skipped if
# that ref has no install-service.sh), so a running install can always be
# reinstalled identically.
set -euo pipefail
cd "$(dirname "$0")/.."
SRC=$PWD
BASE_REF=${BASE_REF:-origin/main}
T=$(mktemp -d "${TMPDIR:-/tmp}/sandpit-install-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
H=$T/home
DEF=$H/.local/share/sandpit        # sandpit's default data directory
WISPDATA=$H/.local/share/wisp      # wisp's, never to be touched
fails=0
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; fails=$((fails + 1)); }

# Stubs. systemctl answers "yes, active" to everything, which also skips the
# "a daemon is already running here" probe.
mkdir -p "$T/stub" "$T/sys"
cat > "$T/stub/systemctl" <<EOF
#!/bin/sh
case "\$HOME" in "$T"/*) ;; *) echo "stub systemctl: HOME is not the test's" >&2; exit 99 ;; esac
echo "systemctl \$*" >> "$T/calls"
case " \$* " in *" show "*) echo 5s ;; esac
exit 0
EOF
printf '#!/bin/sh\necho yes\n' > "$T/stub/loginctl"
printf '#!/bin/sh\nexit 0\n' > "$T/stub/journalctl"
# Every data dir is on / unless a test says otherwise, whatever the host's /tmp is.
printf '#!/bin/sh\necho "${FAKE_DATA_MNT:-/}"\n' > "$T/stub/findmnt"
chmod +x "$T/stub/"*

# A fake checkout: the script under test plus a stub daemon.
mkrepo() { # dir script-source
  mkdir -p "$1/scripts" "$1/bin"
  cp "$2" "$1/scripts/install-service.sh"
  printf '#!/bin/sh\n# stub sandpitd\n[ "$1" = status ] && echo "{\\"daemon\\": {"\nexit 0\n' > "$1/bin/sandpitd"
  chmod +x "$1/bin/sandpitd"
}
mkrepo "$T/repo" "$SRC/scripts/install-service.sh"

mkdata() {
  mkdir -p "$1/bin" "$1/kernel" "$1/images"
  touch "$1/bin/firecracker" "$1/kernel/vmlinux" "$1/images/base.ext4" "$1/initrd.cpio"
}

# run REPO [VAR=value...] -- script args...: the script with HOME and PATH swapped out.
run() {
  local repo=$1; shift
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u SANDPIT_DATA -u WISP_SYSTEM_UNIT_DIR \
    HOME="$H" USER=sandpit-test PATH="$T/stub:$PATH" "${envs[@]}" \
    bash "$repo/scripts/install-service.sh" "$@"
}

# A home with sandpit's default data dir and, beside it, a wisp install as wisp's
# own script leaves it: wisp.service (wispd) on wisp's data dir.
fresh() {
  rm -rf "$H" "$T/calls"; mkdir -p "$H"; mkdata "$DEF"; mkdata "$WISPDATA"
  mkdir -p "$H/.config/systemd/user" "$H/.config/wisp" "$H/.local/lib/wisp/wisp"
  printf '[Unit]\nDescription=wisp API daemon\n[Service]\nEnvironmentFile=%s\nExecStart=%s --data %s $WISPD_FLAGS\n' \
    "$H/.config/wisp/wisp.env" "$H/.local/lib/wisp/wisp/wispd" "$WISPDATA" > "$H/.config/systemd/user/wisp.service"
  printf 'WISPD_FLAGS=\n' > "$H/.config/wisp/wisp.env"
  printf '#!/bin/sh\n' > "$H/.local/lib/wisp/wisp/wispd"
}
snapshot() { (cd "$H" && find . -print0 | sort -z | xargs -0 -r stat -c '%n %s %Y %a' && find . -type f -print0 | sort -z | xargs -0 -r sha256sum); }
# wisp's own files: its unit, env files, lib dirs and data dir.
wisp_files() { (cd "$H" && find .config/systemd/user/wisp.service .config/wisp .local/lib/wisp .local/share/wisp -print0 | sort -z | xargs -0 stat -c '%n %s %Y %a %i' && find .config/systemd/user/wisp.service .config/wisp .local/lib/wisp .local/share/wisp -type f -print0 | sort -z | xargs -0 sha256sum); }

# ---- the default install renders exactly as the base ref's script did ----
if old=$(git show "$BASE_REF:scripts/install-service.sh" 2>/dev/null); then
  printf '%s\n' "$old" > "$T/old.sh"
  mkrepo "$T/oldrepo" "$T/old.sh"
  check_same() { # label, args...
    local label=$1; shift
    fresh; run "$T/oldrepo" -- "$@" > /dev/null; rm -rf "$T/render-old"; cp -a "$H" "$T/render-old"
    fresh; run "$T/repo" -- "$@" > /dev/null
    if diff -r "$T/render-old" "$H" > "$T/diff"; then pass "$label renders as $BASE_REF"; else fail "$label differs from $BASE_REF:"; cat "$T/diff"; fi
    # Reinstalling over what the old script wrote must work and change nothing.
    run "$T/repo" -- "$@" > /dev/null || fail "$label: reinstall over $BASE_REF's install refused"
    diff -r "$T/render-old" "$H" > /dev/null && pass "$label reinstalls identically" || fail "$label: reinstall changed files"
  }
  check_same "default install"
  check_same "default install with flags" -- --net-pool 1 --max-running 8 --confine=strict
  check_same "explicit --name sandpit --data <default>" --name sandpit --data "$DEF"
else
  echo "skip $BASE_REF has no scripts/install-service.sh"
fi

# ---- the default install ----
fresh
wisp_before=$(wisp_files)
if run "$T/repo" WISP_SYSTEM_UNIT_DIR="$T/sys" -- > "$T/out"; then
  LIB=$H/.local/lib/sandpit/sandpit
  want_unit="# Generated by sandpit scripts/install-service.sh; re-run it rather than editing.
[Unit]
Description=sandpit sandbox daemon (sandpit, data in $DEF)
After=wisp.service

[Service]
Type=exec
EnvironmentFile=$H/.config/sandpit/sandpit.env
ExecStartPre=$LIB/wait-host.sh
ExecStart=$LIB/sandpitd --data $DEF \$SANDPIT_FLAGS
# SIGTERM goes to sandpitd alone, which suspends every running sprite to disk"
  got_unit=$(head -n 11 "$H/.config/systemd/user/sandpit.service")
  [ "$got_unit" = "$want_unit" ] && pass "default unit" || { fail "default unit:"; diff <(echo "$want_unit") <(echo "$got_unit"); }
  grep -qx 'KillMode=mixed' "$H/.config/systemd/user/sandpit.service" && grep -qx 'TimeoutStopSec=600' "$H/.config/systemd/user/sandpit.service" \
    && pass "KillMode=mixed and a long stop timeout" || fail "KillMode/TimeoutStopSec missing"
  want_env="# sandpitd flags for sandpit.service; edit, then: systemctl --user restart sandpit
SANDPIT_FLAGS="
  [ "$(cat "$H/.config/sandpit/sandpit.env")" = "$want_env" ] && pass "default env file" || fail "default env file: $(cat "$H/.config/sandpit/sandpit.env")"
  cmp -s "$LIB/sandpitd" "$T/repo/bin/sandpitd" && pass "sandpitd installed into ~/.local/lib/sandpit/sandpit" || fail "binary in $LIB"
  grep -q '^  exit 0$' "$LIB/wait-host.sh" && pass "nothing to wait for without boot units" || { fail "wait-host.sh:"; cat "$LIB/wait-host.sh"; }
  grep -q 'systemctl --user restart sandpit.service' "$T/calls" && ! grep -q ' wisp.service' "$T/calls" && pass "only sandpit.service was (re)started" || { fail "systemctl calls:"; cat "$T/calls"; }
  [ "$(wisp_files)" = "$wisp_before" ] && pass "wisp's files are untouched" || fail "the install changed wisp's files"
  grep -q 'lingering is off' "$T/out" && fail "lingering reported off" || pass "no lingering warning when Linger=yes"
else
  fail "default install refused:"; cat "$T/out"
fi
rm -f "$H/.config/systemd/user/wisp.service"; rm -f "$H/.config/systemd/user/sandpit.service"
run "$T/repo" -- > /dev/null
grep -q '^After=' "$H/.config/systemd/user/sandpit.service" && fail "After= without a wisp.service" || pass "no After= when wisp.service is absent"
run "$T/repo" -- > /dev/null && pass "reinstall without flags keeps the env file" || fail "reinstall refused"

# ---- a second stack beside it ----
fresh
run "$T/repo" WISP_SYSTEM_UNIT_DIR="$T/sys" -- > /dev/null
def_files() { (cd "$H" && find .config/systemd/user/sandpit.service .config/sandpit/sandpit.env .local/lib/sandpit/sandpit .local/share/sandpit -print0 | sort -z | xargs -0 stat -c '%n %s %Y %a %i'); }
def_before=$(def_files)
SB=$H/sb
mkdata "$SB"
printf '[Service]\nExecStart=/bin/true\n' > "$T/sys/wisp-net2.service"
rm -f "$T/calls"
if run "$T/repo" WISP_SYSTEM_UNIT_DIR="$T/sys" -- --name sb --data "$SB" -- --listen 127.0.0.1:7910 --net-pool 2 > "$T/out"; then
  LIB=$H/.local/lib/sandpit/sb
  grep -qx "ExecStart=$LIB/sandpitd --data $SB \$SANDPIT_FLAGS" "$H/.config/systemd/user/sb.service" && pass "second stack's unit" || { fail "sb unit:"; cat "$H/.config/systemd/user/sb.service"; }
  want_env="# sandpitd flags for sb.service; edit, then: systemctl --user restart sb
SANDPIT_FLAGS=--listen 127.0.0.1:7910 --net-pool 2"
  [ "$(cat "$H/.config/sandpit/sb.env")" = "$want_env" ] && pass "second stack's env file" || fail "sb env file: $(cat "$H/.config/sandpit/sb.env")"
  grep -q '^  \[ -e /sys/class/net/msbr2 \] && exit 0$' "$LIB/wait-host.sh" && pass "it waits for its own bridge (msbr2)" || { fail "wait-host.sh:"; cat "$LIB/wait-host.sh"; }
  [ "$(def_files)" = "$def_before" ] && pass "the default install's files are untouched" || fail "the second install changed the default install's files"
  grep -q 'systemctl --user restart sb.service' "$T/calls" && ! grep -q ' sandpit.service' "$T/calls" && pass "only sb.service was (re)started" || { fail "systemctl calls:"; cat "$T/calls"; }
else
  fail "second stack refused:"; cat "$T/out"
fi
# The volume unit and a data dir on a filesystem of its own (geek's /bulk is nofail).
printf '[Service]\nExecStart=/usr/bin/mount -o loop x %s/vm\n' "$SB" > "$T/sys/wisp-storage.service"
run "$T/repo" WISP_SYSTEM_UNIT_DIR="$T/sys" FAKE_DATA_MNT=/bulk -- --name sb --data "$SB" > /dev/null
grep -q "^  mountpoint -q '$SB/vm' && mountpoint -q '/bulk' && \[ -e /sys/class/net/msbr2 \] && exit 0\$" "$H/.local/lib/sandpit/sb/wait-host.sh" \
  && pass "waits for wisp-storage's volume, the data dir's own mount and the bridge" || { fail "wait-host.sh with storage and /bulk:"; cat "$H/.local/lib/sandpit/sb/wait-host.sh"; }
grep -q '^SANDPIT_FLAGS=--listen 127.0.0.1:7910 --net-pool 2$' "$H/.config/sandpit/sb.env" && pass "a reinstall without flags keeps them" || fail "remembered flags lost"
rm -f "$T/sys/wisp-storage.service" "$T/sys/wisp-net2.service"

# ---- every refusal refuses and writes nothing ----
refuses() { # label, expected message, [VAR=value...] -- args...
  local label=$1 msg=$2; shift 2
  local before; before=$(snapshot)
  if run "$T/repo" "$@" > "$T/out" 2>&1; then
    fail "$label: was not refused"; return
  fi
  grep -q -- "$msg" "$T/out" || { fail "$label: unexpected message:"; cat "$T/out"; return; }
  [ "$(snapshot)" = "$before" ] && pass "$label: refused, nothing written" || fail "$label: refused but wrote files"
}
fresh
OTHER=$H/other; mkdata "$OTHER"
# wisp's own: its unit name and its data directory, whatever else is said.
refuses "the name wisp" "wisp's own service" -- --name wisp --data "$OTHER"
refuses "the name wisp with --force-pair" "wisp's own service" -- --name wisp --force-pair
refuses "--name wisp --takeover onto another data dir" "runs on wisp's data directory" -- --name wisp --data "$OTHER" --takeover
refuses "uninstalling wisp.service" "wisp's own service" -- --uninstall --name wisp
refuses "wisp's data dir" "wisp's data directory" -- --name sb --data "$WISPDATA"
refuses "wisp's data dir, other spelling" "wisp's data directory" -- --name sb --data "$WISPDATA/../wisp/"
refuses "wisp's data dir via SANDPIT_DATA" "wisp's data directory" SANDPIT_DATA="$WISPDATA" --
refuses "wisp's data dir with --force-pair" "wisp's data directory" -- --name sb --data "$WISPDATA" --force-pair
refuses "--check on wisp's data dir" "wisp's data directory" -- --check --name sb --data "$WISPDATA"
# wispd's ports.
refuses "--listen 127.0.0.1:7788" "7788 or 7789 is wispd's" -- --name sb --data "$OTHER" -- --listen 127.0.0.1:7788
refuses "-listen=127.0.0.1:7789" "7788 or 7789 is wispd's" -- --name sb --data "$OTHER" -- --net=false -listen=127.0.0.1:7789
refuses "--listen :7788" "7788 or 7789 is wispd's" -- --name sb --data "$OTHER" -- --listen :7788
refuses "another listener on 7789" "7788 or 7789 is wispd's" -- --name sb --data "$OTHER" -- --e2b-listen 127.0.0.1:7789
# Pairings, as wisp's script had them for its main install.
refuses "another name on sandpit's default data dir" "on sandpit's default data directory" -- --name sandpit2
refuses "the name sandpit on another data dir" "--name sandpit is the default install" -- --data "$OTHER"
refuses "the name sandpit via SANDPIT_DATA elsewhere" "--name sandpit is the default install" SANDPIT_DATA="$OTHER" --
refuses "bad --name ''" "bad --name" -- --uninstall --name ''
refuses "bad --name .." "bad --name" -- --uninstall --name ..
refuses "bad --name a/b" "bad --name" -- --uninstall --name a/b
before=$(snapshot)
run "$T/repo" -- --check --name sb --data "$OTHER" -- --listen 127.0.0.1:7910 > /dev/null && [ "$(snapshot)" = "$before" ] \
  && pass "--check passes a good install and writes nothing" || fail "--check on a good install"
run "$T/repo" -- --check > /dev/null && pass "--check passes the default install (7900 is clear of wispd)" || fail "--check on the default install"
run "$T/repo" -- --name sandpit2 --force-pair > /dev/null && pass "--force-pair: another name on sandpit's data dir" || fail "--force-pair refused"
# Existing units are never repointed.
fresh; mkdata "$OTHER"
run "$T/repo" -- > /dev/null
refuses "repointing sandpit.service at another data dir" "exists and runs" -- --data "$OTHER" --force-pair
run "$T/repo" -- --name sb --data "$OTHER" -- --listen 127.0.0.1:7910 > /dev/null
OTHER2=$H/other2; mkdata "$OTHER2"
refuses "repointing sb.service at another data dir" "exists and runs" -- --name sb --data "$OTHER2"
sed -i 's|^ExecStart=[^ ]*|ExecStart=/opt/elsewhere/sandpitd|' "$H/.config/systemd/user/sandpit.service"
refuses "default install over a sandpit.service running another binary" "exists and runs" --
refuses "uninstalling a unit running another binary" "not sandpit's to remove" -- --uninstall
# A remembered listen on wispd's port is caught too.
sed -i 's|^SANDPIT_FLAGS=.*|SANDPIT_FLAGS=--listen 127.0.0.1:7788|' "$H/.config/sandpit/sb.env"
refuses "a remembered --listen on 7788" "7788 or 7789 is wispd's" -- --name sb --data "$OTHER"
# An env file without SANDPIT_FLAGS= is not silently reused.
printf 'SOMETHING_ELSE=1\n' > "$H/.config/sandpit/sb.env"
refuses "an env file without SANDPIT_FLAGS=" "has no SANDPIT_FLAGS= line" -- --name sb --data "$OTHER"
run "$T/repo" -- --name sb --data "$OTHER" -- --listen 127.0.0.1:1 > /dev/null \
  && grep -q '^SANDPIT_FLAGS=--listen 127.0.0.1:1$' "$H/.config/sandpit/sb.env" && pass "flags after -- rewrite a stale env file" || fail "flags after -- did not rewrite the stale env file"
run "$T/repo" -- --uninstall --name sb > /dev/null && [ ! -e "$H/.config/systemd/user/sb.service" ] && [ ! -e "$H/.local/lib/sandpit/sb" ] && [ -e "$H/.config/sandpit/sb.env" ] \
  && pass "--uninstall removes the unit and lib dir, keeps the env file" || fail "--uninstall"

# ---- --takeover of wispd itself: wisp.service on wisp's data dir becomes sandpitd ----
fresh
printf 'WISPD_FLAGS=--url-domain widgets.test --public-listen :8443 --api-listen 127.0.0.1:7789\n' > "$H/.config/wisp/wisp.env"
run "$T/repo" -- --check --name wisp --data "$WISPDATA" --takeover > "$T/out" && grep -q "taking over wisp's wispd" "$T/out" \
  && pass "--check passes a takeover of wispd" || { fail "--check --takeover of wispd:"; cat "$T/out"; }
if run "$T/repo" -- --name wisp --data "$WISPDATA" --takeover > "$T/out"; then
  U=$H/.config/systemd/user/wisp.service
  grep -qxF "ExecStart=$H/.local/lib/sandpit/wisp/sandpitd --data $WISPDATA \$SANDPIT_FLAGS" "$U" \
    && pass "wispd takeover: wisp.service now runs sandpitd on wisp's data" || { fail "wispd takeover unit:"; cat "$U"; }
  grep -qxF "SANDPIT_FLAGS=--listen 127.0.0.1:7788 --url-domain widgets.test --public-listen :8443 --api-listen 127.0.0.1:7789" "$H/.config/sandpit/wisp.env" \
    && pass "wispd takeover carries WISPD_FLAGS over, with wispd's --listen 7788 made explicit" || fail "wispd takeover env: $(cat "$H/.config/sandpit/wisp.env")"
  ! grep -q '^After=wisp.service' "$U" && pass "wispd takeover: wisp.service is not ordered after itself" || fail "wisp.service orders after itself"
  [ -e "$H/.local/lib/wisp/wisp/wispd" ] && [ -e "$H/.config/wisp/wisp.env" ] \
    && pass "wispd takeover keeps wispd and its env file for a rollback" || fail "wispd takeover removed wisp's files"
  grep -q "took over from wisp's wispd" "$T/out" && pass "wispd takeover says what it took over" || { fail "takeover output:"; cat "$T/out"; }
  run "$T/repo" -- --name wisp --data "$WISPDATA" > /dev/null && pass "after taking wispd over, a plain reinstall of wisp is fine" || fail "reinstall of wisp after takeover refused"
  run "$T/repo" -- --name wisp --data "$WISPDATA" -- --listen 127.0.0.1:7788 --api-listen 127.0.0.1:7789 --e2b-listen 127.0.0.1:7791 > /dev/null \
    && pass "taken over, wisp keeps wispd's ports 7788/7789" || fail "wisp refused its own ports"
  run "$T/repo" -- --uninstall --name wisp > /dev/null && pass "taken over, wisp.service is sandpit's to uninstall" || fail "uninstall of taken-over wisp refused"
else
  fail "wispd takeover refused:"; cat "$T/out"
fi
fresh
sed -i 's|^ExecStart=[^ ]*wispd |ExecStart=/opt/elsewhere/wispd |' "$H/.config/systemd/user/wisp.service"
refuses "--takeover of a wisp.service not running wisp's wispd" "replaces wisp's wispd" -- --name wisp --data "$WISPDATA" --takeover
fresh
refuses "another name on wisp's data dir, even with --takeover" "only wisp.service runs on it" -- --name sb --data "$WISPDATA" --takeover

# ---- --takeover: geek's sandboxd.service (wisp's sandboxd on /bulk/sandboxd) ----
# What wisp's make install-sandboxd NAME=sandboxd DATA=... left behind.
mk_wisp_sandboxd() { # data dir
  local wl=$H/.local/lib/wisp/sandboxd
  mkdir -p "$wl"
  printf '#!/bin/sh\n# wisp sandboxd\n' > "$wl/sandboxd"; printf '#!/bin/sh\n# wispd\n' > "$wl/wispd"; chmod +x "$wl/"*
  cat > "$H/.config/systemd/user/sandboxd.service" <<EOF
# Generated by wisp scripts/install-service.sh; re-run it rather than editing.
[Unit]
Description=wisp API daemon (sandboxd, data in $1)
After=wisp.service

[Service]
Type=exec
EnvironmentFile=$H/.config/wisp/sandboxd.env
ExecStartPre=$wl/wait-host.sh
ExecStart=$wl/sandboxd --data $1 \$SANDBOXD_FLAGS
KillMode=mixed
EOF
  printf '# sandboxd flags for sandboxd.service\nSANDBOXD_FLAGS=--listen 127.0.0.1:7790 --e2b-listen 127.0.0.1:7791 --net-pool 1\n' > "$H/.config/wisp/sandboxd.env"
}
fresh
BULK=$H/bulk/sandboxd; mkdata "$BULK"
mk_wisp_sandboxd "$BULK"
wisp_before=$(wisp_files)
refuses "replacing wisp's sandboxd without --takeover" "pass --takeover" -- --name sandboxd --data "$BULK"
refuses "--takeover onto another data dir" "--takeover keeps the data directory" -- --name sandboxd --data "$OTHER" --takeover
refuses "--takeover with nothing to take over" "no .*sbx.service to take over" -- --name sbx --data "$OTHER" --takeover
sed -i 's|^ExecStart=[^ ]*sandboxd |ExecStart=/opt/wispd |' "$H/.config/systemd/user/sandboxd.service"
refuses "--takeover of a unit not running wisp's sandboxd" "--takeover replaces wisp's sandboxd" -- --name sandboxd --data "$BULK" --takeover
refuses "uninstalling wisp's sandboxd.service" "not sandpit's to remove" -- --uninstall --name sandboxd
mk_wisp_sandboxd "$BULK"
run "$T/repo" -- --check --name sandboxd --data "$BULK" --takeover > "$T/out" && grep -q "taking over wisp's sandboxd" "$T/out" \
  && pass "--check passes a takeover" || { fail "--check --takeover:"; cat "$T/out"; }
rm -f "$T/calls"
if run "$T/repo" -- --name sandboxd --data "$BULK" --takeover > "$T/out"; then
  LIB=$H/.local/lib/sandpit/sandboxd
  grep -qx "ExecStart=$LIB/sandpitd --data $BULK \$SANDPIT_FLAGS" "$H/.config/systemd/user/sandboxd.service" \
    && grep -qx "EnvironmentFile=$H/.config/sandpit/sandboxd.env" "$H/.config/systemd/user/sandboxd.service" \
    && grep -qx 'After=wisp.service' "$H/.config/systemd/user/sandboxd.service" \
    && pass "takeover: same unit name and data, now sandpitd" || { fail "takeover unit:"; cat "$H/.config/systemd/user/sandboxd.service"; }
  grep -qx 'SANDPIT_FLAGS=--listen 127.0.0.1:7790 --e2b-listen 127.0.0.1:7791 --net-pool 1' "$H/.config/sandpit/sandboxd.env" \
    && pass "takeover carries SANDBOXD_FLAGS over" || fail "takeover env: $(cat "$H/.config/sandpit/sandboxd.env")"
  [ -x "$H/.local/lib/wisp/sandboxd/sandboxd" ] && grep -q '^SANDBOXD_FLAGS=' "$H/.config/wisp/sandboxd.env" \
    && pass "takeover keeps wisp's lib dir and env file for a rollback" || fail "takeover removed wisp's files"
  grep -q 'systemctl --user restart sandboxd.service' "$T/calls" && pass "takeover restarts sandboxd.service" || { fail "systemctl calls:"; cat "$T/calls"; }
  run "$T/repo" -- --name sandboxd --data "$BULK" > /dev/null && pass "after a takeover, a plain reinstall is fine" || fail "reinstall after takeover refused"
  run "$T/repo" -- --name sandboxd --data "$BULK" --takeover > /dev/null && pass "a repeated --takeover is harmless" || fail "repeated --takeover refused"
else
  fail "takeover refused:"; cat "$T/out"
fi
fresh; mkdata "$BULK"; mk_wisp_sandboxd "$BULK"
run "$T/repo" -- --name sandboxd --data "$BULK" --takeover -- --listen 127.0.0.1:7900 > /dev/null \
  && grep -qx 'SANDPIT_FLAGS=--listen 127.0.0.1:7900' "$H/.config/sandpit/sandboxd.env" \
  && pass "takeover with flags after -- uses them" || fail "takeover with flags"
fresh; mkdata "$BULK"; mk_wisp_sandboxd "$BULK"
sed -i 's|^SANDBOXD_FLAGS=.*|SANDBOXD_FLAGS=--listen 127.0.0.1:7788|' "$H/.config/wisp/sandboxd.env"
refuses "takeover carrying over a listen on 7788" "7788 or 7789 is wispd's" -- --name sandboxd --data "$BULK" --takeover

# ---- the Makefile (make -n: nothing is run) ----
mk() { env -u NAME -u DATA -u FLAGS -u TAKEOVER -u SANDPIT_DATA -u XDG_DATA_HOME -u VARIANTS -u MAKEFLAGS -u MAKELEVEL -u MFLAGS HOME="$H" make -s -n -C "$SRC" "$@" 2>&1; }
out=$(mk install-service)
want="./scripts/install-service.sh --check --name sandpit --data $H/.local/share/sandpit"
check_line=$(grep -nxF -- "$want" <<<"$out" | cut -d: -f1)
initrd_line=$(grep -nx './scripts/build-initrd.sh' <<<"$out" | cut -d: -f1)
install_line=$(grep -nxF -- "./scripts/install-service.sh --name sandpit --data $H/.local/share/sandpit" <<<"$out" | cut -d: -f1)
[ -n "$check_line" ] && [ -n "$initrd_line" ] && [ -n "$install_line" ] && [ "$check_line" -lt "$initrd_line" ] && [ "$initrd_line" -lt "$install_line" ] \
  && pass "make install-service: checks, then the initrd, then the install (sandpit on the default data dir)" || { fail "make install-service recipe:"; echo "$out"; }
out=$(mk install-service NAME=sandboxd DATA=/bulk/sandboxd TAKEOVER=1 FLAGS='--listen 127.0.0.1:7910')
grep -qxF -- "./scripts/install-service.sh --name sandboxd --data /bulk/sandboxd --takeover -- --listen 127.0.0.1:7910" <<<"$out" \
  && pass "make install-service NAME= DATA= TAKEOVER=1 FLAGS= passes them on" || { fail "make install-service with args:"; echo "$out"; }
out=$(mk install-service DATA=/srv/sandpit/data FORCE_PAIR=1)
grep -qxF -- "./scripts/install-service.sh --name sandpit --data /srv/sandpit/data --force-pair" <<<"$out" \
  && pass "make install-service FORCE_PAIR=1 passes --force-pair" || { fail "make install-service FORCE_PAIR=1:"; echo "$out"; }
env -u SANDPIT_DATA -u XDG_DATA_HOME -u MAKEFLAGS -u MAKELEVEL -u MFLAGS NAME=somehost DATA=/data FLAGS=-x HOME="$H" make -s -n -C "$SRC" install-service 2>&1 | grep -qxF -- "./scripts/install-service.sh --name sandpit --data $H/.local/share/sandpit" \
  && pass "make install-service ignores NAME/DATA/FLAGS from the environment" || fail "make install-service tripped on environment NAME/DATA/FLAGS"
for t in install-service initrd images; do
  mk "$t" DATA="$H/.local/share/wisp" > "$T/out" && fail "make $t into wisp's data dir" || { grep -q "wisp's data directory" "$T/out" && pass "make $t refuses wisp's data dir" || fail "make $t into wisp's data dir: $(cat "$T/out")"; }
done
mk initrd | grep -q "initrd: writing $H/.local/share/sandpit/initrd.cpio" && pass "make initrd names its (default) data dir" || fail "make initrd output"
SANDPIT_DATA=/x make -s -n -C "$SRC" image | grep -q "image: writing /x/images/base.ext4" && pass "make image honours SANDPIT_DATA and says so" || fail "make image output"
out=$(mk images)
grep -q '^./scripts/fetch-deps.sh$' <<<"$out" && grep -q 'for v in base e2b vercel daytona modal; do' <<<"$out" && grep -q "$H/.local/share/sandpit/images" <<<"$out" \
  && pass "make images fetches deps and builds every variant (modal included) into the default data dir" || { fail "make images recipe:"; echo "$out"; }
mk images DATA=/x | grep -q 'writing /x/images/' && pass "make images DATA= builds elsewhere" || fail "make images DATA="
mk images VARIANTS=e2b | grep -q 'for v in e2b; do' && pass "make images VARIANTS picks the disks" || fail "make images VARIANTS"
mk build netd | grep -q 'go build -o bin/sandpitd ./cmd/sandpitd' && mk netd | grep -q 'bin/sandpit-netd ./cmd/sandpit-netd' \
  && pass "make build/netd build sandpitd and sandpit-netd" || fail "make build/netd"
mk e2e | grep -q 'SPRITES_E2E_URL=http://127.0.0.1:7900' && pass "make e2e defaults to 127.0.0.1:7900" || fail "make e2e URL"

echo
[ "$fails" = 0 ] && echo "all install-service.sh checks passed" || { echo "$fails check(s) failed"; exit 1; }
