#!/bin/bash
# Repro: host_fs FUSE rename() produces broken inodes
#
# Files created via rename (tmp + mv) through a host_fs mount can be listed
# and stat'd, but open() fails with ENOENT. Direct writes (echo >) work fine.
#
# This breaks git, which uses atomic writes (lockfile + rename) for nearly
# every mutating operation (checkout, commit, add, etc.).
#
# USAGE:
#   1. Build image:     matchlock build -t fuse-repro:latest examples/host-fs-rename-repro
#   2. Create test dir: (see below)
#   3. Launch VM:       matchlock run ... (see below)
#   4. Run tests:       (see below)
#
# SETUP:
#   rm -rf /tmp/matchlock-fuse-repro
#   mkdir -p /tmp/matchlock-fuse-repro
#   cd /tmp/matchlock-fuse-repro
#   git init && git config user.email "test@test.com" && git config user.name "Test"
#   mkdir -p src/utils
#   echo 'console.log("hello")' > src/index.js
#   echo 'export const add = (a, b) => a + b' > src/utils/math.js
#   echo '# Test Repo' > README.md
#   git add -A && git commit -m "initial commit"
#
# LAUNCH VM (blocks — use a second terminal for tests):
#   matchlock run \
#     --image fuse-repro:latest \
#     --workspace /workspace \
#     -v /tmp/matchlock-fuse-repro:/workspace/repo:host_fs \
#     --privileged \
#     --rm=false \
#     --
#
# TESTS (in second terminal, replace $VM with actual VM ID):

set -e

if [ -z "$1" ]; then
  echo "Usage: $0 <vm-id>"
  echo "  Run the tests against a running matchlock VM."
  exit 1
fi

VM=$1
PASS=0
FAIL=0

run_test() {
  local desc="$1"
  shift
  echo -n "  $desc: "
  if output=$("$@" 2>&1); then
    echo "PASS ($output)"
    PASS=$((PASS + 1))
  else
    echo "FAIL ($output)"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== Test: direct write (echo >) ==="
matchlock exec "$VM" -w /workspace/repo -- sh -c 'echo "direct" > new.txt'
run_test "cat new.txt" matchlock exec "$VM" -w /workspace/repo -- cat new.txt

echo ""
echo "=== Test: atomic write (tmp + mv) ==="
matchlock exec "$VM" -w /workspace/repo -- sh -c 'echo "atomic" > /workspace/repo/tmp.xyz && mv /workspace/repo/tmp.xyz /workspace/repo/renamed.txt'
run_test "stat renamed.txt" matchlock exec "$VM" -w /workspace/repo -- stat -c '%s bytes' renamed.txt
run_test "cat renamed.txt" matchlock exec "$VM" -w /workspace/repo -- cat renamed.txt

echo ""
echo "=== Test: git operations (use lockfile + rename internally) ==="
run_test "git status" matchlock exec "$VM" -w /workspace/repo -- git status
run_test "git log" matchlock exec "$VM" -w /workspace/repo -- git log --oneline
run_test "git checkout -b" matchlock exec "$VM" -w /workspace/repo -- git checkout -b test-branch
run_test "git status after checkout" matchlock exec "$VM" -w /workspace/repo -- git status
run_test "cat .git/HEAD" matchlock exec "$VM" -w /workspace/repo -- cat .git/HEAD

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] && echo "All tests passed." || echo "Some tests failed."
exit "$FAIL"
