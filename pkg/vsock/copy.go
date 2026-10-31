package vsock

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	// copyChunkSize is the payload size of archive frames sent to the guest.
	copyChunkSize = 256 * 1024
	// maxCopyFrameSize bounds frames read from the guest, which is untrusted.
	maxCopyFrameSize = 4 << 20
)

// CopyIn streams a copy archive to the guest agent and waits until it has been
// extracted. The guest validates the destination before any data is sent.
// The caller supplies an already-dialed conn; CopyIn takes ownership and
// closes it. Cancelling ctx closes the connection, which makes the guest stop
// the copy. A single copied file is never left half-written, but an
// interrupted directory copy may leave partial results.
func CopyIn(ctx context.Context, conn net.Conn, req copyarchive.CopyInRequest, archive io.Reader) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	data, err := json.Marshal(req)
	if err != nil {
		return errx.Wrap(ErrEncodeCopyRequest, err)
	}
	if err := SendMessage(conn, MsgTypeCopyIn, data); err != nil {
		return ctxErr(ctx, errx.Wrap(ErrWriteRequest, err))
	}

	msgType, payload, err := readCopyFrame(conn)
	if err != nil {
		return ctxErr(ctx, err)
	}
	switch msgType {
	case MsgTypeReady:
	case MsgTypeFileResult:
		if err := fileResultError(payload); err != nil {
			return err
		}
		return errx.With(ErrUnexpectedCopyMsg, ": guest finished before receiving the archive")
	default:
		return errx.With(ErrUnexpectedCopyMsg, ": type=%d", msgType)
	}

	result := make(chan error, 1)
	go func() {
		msgType, payload, err := readCopyFrame(conn)
		switch {
		case err != nil:
			result <- ctxErr(ctx, err)
		case msgType != MsgTypeFileResult:
			result <- errx.With(ErrUnexpectedCopyMsg, ": type=%d", msgType)
		default:
			result <- fileResultError(payload)
		}
	}()

	done := make(chan struct{})
	defer close(done)
	chunks := copyarchive.ReadChunks(archive, copyChunkSize, done)
	for {
		select {
		case err := <-result:
			// The guest stopped early. Tell it the stream is over so it can
			// close cleanly, then report its result.
			_ = SendMessage(conn, MsgTypeStdin, nil)
			if err == nil {
				err = errx.With(ErrUnexpectedCopyMsg, ": guest finished before the archive ended")
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case chunk := <-chunks:
			if len(chunk.Data) > 0 {
				if err := SendMessage(conn, MsgTypeStdin, chunk.Data); err != nil {
					// The guest's result explains a failed write, if it sent one.
					return firstErr(<-result, ctxErr(ctx, errx.Wrap(ErrWriteRequest, err)))
				}
			}
			switch {
			case chunk.Err == nil:
			case errors.Is(chunk.Err, io.EOF):
				if err := SendMessage(conn, MsgTypeStdin, nil); err != nil {
					return firstErr(<-result, ctxErr(ctx, errx.Wrap(ErrWriteRequest, err)))
				}
				return <-result
			default:
				// Abort the guest so it stops the copy.
				conn.Close()
				<-result
				return ctxErr(ctx, errx.Wrap(ErrReadCopyArchive, chunk.Err))
			}
		}
	}
}

// CopyOut asks the guest agent for a copy archive of req.Path. It returns the
// source description and a reader for the archive, which reports a guest-side
// failure instead of io.EOF. Closing the reader closes conn, which aborts the
// transfer in the guest. The caller supplies an already-dialed conn and must
// close the returned reader.
func CopyOut(ctx context.Context, conn net.Conn, req copyarchive.CopyOutRequest) (copyarchive.Source, io.ReadCloser, error) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (copyarchive.Source, io.ReadCloser, error) {
		stop()
		conn.Close()
		return copyarchive.Source{}, nil, err
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fail(errx.Wrap(ErrEncodeCopyRequest, err))
	}
	if err := SendMessage(conn, MsgTypeCopyOut, data); err != nil {
		return fail(ctxErr(ctx, errx.Wrap(ErrWriteRequest, err)))
	}

	msgType, payload, err := readCopyFrame(conn)
	if err != nil {
		return fail(ctxErr(ctx, err))
	}
	switch msgType {
	case MsgTypeCopyInfo:
	case MsgTypeFileResult:
		if err := fileResultError(payload); err != nil {
			return fail(err)
		}
		return fail(errx.With(ErrUnexpectedCopyMsg, ": guest sent no archive"))
	default:
		return fail(errx.With(ErrUnexpectedCopyMsg, ": type=%d", msgType))
	}

	var source copyarchive.Source
	if err := json.Unmarshal(payload, &source); err != nil {
		return fail(errx.Wrap(ErrReadResponseData, err))
	}
	if err := copyarchive.ValidateSource(source); err != nil {
		return fail(err)
	}
	return source, &copyOutReader{ctx: ctx, conn: conn, stop: stop, onSkip: req.OnSkip}, nil
}

type copyOutReader struct {
	ctx    context.Context
	conn   net.Conn
	stop   func() bool
	onSkip func(name, reason string)
	buf    []byte
	err    error
}

func (r *copyOutReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		msgType, payload, err := readCopyFrame(r.conn)
		switch {
		case err != nil:
			r.err = ctxErr(r.ctx, err)
		case msgType == MsgTypeStdout:
			r.buf = payload
		case msgType == MsgTypeStderr:
			copyarchive.ReportSkip(r.onSkip, payload)
		case msgType == MsgTypeFileResult:
			r.err = fileResultError(payload)
			if r.err == nil {
				r.err = io.EOF
			}
		default:
			r.err = errx.With(ErrUnexpectedCopyMsg, ": type=%d", msgType)
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *copyOutReader) Close() error {
	r.stop()
	return r.conn.Close()
}

// readCopyFrame reads one framed message, refusing oversized frames so a guest
// cannot make the host allocate arbitrary amounts of memory.
func readCopyFrame(conn net.Conn) (uint8, []byte, error) {
	header := make([]byte, 5)
	if _, err := ReadFull(conn, header); err != nil {
		return 0, nil, errx.Wrap(ErrReadResponseHeader, err)
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > maxCopyFrameSize {
		return 0, nil, errx.With(ErrCopyFrameTooLarge, ": %d bytes", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := ReadFull(conn, payload); err != nil {
			return 0, nil, errx.Wrap(ErrReadResponseData, err)
		}
	}
	return header[0], payload, nil
}

func fileResultError(payload []byte) error {
	var resp FileResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return errx.Wrap(ErrReadResponseData, err)
	}
	if resp.Error != "" {
		return errx.With(ErrFileRemote, ": %s", resp.Error)
	}
	return nil
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
