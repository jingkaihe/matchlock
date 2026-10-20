//go:build linux

// crossconnect_driver launches two QEMU sandboxes (A and B), injects the static
// guest-side AF_VSOCK probe (vsockprobe) into both, and runs the discriminating
// cross-guest VFS isolation sequence entirely via the guest-originated probe:
//
//	A self-dial -> ACCEPTED (guest's own CID allowed)
//	B self-dial -> ACCEPTED
//	B dial-A    -> REJECTED (B's CID != A's expected CID)
//	A dial-B    -> REJECTED
//	A self-dial -> ACCEPTED (again; VFS service not disturbed)
//
// The driver only orchestrates via the SDK and reads both ports straight from
// each guest's /proc/cmdline through the probe's --port mode. No SDK changes,
// state-DB discovery, or internal-machine access are needed.
//
// Run on linux with /dev/vhost-vsock (the VM), MATCHLOCK_BACKEND=qemu.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "-diag" {
		if err := runDiag(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runDiag launches ONE sandbox and prints the guest cmdline + probe self-dial
// behavior, so we can confirm the guest exposes matchlock.vfs_port and that the
// probe's self-dial (positive control) is accepted before running the full
// two-guest sequence.
func runDiag() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	probe, err := os.ReadFile("/tmp/vsockprobe")
	if err != nil {
		return fmt.Errorf("read probe: %w", err)
	}
	client, err := newClient(ctx, probe, "D")
	if err != nil {
		return err
	}
	defer cleanup(client)

	for _, cmd := range []string{
		"cat /proc/cmdline",
		"/workspace/probe --port; echo PORT_RC=$?",
		"/workspace/probe; echo DIAL_RC=$?",
	} {
		res, err := client.Exec(ctx, cmd)
		if err != nil {
			return fmt.Errorf("exec %q: %w", cmd, err)
		}
		fmt.Printf("CMD %q => rc=%d\nSTDOUT:\n%s\nSTDERR:\n%s\n----\n", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return nil
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	probe, err := os.ReadFile("/tmp/vsockprobe")
	if err != nil {
		return fmt.Errorf("read probe: %w", err)
	}

	clientA, err := newClient(ctx, probe, "A")
	if err != nil {
		return err
	}
	defer cleanup(clientA)

	clientB, err := newClient(ctx, probe, "B")
	if err != nil {
		return err
	}
	defer cleanup(clientB)

	// Ensure two genuinely distinct, concurrently-live sandboxes.
	if clientA.VMID() == "" || clientB.VMID() == "" || clientA.VMID() == clientB.VMID() {
		return fmt.Errorf("not two distinct sandboxes: A=%q B=%q", clientA.VMID(), clientB.VMID())
	}

	// Read both VFS ports from the guests themselves.
	aPort, err := guestPort(ctx, clientA, "A")
	if err != nil {
		return err
	}
	bPort, err := guestPort(ctx, clientB, "B")
	if err != nil {
		return err
	}
	if aPort == bPort || aPort == 0 || bPort == 0 {
		return fmt.Errorf("ports not distinct/valid: A=%d B=%d", aPort, bPort)
	}
	fmt.Printf("A=%s port=%d\nB=%s port=%d\n", clientA.VMID(), aPort, clientB.VMID(), bPort)

	// Discriminating sequence against the exact outcome contract.
	type step struct {
		label string
		args  string
		want  probeOutcome
	}
	steps := []step{
		{"A->A", "dial", probeAccepted},
		{"B->B", "dial", probeAccepted},
		{"B->A", fmt.Sprintf("%d", aPort), probeRejected},
		{"A->B", fmt.Sprintf("%d", bPort), probeRejected},
		{"A->A-again", "dial", probeAccepted},
		{"B->B-again", "dial", probeAccepted},
	}
	for _, s := range steps {
		out, err := runProbe(ctx, clientA, clientB, s.label, s.args)
		if err != nil {
			return err
		}
		if out.outcome != s.want {
			return fmt.Errorf("step %s: got %s (out=%+v), want %v", s.label, out.classText, out, s.want)
		}
		fmt.Printf("step %s: %s ok\n", s.label, out.classText)
	}

	// Prove the VFS services still work after all cross-connections: an exec
	// that WRITES and reads back a marker must exit 0 and return the content.
	for _, c := range []struct {
		label  string
		client *sdk.Client
		marker string
	}{
		{"A", clientA, "a-alive"},
		{"B", clientB, "b-alive"},
	} {
		cmd := fmt.Sprintf("echo %s > /workspace/alive.txt; cat /workspace/alive.txt", c.marker)
		res, err := c.client.Exec(ctx, cmd)
		if err != nil {
			return fmt.Errorf("%s post-cross VFS write/read: %w", c.label, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s post-cross VFS write/read exit=%d stderr=%s", c.label, res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, c.marker) {
			return fmt.Errorf("%s post-cross VFS write/read did not return marker %q (got %q)", c.label, c.marker, res.Stdout)
		}
	}
	fmt.Println("post-cross VFS write/read ok; ISOLATION VERIFIED")
	return nil
}

// newClient launches a sandbox, injects the probe, and returns the client.
func newClient(ctx context.Context, probe []byte, label string) (*sdk.Client, error) {
	client, err := sdk.NewClient(sdk.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("NewClient %s: %w", label, err)
	}

	builder := sdk.New("alpine:latest").
		WithCPUs(0.5).
		WithWorkspace("/workspace").
		MountMemory("/workspace")
	if _, err := client.Launch(builder); err != nil {
		_ = client.Close(0)
		_ = client.Remove()
		return nil, fmt.Errorf("launch %s: %w", label, err)
	}
	if err := client.WriteFileMode(ctx, "/workspace/probe", probe, 0755); err != nil {
		_ = client.Close(0)
		_ = client.Remove()
		return nil, fmt.Errorf("WriteFileMode %s: %w", label, err)
	}
	return client, nil
}

func cleanup(client *sdk.Client) {
	if cerr := client.Close(5 * time.Second); cerr != nil {
		fmt.Fprintf(os.Stderr, "cleanup Close: %v\n", cerr)
	}
	if rerr := client.Remove(); rerr != nil {
		fmt.Fprintf(os.Stderr, "cleanup Remove: %v\n", rerr)
	}
}

// guestPort returns the sandbox's own VFS vsock port by running the probe in
// --port mode inside that guest and parsing "PORT <n>" from stdout.
func guestPort(ctx context.Context, client *sdk.Client, label string) (uint32, error) {
	res, err := client.Exec(ctx, "/workspace/probe --port")
	if err != nil {
		return 0, fmt.Errorf("%s probe --port: %w", label, err)
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("%s probe --port rc=%d stderr=%s", label, res.ExitCode, res.Stderr)
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) != 2 || fields[0] != "PORT" {
		return 0, fmt.Errorf("%s probe --port unexpected stdout %q", label, res.Stdout)
	}
	p, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil || p == 0 {
		return 0, fmt.Errorf("%s probe --port parse %q: %v", label, fields[1], err)
	}
	return uint32(p), nil
}

// runProbe executes the probe in the target guest as given by mkProbeArgs and
// returns a classified outcome.
func runProbe(ctx context.Context, clientA, clientB *sdk.Client, label, arg string) (probeResult, error) {
	client := clientA
	if strings.HasPrefix(label, "B") {
		client = clientB
	}
	cmd := "/workspace/probe"
	switch arg {
	case "dial":
		// no extra args: self-dial via /proc/cmdline
	case "":
		return probeResult{}, fmt.Errorf("empty probe arg for %s", label)
	default:
		cmd += " " + arg
	}
	res, err := client.Exec(ctx, cmd)
	if err != nil {
		return probeResult{}, fmt.Errorf("%s exec %q: %w", label, cmd, err)
	}
	return classify(label, res), nil
}

// probeOutcome is the exact expected classification.
type probeOutcome int

const (
	probeInconclusive probeOutcome = iota // zero value: unclassifiable must NOT pass
	probeAccepted
	probeRejected
)

type probeResult struct {
	label     string
	outcome   probeOutcome
	exitCode  int
	detailed  string
	classText string
}

func (p probeResult) String() string {
	return fmt.Sprintf("rc=%d class=%s %s", p.exitCode, p.classText, p.detailed)
}

// classify maps an ExecResult to an exact probe outcome. Only unambiguous
// outcomes are classified; a mismatched marker, nonzero rc, or empty output is
// INCONCLUSIVE (so the caller rejects it rather than treating it as a pass).
func classify(label string, res *sdk.ExecResult) probeResult {
	out := strings.TrimSpace(res.Stdout)
	pr := probeResult{label: label, exitCode: res.ExitCode, detailed: out}
	switch {
	case res.ExitCode == 0 && strings.Contains(out, "RESULT ACCEPTED"):
		pr.outcome = probeAccepted
		pr.classText = "ACCEPTED"
	case res.ExitCode == 1 && strings.Contains(out, "RESULT REJECTED"):
		pr.outcome = probeRejected
		pr.classText = "REJECTED"
	default:
		pr.outcome = probeInconclusive
		pr.classText = "INCONCLUSIVE"
	}
	return pr
}
