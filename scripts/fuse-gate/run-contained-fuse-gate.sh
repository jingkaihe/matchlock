#!/usr/bin/env bash
# run-contained-fuse-gate.sh — run a command (typically a Go test gate that may
# mount real FUSE filesystems) inside an OWNED PRIVATE mount+pid namespace with
# a private /tmp, private mount propagation and isolated synthetic-fixture git
# auto-maintenance policy.
#
# Why: a host-FUSE gate that wedges (a daemon blocked on its own unserved
# connection, or a detached `git maintenance --auto --detach` client holding
# cwd+lock inside the mount) must never be able to leave a mount, fixture or
# process in the shared host namespace. Running the gate in a private mount+pid
# namespace guarantees that every mount, /tmp fixture and child process it
# creates dies with the namespace, while the pre-existing host mounts under /tmp
# are hidden by a private tmpfs so the gate cannot even traverse them.
#
# The driver records, for every gate: parent and child namespace ids (verified
# to DIFFER before any test runs), mount/process state inside the namespace
# before and after the command, the full producer output, the real producer exit
# code, and a leak check that fails the gate (never reports success) if the
# command left a FUSE mount or child process behind inside the namespace.
#
# Usage:
#   run-contained-fuse-gate.sh --out <evidence-dir> -- <command...>
#
# The evidence directory must be an owned fresh path (never a shared /tmp scan
# target). All state files are written there; nothing outside it is modified.
set -u

usage() {
  echo "usage: $0 --out <evidence-dir> -- <command...>" >&2
  exit 2
}

OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) OUT="$2"; shift 2 ;;
    --) shift; break ;;
    *) usage ;;
  esac
done
[ -n "$OUT" ] || usage
[ $# -gt 0 ] || usage

mkdir -p "$OUT" || { echo "cannot create $OUT" >&2; exit 2; }

HOST_MNT=$(stat -Lc %i /proc/self/ns/mnt) || exit 2
HOST_PID=$(stat -Lc %i /proc/self/ns/pid) || exit 2
HOST_FUSE_BEFORE=$(grep -c ' fuse\.' /proc/self/mountinfo || true)
HOST_FUSE_LIST_BEFORE=$(grep ' fuse\.' /proc/self/mountinfo || true)

date -u '+%Y-%m-%dT%H:%M:%SZ' > "$OUT/started.txt"
echo "host_mnt_ns=$HOST_MNT" > "$OUT/host-namespaces.txt"
echo "host_pid_ns=$HOST_PID" >> "$OUT/host-namespaces.txt"
echo "host_fuse_mounts_before=$HOST_FUSE_BEFORE" >> "$OUT/host-namespaces.txt"
printf '%s\n' "$HOST_FUSE_LIST_BEFORE" > "$OUT/host-fuse-mounts.before.txt"
printf 'cmd: %s\n' "$*" > "$OUT/command.txt"

# Child runner: written into the owned evidence dir (never into /tmp, which is
# replaced by a private tmpfs inside the namespace).
CHILD="$OUT/child-runner.sh"
cat > "$CHILD" <<'EOF'
#!/usr/bin/env bash
# Runs as PID 1 in the fresh mount+pid namespace.
set -u
OUT="$1"; shift
CMD=( "$@" )
mkdir -p "$OUT"

CHILD_MNT=$(stat -Lc %i /proc/self/ns/mnt)
CHILD_PID=$(stat -Lc %i /proc/self/ns/pid)
{
  echo "child_mnt_ns=$CHILD_MNT"
  echo "child_pid_ns=$CHILD_PID"
  echo "parent_mnt_ns=$PARENT_MNT_NS"
  echo "parent_pid_ns=$PARENT_PID_NS"
} > "$OUT/child-namespaces.txt"

# CONTAINMENT GATE: never execute a FUSE test in the shared namespace. If the
# namespaces did not actually change, fail explicitly before running anything.
if [ "$CHILD_MNT" = "$PARENT_MNT_NS" ] || [ "$CHILD_PID" = "$PARENT_PID_NS" ]; then
  echo "FATAL: namespace containment not established (mnt=$CHILD_MNT pid=$CHILD_PID)" | tee "$OUT/containment-fail.txt"
  exit 91
fi
echo "namespace containment verified: mnt $CHILD_MNT != $PARENT_MNT_NS, pid $CHILD_PID != $PARENT_PID_NS" | tee "$OUT/containment-ok.txt"

# Private propagation for every subtree of this namespace: nothing mounted here
# can propagate into the shared host namespace.
mount --make-rprivate / || { echo "FATAL: mount --make-rprivate / failed" | tee "$OUT/containment-fail.txt"; exit 91; }

# Private /tmp: fresh tmpfs hides any pre-existing host mounts/fixtures under
# /tmp (the wedged legacy mounts are unreachable from the gate by construction)
# and keeps every t.TempDir() fixture inside the namespace.
if ! mount -t tmpfs tmpfs /tmp; then
  echo "FATAL: cannot mount private tmpfs over /tmp" | tee "$OUT/containment-fail.txt"
  exit 91
fi

# Synthetic-fixture git policy (process-scoped; operator/global git config and
# product git behaviour untouched): no auto gc/maintenance and no background
# auto-detach child from ANY git the gate spawns. A detached
# `git maintenance run --auto --detach` child is exactly what previously held
# cwd + .git/objects/maintenance.lock inside a mount and wedged teardown.
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_COUNT=4
export GIT_CONFIG_KEY_0=maintenance.auto
export GIT_CONFIG_VALUE_0=false
export GIT_CONFIG_KEY_1=gc.auto
export GIT_CONFIG_VALUE_1=0
export GIT_CONFIG_KEY_2=maintenance.autoDetach
export GIT_CONFIG_VALUE_2=false
export GIT_CONFIG_KEY_3=gc.autoDetach
export GIT_CONFIG_VALUE_3=false

NS_FUSE_BEFORE=$(grep -c ' fuse\.' /proc/self/mountinfo || true)
{
  echo "ns_fuse_mounts_before=$NS_FUSE_BEFORE"
  echo "ns_mnt_ns=$CHILD_MNT"
  date -u '+%Y-%m-%dT%H:%M:%SZ'
} > "$OUT/ns-state.before.txt"
grep ' fuse\.' /proc/self/mountinfo > "$OUT/ns-fuse-mounts.before.txt" || true

echo "=== child running: ${CMD[*]} ==="
set +e
"${CMD[@]}" > "$OUT/gate.log" 2>&1
RC=$?
set -e
echo "$RC" > "$OUT/gate.exit"

NS_FUSE_AFTER=$(grep -c ' fuse\.' /proc/self/mountinfo || true)
grep ' fuse\.' /proc/self/mountinfo > "$OUT/ns-fuse-mounts.after.txt" || true
{
  echo "ns_fuse_mounts_after=$NS_FUSE_AFTER"
  echo "ns_fuse_mounts_before=$NS_FUSE_BEFORE"
  echo "producer_exit=$RC"
  date -u '+%Y-%m-%dT%H:%M:%SZ'
} > "$OUT/ns-state.after.txt"

# Leak check: a passing gate must not leave a FUSE mount or a live child
# (fused.test / git maintenance) behind in the namespace. A leftover mount is an
# unconfirmed obligation: the gate FAILS and the obligation is recorded — never
# report success over a still-mounted fixture. (The namespace teardown on this
# shell's exit still guarantees nothing survives into the host namespace.)
LEAKED=0
if [ "$NS_FUSE_AFTER" -gt "$NS_FUSE_BEFORE" ]; then
  LEAKED=1
  echo "LEAK: FUSE mounts left behind inside namespace (before=$NS_FUSE_BEFORE after=$NS_FUSE_AFTER)" | tee "$OUT/leak-mounts.txt"
fi
LEFTOVER_PROCS=""
for p in /proc/[0-9]*/cmdline; do
  cmd=$(tr '\0' ' ' < "$p" 2>/dev/null || true)
  case "$cmd" in
    *fused.test*|*"git maintenance"*|*"git gc"*)
      LEFTOVER_PROCS="$LEFTOVER_PROCS pid=${p#/proc/} cmd=$cmd" ;;
  esac
done
if [ -n "$LEFTOVER_PROCS" ]; then
  LEAKED=1
  echo "LEAK: child processes left behind:$LEFTOVER_PROCS" | tee "$OUT/leak-procs.txt"
fi

if [ "$RC" -ne 0 ]; then
  echo "gate FAILED with producer exit $RC" | tee "$OUT/result.txt"
  exit "$RC"
fi
if [ "$LEAKED" -ne 0 ]; then
  echo "gate FAILED: leaked namespace resources despite exit 0" | tee "$OUT/result.txt"
  exit 70
fi
echo "gate PASSED (exit 0, no leaked mounts/processes)" | tee "$OUT/result.txt"
exit 0
EOF
chmod +x "$CHILD"

# Run the gate as PID 1 of a fresh mount+pid namespace with a fresh /proc.
PARENT_MNT_NS="$HOST_MNT" PARENT_PID_NS="$HOST_PID" \
  unshare --mount --pid --fork --mount-proc --propagation private \
  "$CHILD" "$OUT" "$@"
RC=$?
echo "unshare wrapper exit: $RC"

# Host-side verification: the private namespace must have kept everything out of
# the shared host namespace.
HOST_FUSE_AFTER=$(grep -c ' fuse\.' /proc/self/mountinfo || true)
{
  echo "host_fuse_mounts_after=$HOST_FUSE_AFTER"
  echo "host_fuse_mounts_before=$HOST_FUSE_BEFORE"
} >> "$OUT/host-namespaces.txt"
if [ "$HOST_FUSE_AFTER" -ne "$HOST_FUSE_BEFORE" ]; then
  echo "HOST LEAK: host FUSE mount count changed during contained gate ($HOST_FUSE_BEFORE -> $HOST_FUSE_AFTER)" | tee "$OUT/host-leak.txt"
  RC=80
fi

exit "$RC"
