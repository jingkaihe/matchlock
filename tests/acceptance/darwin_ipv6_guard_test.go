package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file deliberately carries no `acceptance` build tag so the darwin skip
// audit is exercised by the ordinary unit suite (`mise run test`, i.e.
// `go test ./...`), exactly like qemu_backend_gate_test.go. It never launches a
// VM: it reads the acceptance sources next to it and asserts, mechanically, that
// every acceptance test asserting Linux-backend IPv6 interception state starts
// with the darwin guard.
//
// Why this exists: the darwin backend is IPv4-only by design, so an acceptance
// test that asserts guest IPv6 interception state FAILS on macOS (the fork tip
// failed exactly once there: TestSDKIPv6EgressState). The fix is a skip, and a
// skip is easy to forget on the next IPv6 test, so the guard is pinned here
// rather than left to review. The darwin surface itself is proven to still
// COMPILE the acceptance package by scripts/darwin-crosscompile-gate.sh
// (`go test -c -tags acceptance`, GOOS=darwin), whose OK line for
// ./tests/acceptance/ this audit complements.

// darwinIPv6SkipReason is the canonical skip reason for a Linux-backend IPv6
// acceptance test on darwin. Every audited guard must use this exact string.
const darwinIPv6SkipReason = "IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature"

// darwinGuardVerdict records the audit outcome for one acceptance test.
type darwinGuardVerdict string

const (
	// verdictAlreadyGuarded: the test already carried the darwin guard.
	verdictAlreadyGuarded darwinGuardVerdict = "already guarded"
	// verdictGuardAdded: the darwin guard was added by this story.
	verdictGuardAdded darwinGuardVerdict = "guard added"
	// verdictNotApplicable: the test needs no guard. Every such entry must name a
	// file in darwinGuardExemptFiles, so the exemption is explicit and auditable.
	verdictNotApplicable darwinGuardVerdict = "not applicable"
)

// darwinGuardAuditEntry is one row of the audit in the US-005 story report.
type darwinGuardAuditEntry struct {
	File string
	Test string
	// Verdict is the reported per-test audit outcome.
	Verdict darwinGuardVerdict
	// Note is the short justification reported next to the verdict.
	Note string
}

// darwinGuardAudit is the audit list: every acceptance test that asserts
// Linux-backend IPv6 interception state (a per-VM guest ULA, a ::/0 guest route,
// ip6 nftables tables, or IPv6-over-proxy reachability). Their sources must all
// start with the darwin guard.
//
// ipv4_parity_ipv6_test.go is the reference: it carried the guard the story
// copies. The audited files are also scanned for test functions missing from
// this table, so a NEW IPv6 test added later without a guard fails this gate.
var darwinGuardAudit = []darwinGuardAuditEntry{
	{
		File:    "ipv4_parity_ipv6_test.go",
		Test:    "TestSDKIPv4InterceptionParityWithDualStackProxy",
		Verdict: verdictAlreadyGuarded,
		Note:    "reference guard: asserts the IPv6 half of a dual-stack sandbox (guest default route, matchlock6_<tap> table)",
	},
	{
		File:    "ipv6_egress_test.go",
		Test:    "TestSDKIPv6EgressState",
		Verdict: verdictGuardAdded,
		Note:    "the macOS failure that motivated the story: asserts a per-VM guest ULA and a ::/0 guest route",
	},
	{
		File:    "ipv6_interception_test.go",
		Test:    "TestSDKIPv6InterceptionAllowedRefusedAndNoLeak",
		Verdict: verdictAlreadyGuarded,
		Note:    "asserts IPv6 destinations are reached/refused through the dual-stack proxy; reason string normalized to the canonical one",
	},
	{
		File:    "ipv6_interception_test.go",
		Test:    "TestSDKIPv6PublicEgressThroughProxy",
		Verdict: verdictAlreadyGuarded,
		Note:    "asserts public IPv6 reachability through the dual-stack proxy; reason string normalized to the canonical one",
	},
	{
		File:    "ipv6_linklocal_test.go",
		Test:    "TestSDKIPv6LinkLocalBypass",
		Verdict: verdictGuardAdded,
		Note:    "TAP/QEMU IPv6 link-local diagnostic; darwin guard split out of the combined QEMU check so it skips with the canonical reason",
	},
	{
		File:    "ipv6_netdiag_test.go",
		Test:    "TestSDKIPv6NetDiag",
		Verdict: verdictGuardAdded,
		Note:    "asserts the per-TAP ip6 nftables table (matchlock6_<tap>); darwin guard split out of the combined QEMU check",
	},
	{
		File:    "allow_private_name_test.go",
		Test:    "TestSDKAllowPrivateNameEntryCoversResolvedLiteral",
		Verdict: verdictAlreadyGuarded,
		Note:    "added by US-004; enforces allow_private over the Linux interception path (nftables DNAT + passthrough proxy)",
	},
	{
		File:    "qemu_tap_reap_test.go",
		Test:    "TestQEMUTAPReapedOnCrash",
		Verdict: verdictNotApplicable,
		Note:    "audited and exempt: the file is `//go:build acceptance && linux`, so darwin never compiles it; it asserts the QEMU TAP lifecycle, not IPv6 state",
	},
}

// darwinGuardAuditFiles lists every acceptance source the audit covers. The
// audit scans these for test functions so a test absent from darwinGuardAudit
// cannot silently ship unguarded.
var darwinGuardAuditFiles = []string{
	"ipv4_parity_ipv6_test.go",
	"ipv6_egress_test.go",
	"ipv6_interception_test.go",
	"ipv6_linklocal_test.go",
	"ipv6_netdiag_test.go",
	"allow_private_name_test.go",
}

// darwinGuardExemptFiles lists audited sources whose tests need no darwin guard
// because the build tag keeps them off the darwin surface entirely. Their test
// functions are still scanned and must still appear in darwinGuardAudit with the
// verdictNotApplicable verdict, so the exemption is a recorded decision rather
// than an omission.
var darwinGuardExemptFiles = []string{
	"qemu_tap_reap_test.go", // //go:build acceptance && linux
}

// funcDeclRe matches a Go function declaration and captures its name.
var funcDeclRe = regexp.MustCompile(`(?m)^func ([A-Za-z0-9_]+)\(`)

// testFuncNames returns the names of every function declared in src.
func testFuncNames(src string) []string {
	var names []string
	for _, m := range funcDeclRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	return names
}

// funcSource extracts the source of the named function from src: from its
// declaration line to the start of the next top-level declaration (or EOF).
func funcSource(src, funcName string) (string, bool) {
	needle := "func " + funcName + "("
	start := strings.Index(src, "\n"+needle)
	switch {
	case start >= 0:
		start++ // step past the leading newline
	case strings.HasPrefix(src, needle):
		start = 0
	default:
		return "", false
	}
	rest := src[start:]
	if next := strings.Index(rest[1:], "\nfunc "); next >= 0 {
		rest = rest[:next+1]
	}
	return rest, true
}

// isBlankOrComment reports whether a source line carries neither a statement nor
// a closing brace, so the guard scan can look through them.
func isBlankOrComment(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "//")
}

// darwinGuardFirstStatement reports whether funcSource's first statement is the
// darwin guard with the canonical skip reason:
//
//	if runtime.GOOS != "linux" {
//		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
//	}
//
// It returns ok=false with a human-readable reason otherwise, so a failure names
// what the function actually starts with. The check is deliberately syntactic:
// it pins the guard so a future IPv6 acceptance test cannot ship without it, and
// it is what makes the audit assertion cheap enough to run in the unit suite.
func darwinGuardFirstStatement(source string) (bool, string) {
	lines := strings.Split(source, "\n")
	i := 0
	for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "func ") {
		i++
	}
	if i >= len(lines) {
		return false, "no func declaration found"
	}
	i++ // body starts after the signature
	for i < len(lines) && isBlankOrComment(lines[i]) {
		i++
	}
	if i >= len(lines) {
		return false, "empty function body"
	}
	if got := strings.TrimSpace(lines[i]); got != `if runtime.GOOS != "linux" {` {
		return false, fmt.Sprintf("first statement is %q, want `if runtime.GOOS != \"linux\" {`", got)
	}
	wantSkip := "t.Skip(" + strconv.Quote(darwinIPv6SkipReason) + ")"
	for j := i + 1; j < len(lines); j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "}" {
			return false, "darwin guard block has no t.Skip with the canonical reason"
		}
		if isBlankOrComment(lines[j]) {
			continue
		}
		if trimmed != wantSkip {
			return false, fmt.Sprintf("guard statement is %q, want t.Skip with the canonical reason %q", trimmed, darwinIPv6SkipReason)
		}
		return true, ""
	}
	return false, "unterminated darwin guard block"
}

// TestDarwinIPv6AcceptanceGuards is the US-005 audit gate: every audited test
// starts with the darwin guard, every test function in the audited files is
// accounted for in the audit table, and the canonical reason is pinned.
func TestDarwinIPv6AcceptanceGuards(t *testing.T) {
	audited := map[string]map[string]darwinGuardAuditEntry{}
	for _, entry := range darwinGuardAudit {
		scoped := append(append([]string{}, darwinGuardAuditFiles...), darwinGuardExemptFiles...)
		require.Contains(t, scoped, entry.File,
			"audit entry %s/%s must name a file in darwinGuardAuditFiles or darwinGuardExemptFiles", entry.File, entry.Test)
		exempt := containsString(darwinGuardExemptFiles, entry.File)
		if exempt {
			require.Equal(t, verdictNotApplicable, entry.Verdict,
				"%s is exempt from the darwin guard, so its audits must say %q", entry.File, verdictNotApplicable)
		} else {
			require.NotEqual(t, verdictNotApplicable, entry.Verdict,
				"%s is not in darwinGuardExemptFiles, so %s cannot be %q",
				entry.File, entry.Test, verdictNotApplicable)
		}
		if audited[entry.File] == nil {
			audited[entry.File] = map[string]darwinGuardAuditEntry{}
		}
		require.NotContains(t, audited[entry.File], entry.Test,
			"%s/%s appears twice in the audit table", entry.File, entry.Test)
		audited[entry.File][entry.Test] = entry
	}

	for _, file := range append(append([]string{}, darwinGuardAuditFiles...), darwinGuardExemptFiles...) {
		path := filepath.Join(".", file)
		raw, err := os.ReadFile(path)
		require.NoError(t, err, "audited acceptance source %s must be readable from the package dir", path)
		src := string(raw)

		// Coverage: every test function in the audited file is in the table, so a
		// new IPv6 assertion cannot ship without a verdict, and an exempt file
		// cannot grow an unlisted test either.
		for _, name := range testFuncNames(src) {
			if !strings.HasPrefix(name, "Test") {
				continue
			}
			_, listed := audited[file][name]
			assert.True(t, listed,
				"%s declares %s but the darwin audit table has no entry for it: "+
					"add it to darwinGuardAudit (or it will fail on darwin unguarded)", file, name)
		}

		for _, entry := range darwinGuardAudit {
			if entry.File != file {
				continue
			}
			entry := entry
			t.Run(file+"/"+entry.Test, func(t *testing.T) {
				source, found := funcSource(src, entry.Test)
				require.True(t, found, "%s must declare %s", file, entry.Test)
				if entry.Verdict == verdictNotApplicable {
					// The file's build tag keeps the test off darwin entirely; there is
					// no darwin run to skip, so no guard is required.
					return
				}
				ok, detail := darwinGuardFirstStatement(source)
				assert.True(t, ok,
					"%s: %s must begin with the darwin guard (%s): %s",
					file, entry.Test, entry.Verdict, detail)
			})
		}
	}
}

// containsString reports whether want is in list.
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestDarwinGuardFirstStatementClassifier unit-tests the syntactic classifier the
// audit gate rests on, so a classifier bug cannot make the audit vacuously pass.
// The fixtures mirror the real guard shapes: the canonical guard, the guard with
// an explanatory comment above the skip, a missing guard, a guard with the wrong
// reason, a non-first guard, and an unterminated block.
func TestDarwinGuardFirstStatementClassifier(t *testing.T) {
	canonical := "func TestX(t *testing.T) {\n" +
		"\tif runtime.GOOS != \"linux\" {\n" +
		"\t\tt.Skip(" + strconv.Quote(darwinIPv6SkipReason) + ")\n" +
		"\t}\n" +
		"\tdoWork(t)\n}\n"
	commented := "func TestX(t *testing.T) {\n" +
		"\t// The darwin backend is IPv4-only by design.\n" +
		"\tif runtime.GOOS != \"linux\" {\n" +
		"\t\t// Same guard as the IPv4/IPv6 parity sibling.\n" +
		"\t\tt.Skip(" + strconv.Quote(darwinIPv6SkipReason) + ")\n" +
		"\t}\n" +
		"\tdoWork(t)\n}\n"
	guardedThenQEMU := canonical + "func TestY(t *testing.T) {\n" +
		"\tif os.Getenv(\"MATCHLOCK_BACKEND\") != \"qemu\" {\n" +
		"\t\tt.Skipf(\"requires MATCHLOCK_BACKEND=qemu (got %q)\", os.Getenv(\"MATCHLOCK_BACKEND\"))\n" +
		"\t}\n}\n"

	cases := []struct {
		name       string
		src        string
		wantOK     bool
		wantDetail string
	}{
		{"canonical-guard-passes", canonical, true, ""},
		{"comment-before-and-inside-guard-passes", commented, true, ""},
		{"unguarded-test-fails", "func TestX(t *testing.T) {\n\tdoWork(t)\n}\n", false, "first statement"},
		{"wrong-reason-fails", "func TestX(t *testing.T) {\n" +
			"\tif runtime.GOOS != \"linux\" {\n" +
			"\t\tt.Skip(\"IPv6 interception is a Linux-backend feature\")\n" +
			"\t}\n}\n", false, "canonical reason"},
		{"guard-not-first-fails", "func TestX(t *testing.T) {\n" +
			"\tdoWork(t)\n" +
			"\tif runtime.GOOS != \"linux\" {\n" +
			"\t\tt.Skip(" + strconv.Quote(darwinIPv6SkipReason) + ")\n" +
			"\t}\n}\n", false, "first statement"},
		{"guard-without-skip-fails", "func TestX(t *testing.T) {\n" +
			"\tif runtime.GOOS != \"linux\" {\n" +
			"\t\tdoWork(t)\n" +
			"\t}\n}\n", false, "guard statement"},
		{"wrong-goos-check-fails", "func TestX(t *testing.T) {\n" +
			"\tif runtime.GOOS != \"darwin\" {\n" +
			"\t\tt.Skip(" + strconv.Quote(darwinIPv6SkipReason) + ")\n" +
			"\t}\n}\n", false, "first statement"},
		{"later-func-is-not-the-guard-of-the-earlier-one", guardedThenQEMU, true, ""},
		{"empty-body-fails", "func TestX(t *testing.T) {}\n", false, "empty function body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, detail := darwinGuardFirstStatement(tc.src)
			assert.Equal(t, tc.wantOK, ok, "detail=%q", detail)
			if tc.wantOK {
				assert.Empty(t, detail)
				return
			}
			assert.Contains(t, detail, tc.wantDetail)
		})
	}

	// funcSource must stop at the next declaration, which is what keeps the
	// guardedThenQEMU fixture from attributing TestY's body to TestX.
	x, found := funcSource(guardedThenQEMU, "TestX")
	require.True(t, found)
	assert.NotContains(t, x, "func TestY(")
	y, found := funcSource(guardedThenQEMU, "TestY")
	require.True(t, found)
	assert.Contains(t, y, "MATCHLOCK_BACKEND")
	_, found = funcSource(guardedThenQEMU, "TestMissing")
	assert.False(t, found)
}
