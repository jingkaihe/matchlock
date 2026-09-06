#!/usr/bin/env bash
#
# darwin-crosscompile-gate.sh - prove the whole module's test surface still
# compiles for darwin/arm64 from a Linux host.
#
# There is no macOS runner in CI on the Linux host, so the darwin support is
# guarded by cross-compiling every package's test binary with
# CGO_ENABLED=0 GOOS=darwin GOARCH=arm64. Five packages link
# github.com/Code-Hex/vz/v3 (cgo/Objective-C) and cannot be cross-compiled
# without the macOS SDK; they are expected to fail INSIDE the vz module only.
# Any failure in any other package - or a cgo-package failure whose errors do
# not point into the vz module - is a genuine gate failure and exits non-zero.
#
# Usage:
#   scripts/darwin-crosscompile-gate.sh            # full gate
#   scripts/darwin-crosscompile-gate.sh --self-test
#
# The --self-test mode exercises the classification helpers with fixtures and
# never invokes the Go toolchain.
set -uo pipefail

export CGO_ENABLED=0
export GOOS=darwin
export GOARCH=arm64

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Packages that link github.com/Code-Hex/vz/v3 (cgo/Objective-C). They require
# the macOS SDK and are documented in AGENTS.md as expected cross-compile
# failures on a Linux host.
CGO_VZ_PKGS="cmd/matchlock pkg/diagnose pkg/rpc pkg/sandbox pkg/vm/darwin"

# is_cgo_vz_pkg <import-path> -> 0 when the package is one of the documented
# cgo/Code-Hex-vz packages. Accepts both the full import path and the
# repository-relative package path.
is_cgo_vz_pkg() {
	local want="${1#github.com/jingkaihe/matchlock/}"
	local p
	for p in $CGO_VZ_PKGS; do
		[ "$p" = "$want" ] && return 0
	done
	return 1
}

# vz_error_only <output> -> 0 when every compiler error line references the
# Code-Hex/vz module (a documented cross-compile limitation). Returns 1 when
# the output contains any other error line.
vz_error_only() {
	local out="$1"
	# Drop the "# <package>" build-error headers; keep only real error lines.
	# Any remaining error line that does not mention the vz module means the
	# failure is a genuine repo problem. The module cache path mangles the
	# import path to "!code-!hex/vz", so match the case-insensitive
	# "hex/vz" substring shared by both spellings.
	if printf '%s\n' "$out" | grep -E '^[^#].*\.go:[0-9]+' | grep -qiv 'hex/vz'; then
		return 1
	fi
	return 0
}

self_test() {
	local rc=0
	if ! is_cgo_vz_pkg "pkg/rpc"; then
		echo "self-test: pkg/rpc should be classified as cgo/vz" >&2
		rc=1
	fi
	if ! is_cgo_vz_pkg "github.com/jingkaihe/matchlock/pkg/vm/darwin"; then
		echo "self-test: full pkg/vm/darwin path should classify as cgo/vz" >&2
		rc=1
	fi
	if is_cgo_vz_pkg "pkg/net"; then
		echo "self-test: pkg/net must not be classified as cgo/vz" >&2
		rc=1
	fi
	if ! vz_error_only '# github.com/Code-Hex/vz/v3
/home/u/go/pkg/mod/github.com/!code-!hex/vz/v3@v3.7.1/foo_arm64.go:14:8: undefined: Bar'; then
		echo "self-test: pure vz-module error must be tolerated" >&2
		rc=1
	fi
	if vz_error_only '# github.com/jingkaihe/matchlock/pkg/net
/opt/matchlock/pkg/net/relay.go:10:2: undefined: relayHalfClose'; then
		echo "self-test: repo-file error must not be tolerated" >&2
		rc=1
	fi
	if [ "$rc" -eq 0 ]; then
		echo "self-test: OK"
	fi
	return "$rc"
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit $?
fi

cd "$REPO_ROOT" || exit 1

fail=0

echo "=== CGO_ENABLED=$CGO_ENABLED GOOS=$GOOS GOARCH=$GOARCH go list ./... ==="
pkgs="$(go list ./... 2>&1)" || {
	printf '%s\n' "$pkgs"
	echo "FAIL: go list ./... failed"
	exit 1
}
printf '%s\n' "$pkgs" | grep -v '^github.com/' | sed 's/^/WARN: non-package line: /' || true

echo
echo "=== go test -c per package ==="
for p in $pkgs; do
	out="$(go test -c -o /dev/null "$p" 2>&1)"
	rc=$?
	if [ "$rc" -eq 0 ]; then
		echo "OK   $p"
		continue
	fi
	if is_cgo_vz_pkg "$p" && vz_error_only "$out"; then
		echo "EXPECTED-CGO-FAIL $p"
		continue
	fi
	echo "FAIL $p"
	printf '%s\n' "$out"
	fail=1
done

echo
echo "=== acceptance test binary (./tests/acceptance/) ==="
if go test -c -tags acceptance -o /dev/null ./tests/acceptance/ 2>&1; then
	echo "OK   ./tests/acceptance/ (acceptance)"
else
	echo "FAIL ./tests/acceptance/ (acceptance)"
	fail=1
fi

echo
echo "=== focused non-cgo packages (./pkg/net/, ./pkg/vsock/) ==="
for p in ./pkg/net/ ./pkg/vsock/; do
	if go test -c -o /dev/null "$p" 2>&1; then
		echo "OK   $p"
	else
		echo "FAIL $p"
		fail=1
	fi
done

echo
echo "=== darwin go vet ./... ==="
vet_out="$(go vet ./... 2>&1)"
printf '%s\n' "$vet_out"
# go vet ./... reports the cgo packages too; the only tolerated error lines are
# the ones inside the Code-Hex/vz module. Anything else is a genuine darwin vet
# failure (paths may be repo-relative, so do not filter on $REPO_ROOT).
if printf '%s\n' "$vet_out" | grep -E '^[^#].*\.go:[0-9]+' | grep -qiv 'hex/vz'; then
	echo "FAIL: non-cgo darwin vet errors above"
	fail=1
fi

echo
if [ "$fail" -eq 0 ]; then
	echo "=== darwin cross-compile gate: PASS ==="
else
	echo "=== darwin cross-compile gate: FAIL ==="
fi
exit "$fail"
