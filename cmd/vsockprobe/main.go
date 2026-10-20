//go:build linux

// vsockprobe runs INSIDE a QEMU guest. It dials host CID 2:<port> (a sandbox's
// VFS vsock port), sends a real pkg/vfs.VFSRequest (OpLookup "<path>") serialized
// with the shared cbor codec, and classifies the outcome precisely:
//
//   - ACCEPTED     — received a complete, decoded VFS response. The peer-CID
//     filter let this guest's source CID in, and the server dispatched.
//   - REJECTED     — transport was established (connect succeeded, a frame was
//     written or read-start reached) then EOF or ECONNRESET with NO valid
//     response. This is the expected isolation outcome for a foreign guest.
//   - INCONCLUSIVE — timeout/EAGAIN, connect/setup failure, invalid framing, CBOR
//     error, partial/truncated response, or a write error other than EPIPE/ECONNRESET.
//     These do NOT prove isolation and the caller must treat them as failure.
//
// Exit codes: 0 accepted, 1 rejected, 3 inconclusive, 64 usage.
//
// Usage:
//
//	vsockprobe                          -> own sandbox's port from /proc/cmdline, path "/"
//	vsockprobe <port>                   -> dial <port>, path "/"
//	vsockprobe <port> <path>            -> dial <port>, lookup <path>
//
// Build (static): CGO_ENABLED=0 go build -o vsockprobe ./cmd/vsockprobe
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"golang.org/x/sys/unix"
)

const (
	afVsock       = 40
	vmaddrCIDHost = 2
	vmaddrAny     = 0xffffffff
	maxFrame      = 1 << 20
	// A classified rejection is an EOF or reset after connect. Everything else is
	// inconclusive. Distinguish by errors.Is on io.EOF / ECONNRESET.
)

func main() { os.Exit(run()) }

func run() int {
	// `--port` (or `-p` alone) just prints this sandbox's VFS port and exits.
	if len(os.Args) >= 2 && (os.Args[1] == "--port" || os.Args[1] == "-p") {
		cmdline, err := os.ReadFile("/proc/cmdline")
		if err != nil {
			fmt.Fprintf(os.Stderr, "report port: read cmdline: %v\n", err)
			return 64
		}
		p, err := parseCmdlinePort(string(cmdline))
		if err != nil {
			fmt.Fprintf(os.Stderr, "report port: %v\n", err)
			return 64
		}
		fmt.Fprintf(os.Stdout, "PORT %d\n", p)
		return 0
	}

	port, path, err := targetPort(os.Args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: %s [port] [path]\n", os.Args[0])
		return 64
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "empty path")
		return 64
	}

	fd, err := unix.Socket(afVsock, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE socket err=%v\n", err)
		return 3
	}
	defer unix.Close(fd)

	tv := unix.NsecToTimeval(int64(3 * time.Second))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE rcvtimeo err=%v\n", err)
		return 3
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE sndtimeo err=%v\n", err)
		return 3
	}

	err = unix.Connect(fd, &unix.SockaddrVM{CID: vmaddrCIDHost, Port: port})
	if err != nil {
		// Connect failure before any exchange: could be a filter that closed
		// immediately, OR no listener, OR transient. This is NOT proof of
		// rejection — the server may simply not be there. Classify as
		// inconclusive so the caller cannot mistake it for isolation.
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE connect err=%v\n", err)
		return 3
	}

	req := vfs.VFSRequest{Op: vfs.OpLookup, Path: path}
	body, err := cbor.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE marshal-after-connect err=%v\n", err)
		return 3
	}
	var frame [4]byte
	binary.BigEndian.PutUint32(frame[:], uint32(len(body)))

	rw := &fdIO{fd: fd}
	// Write the frame. A reset/EOF here can be an immediate filter rejection
	// (still a classified rejection, because connect succeeded and the peer
	// closed on first traffic). Other write errors are inconclusive.
	if err := writeAll(rw, frame[:]); err != nil {
		if isResetOrEOF(err) {
			fmt.Fprintf(os.Stdout, "RESULT REJECTED write-hdr err=%v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE write-hdr err=%v\n", err)
		return 3
	}
	if err := writeAll(rw, body); err != nil {
		if isResetOrEOF(err) {
			fmt.Fprintf(os.Stdout, "RESULT REJECTED write-body err=%v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE write-body err=%v\n", err)
		return 3
	}

	resp, rerr := readResponse(rw)
	switch {
	case rerr == nil && resp != nil:
		// A complete decoded response means the filter accepted us. The dispatch
		// Err is the provider's result (may be non-zero for ENOENT etc.), which
		// is NOT a transport rejection.
		fmt.Fprintf(os.Stdout, "RESULT ACCEPTED stat=%v dispatcherr=%d lookup_path=%s\n",
			statString(resp.Stat), resp.Err, path)
		return 0
	case rerr != nil && isRejectErr(rerr):
		fmt.Fprintf(os.Stdout, "RESULT REJECTED reason=%v\n", rerr)
		return 1
	default:
		fmt.Fprintf(os.Stdout, "RESULT INCONCLUSIVE reason=%v\n", rerr)
		return 3
	}
}

// targetPort resolves the port (explicit arg, or own sandbox's port from
// /proc/cmdline when no port arg is given) and the lookup path (default "/").
func targetPort(args []string) (uint32, string, error) {
	path := "/"
	switch len(args) {
	case 1:
		cmdline, err := os.ReadFile("/proc/cmdline")
		if err != nil {
			return 0, "", fmt.Errorf("read /proc/cmdline: %w", err)
		}
		p, err := parseCmdlinePort(string(cmdline))
		if err != nil {
			return 0, "", fmt.Errorf("resolve port from cmdline: %w", err)
		}
		return p, path, nil
	case 2:
		p, err := parsePort(args[1])
		if err != nil {
			return 0, "", err
		}
		return p, path, nil
	case 3:
		p, err := parsePort(args[1])
		if err != nil {
			return 0, "", err
		}
		return p, args[2], nil
	default:
		return 0, "", fmt.Errorf("usage")
	}
}

func parsePort(s string) (uint32, error) {
	p, err := strconv.ParseUint(s, 10, 32)
	if err != nil || p == 0 || p == vmaddrAny {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint32(p), nil
}

func parseCmdlinePort(cmdline string) (uint32, error) {
	var (
		found string
		seen  bool
	)
	for _, part := range strings.Fields(cmdline) {
		if !strings.HasPrefix(part, "matchlock.vfs_port=") {
			continue
		}
		if seen {
			return 0, fmt.Errorf("matchlock.vfs_port specified more than once")
		}
		seen = true
		found = strings.TrimPrefix(part, "matchlock.vfs_port=")
	}
	if !seen {
		return 0, fmt.Errorf("matchlock.vfs_port not present in cmdline")
	}
	return parsePort(found)
}

// fdIO adapts a raw fd to io.Reader/io.Writer, translating (0,nil) from unix.Read
// into io.EOF and retrying EINTR.
type fdIO struct{ fd int }

func (r *fdIO) Read(p []byte) (int, error) {
	for {
		n, err := unix.Read(r.fd, p)
		if err == unix.EINTR {
			continue
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
}

func (r *fdIO) Write(p []byte) (int, error) {
	for {
		n, err := unix.Write(r.fd, p)
		if err == unix.EINTR {
			continue
		}
		return n, err
	}
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		if n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// readResponse reads a length-framed VFS response. It returns the decoded
// response and the error; on the error path it reports how many bytes of the
// header it actually consumed (via errBytes) so the caller can TELL a rejection
// (EOF with zero bytes — peer closed before any response) apart from a genuine
// partial/corrupt response (which must be inconclusive, not a rejection).
func readResponse(r io.Reader) (*vfs.VFSResponse, error) {
	var lenBuf [4]byte
	n, err := io.ReadFull(r, lenBuf[:])
	if err != nil {
		return nil, &readErr{wrap: fmt.Errorf("read-len: %w", err), headerBytes: n, err: err}
	}
	msgLen := binary.BigEndian.Uint32(lenBuf[:])
	if msgLen == 0 || msgLen > maxFrame {
		return nil, &readErr{wrap: fmt.Errorf("bad frame len %d", msgLen), headerBytes: n, err: nil}
	}
	buf := make([]byte, msgLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, &readErr{wrap: fmt.Errorf("read-body: %w", err), headerBytes: n, err: err}
	}
	var resp vfs.VFSResponse
	if err := cbor.Unmarshal(buf, &resp); err != nil {
		return nil, &readErr{wrap: fmt.Errorf("decode: %w", err), headerBytes: n, err: err}
	}
	return &resp, nil
}

// readErr decorates a read failure with how many header bytes were consumed, so
// the rejection classifier can require a zero-byte header (peer never spoke).
type readErr struct {
	wrap        error
	headerBytes int
	err         error
}

func (r *readErr) Error() string { return r.wrap.Error() }
func (r *readErr) Unwrap() error { return r.err }

// isRejectErr reports whether an error from readResponse is a classified
// rejection: the peer closed (EOF/reset) with ZERO bytes of the response header
// received — i.e. it never answered our request. Timeouts, invalid lengths, CBOR
// decode errors, and partial reads (any header bytes received) are NOT
// rejections; those are inconclusive because we genuinely reached the server.
func isRejectErr(err error) bool {
	re, ok := err.(*readErr)
	if !ok {
		return false
	}
	// Any header bytes consumed means the server sent SOMETHING — that is not a
	// clean rejection (it's a partial/corrupt response -> inconclusive).
	if re.headerBytes != 0 {
		return false
	}
	return errors.Is(re.err, io.EOF) || errors.Is(re.err, unix.ECONNRESET) || errors.Is(re.err, syscall.ECONNRESET)
}

// isResetOrEOF reports whether a write error is a classified rejection.
func isResetOrEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, unix.EPIPE) ||
		errors.Is(err, unix.ECONNRESET) || errors.Is(err, syscall.ECONNRESET)
}

func statString(s *vfs.VFSStat) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("size=%d mode=%o isdir=%v", s.Size, s.Mode, s.IsDir)
}
