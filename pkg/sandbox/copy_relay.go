package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
	"github.com/jingkaihe/matchlock/internal/errx"
)

const relayCopyChunkSize = 256 * 1024

type relayCopyResult struct {
	Error string `json:"error,omitempty"`
}

// handleCopyIn forwards an archive streamed as relayMsgStdin frames (ending
// with an empty frame) to the guest and replies with relayMsgCopyResult. A
// client disconnect cancels the copy, which removes the partial result.
func (r *ExecRelay) handleCopyIn(conn net.Conn, data []byte) {
	var req copyarchive.CopyInRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendRelayCopyResult(conn, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	archive, archiveWriter := io.Pipe()
	go func() {
		for {
			msgType, msgData, err := readRelayMsg(conn)
			if err != nil {
				archiveWriter.CloseWithError(io.ErrUnexpectedEOF)
				cancel()
				return
			}
			if msgType != relayMsgStdin {
				continue
			}
			if len(msgData) == 0 {
				archiveWriter.Close()
				return
			}
			// Writes fail once the copy has ended; keep reading so the client
			// is not blocked before it sees the result.
			_, _ = archiveWriter.Write(msgData)
		}
	}()

	err := r.sb.CopyIn(ctx, req, archive)
	archive.Close()
	sendRelayCopyResult(conn, err)
}

// handleCopyOut replies with relayMsgCopyInfo, the archive as relayMsgStdout
// frames, skip notices as relayMsgStderr frames, and a final
// relayMsgCopyResult. A client disconnect cancels the copy.
func (r *ExecRelay) handleCopyOut(conn net.Conn, data []byte) {
	var req copyarchive.CopyOutRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendRelayCopyResult(conn, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// The client sends nothing more, so a read returns only when it leaves.
		_, _ = conn.Read(make([]byte, 1))
		cancel()
	}()

	sender := newRelaySender(conn)
	req.OnSkip = func(name, reason string) {
		_ = sender.Send(relayMsgStderr, copyarchive.EncodeSkip(name, reason))
	}
	source, archive, err := r.sb.CopyOut(ctx, req)
	if err != nil {
		sendRelayCopyResult(conn, err)
		return
	}
	defer archive.Close()
	info, err := json.Marshal(source)
	if err != nil {
		sendRelayCopyResult(conn, err)
		return
	}
	if err := sendRelayMsg(conn, relayMsgCopyInfo, info); err != nil {
		return
	}
	stdout := &relayWriter{sender: sender, msgType: relayMsgStdout}
	_, err = io.CopyBuffer(stdout, archive, make([]byte, relayCopyChunkSize))
	sendRelayCopyResult(conn, err)
}

func sendRelayCopyResult(conn net.Conn, err error) {
	var result relayCopyResult
	if err != nil {
		result.Error = err.Error()
	}
	data, _ := json.Marshal(result)
	_ = sendRelayMsg(conn, relayMsgCopyResult, data)
}

func decodeRelayCopyResult(data []byte) error {
	var result relayCopyResult
	if err := json.Unmarshal(data, &result); err != nil {
		return errx.Wrap(ErrRelayDecode, err)
	}
	if result.Error != "" {
		return errx.With(ErrRelayCopy, ": %s", result.Error)
	}
	return nil
}

// RelayGuest copies files to and from a VM through the exec relay of the
// process that owns it.
type RelayGuest struct {
	SocketPath string
}

func (g RelayGuest) dial(ctx context.Context) (net.Conn, func() bool, error) {
	conn, err := net.Dial("unix", g.SocketPath)
	if err != nil {
		return nil, nil, errx.Wrap(ErrRelayConnect, err)
	}
	return conn, context.AfterFunc(ctx, func() { conn.Close() }), nil
}

// CopyIn implements copyarchive.Guest.
func (g RelayGuest) CopyIn(ctx context.Context, req copyarchive.CopyInRequest, archive io.Reader) error {
	conn, stop, err := g.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer stop()

	data, err := json.Marshal(req)
	if err != nil {
		return errx.Wrap(ErrRelaySend, err)
	}
	if err := sendRelayMsg(conn, relayMsgCopyIn, data); err != nil {
		return relayCtxErr(ctx, errx.Wrap(ErrRelaySend, err))
	}

	// Watch for the result while sending, so that an early rejection or
	// cancellation is seen even while waiting for input such as stdin.
	result := make(chan error, 1)
	go func() {
		msgType, data, err := readRelayMsg(conn)
		switch {
		case err != nil:
			result <- relayCtxErr(ctx, errx.Wrap(ErrRelayRead, err))
		case msgType != relayMsgCopyResult:
			result <- errx.With(ErrRelayUnexpected, ": %d", msgType)
		default:
			result <- decodeRelayCopyResult(data)
		}
	}()

	done := make(chan struct{})
	defer close(done)
	chunks := copyarchive.ReadChunks(archive, relayCopyChunkSize, done)
	sentEOF := false
	for {
		select {
		case err := <-result:
			if err == nil && !sentEOF {
				err = errx.With(ErrRelayUnexpected, ": copy finished before the archive ended")
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case chunk := <-chunks:
			if len(chunk.Data) > 0 {
				if err := sendRelayMsg(conn, relayMsgStdin, chunk.Data); err != nil {
					// The relay stopped reading; its result says why.
					chunks = nil
					continue
				}
			}
			switch {
			case chunk.Err == nil:
			case errors.Is(chunk.Err, io.EOF):
				_ = sendRelayMsg(conn, relayMsgStdin, nil)
				sentEOF = true
				chunks = nil
			default:
				// Returning closes the connection, which aborts the guest copy.
				return relayCtxErr(ctx, chunk.Err)
			}
		}
	}
}

// CopyOut implements copyarchive.Guest.
func (g RelayGuest) CopyOut(ctx context.Context, req copyarchive.CopyOutRequest) (copyarchive.Source, io.ReadCloser, error) {
	conn, stop, err := g.dial(ctx)
	if err != nil {
		return copyarchive.Source{}, nil, err
	}
	fail := func(err error) (copyarchive.Source, io.ReadCloser, error) {
		stop()
		conn.Close()
		return copyarchive.Source{}, nil, relayCtxErr(ctx, err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fail(errx.Wrap(ErrRelaySend, err))
	}
	if err := sendRelayMsg(conn, relayMsgCopyOut, data); err != nil {
		return fail(errx.Wrap(ErrRelaySend, err))
	}
	msgType, data, err := readRelayMsg(conn)
	if err != nil {
		return fail(errx.Wrap(ErrRelayRead, err))
	}
	switch msgType {
	case relayMsgCopyInfo:
	case relayMsgCopyResult:
		if err := decodeRelayCopyResult(data); err != nil {
			return fail(err)
		}
		return fail(errx.With(ErrRelayUnexpected, ": no archive"))
	default:
		return fail(errx.With(ErrRelayUnexpected, ": %d", msgType))
	}
	var source copyarchive.Source
	if err := json.Unmarshal(data, &source); err != nil {
		return fail(errx.Wrap(ErrRelayDecode, err))
	}
	return source, &relayCopyReader{ctx: ctx, conn: conn, stop: stop, onSkip: req.OnSkip}, nil
}

type relayCopyReader struct {
	ctx    context.Context
	conn   net.Conn
	stop   func() bool
	onSkip func(name, reason string)
	buf    []byte
	err    error
}

func (r *relayCopyReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		msgType, data, err := readRelayMsg(r.conn)
		switch {
		case err != nil:
			r.err = relayCtxErr(r.ctx, errx.Wrap(ErrRelayRead, err))
		case msgType == relayMsgStdout:
			r.buf = data
		case msgType == relayMsgStderr:
			copyarchive.ReportSkip(r.onSkip, data)
		case msgType == relayMsgCopyResult:
			if r.err = decodeRelayCopyResult(data); r.err == nil {
				r.err = io.EOF
			}
		default:
			r.err = errx.With(ErrRelayUnexpected, ": %d", msgType)
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *relayCopyReader) Close() error {
	r.stop()
	return r.conn.Close()
}

func relayCtxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
