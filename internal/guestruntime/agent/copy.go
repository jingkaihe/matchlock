//go:build linux

package guestagent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
	"github.com/jingkaihe/matchlock/internal/errx"
)

const copyFrameSize = 256 * 1024

// handleCopyIn extracts a copy archive streamed by the host as MsgTypeStdin
// frames, ending with an empty frame. It validates the destination and
// acknowledges with MsgTypeReady before reading the archive, and always reads
// up to the end of the stream before replying so the host is never left
// blocked on a write. If the host disconnects, the partial copy is removed.
func handleCopyIn(fd int, data []byte) {
	var req copyarchive.CopyInRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendFileResponse(fd, &FileResponse{Error: err.Error()})
		return
	}

	opts := copyarchive.ExtractOptions{ExactModes: true, PreserveOwner: req.PreserveOwner}
	if req.Owner != "" && !req.PreserveOwner {
		uid, gid, _, err := resolveUser(req.Owner)
		if err != nil {
			sendFileResponse(fd, &FileResponse{Error: err.Error()})
			return
		}
		opts.Owner = &copyarchive.Owner{UID: uid, GID: gid}
	}
	plan, err := copyarchive.PlanDestination(req.Dest, req.Source)
	if err != nil {
		sendFileResponse(fd, &FileResponse{Error: err.Error()})
		return
	}
	sendMessage(fd, MsgTypeReady, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &stdinFrameReader{fd: fd, onError: cancel}
	err = copyarchive.Extract(ctx, stream, plan, opts)
	if err != nil && stream.err == nil {
		// Report the failure now so the host stops sending, then read to the
		// end of the stream so the host's last writes do not fail.
		sendFileResponse(fd, &FileResponse{Error: err.Error()})
		_, _ = io.Copy(io.Discard, stream)
		return
	}
	// Consume the rest of the stream, including trailing tar padding.
	if _, drainErr := io.Copy(io.Discard, stream); drainErr != nil {
		// The host went away; nobody is waiting for a reply.
		return
	}
	resp := &FileResponse{}
	if err != nil {
		resp.Error = err.Error()
	}
	sendFileResponse(fd, resp)
}

// handleCopyOut streams a copy archive of the requested path to the host:
// MsgTypeCopyInfo with the source description, MsgTypeStdout frames with the
// archive, and a final MsgTypeFileResult. A host disconnect stops the walk.
func handleCopyOut(fd int, data []byte) {
	var req copyarchive.CopyOutRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendFileResponse(fd, &FileResponse{Error: err.Error()})
		return
	}
	source, readPath, err := copyarchive.StatSource(req.Path, req.FollowLink)
	if err != nil {
		sendFileResponse(fd, &FileResponse{Error: err.Error()})
		return
	}
	info, err := json.Marshal(source)
	if err != nil {
		sendFileResponse(fd, &FileResponse{Error: err.Error()})
		return
	}
	sendMessage(fd, MsgTypeCopyInfo, info)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := &stdoutFrameWriter{fd: fd, onError: cancel}
	bw := bufio.NewWriterSize(frames, copyFrameSize)
	err = copyarchive.WriteArchive(ctx, bw, readPath, source, copyarchive.WriteOptions{Exclude: req.Exclude})
	if err == nil {
		err = bw.Flush()
	}
	if frames.err != nil {
		return
	}
	resp := &FileResponse{}
	if err != nil {
		resp.Error = err.Error()
	}
	sendFileResponse(fd, resp)
}

// stdinFrameReader reads MsgTypeStdin frame payloads until an empty frame.
type stdinFrameReader struct {
	fd      int
	buf     []byte
	err     error
	onError func()
}

func (r *stdinFrameReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		msgType, data, err := readMessage(r.fd)
		switch {
		case err != nil:
			r.err = errx.Wrap(ErrEOF, err)
			r.onError()
		case msgType != MsgTypeStdin:
			continue
		case len(data) == 0:
			r.err = io.EOF
		default:
			r.buf = data
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// stdoutFrameWriter sends each write as a MsgTypeStdout frame and reports
// write failures, which mean the host disconnected.
type stdoutFrameWriter struct {
	fd      int
	err     error
	once    sync.Once
	onError func()
}

func (w *stdoutFrameWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > copyFrameSize {
			chunk = chunk[:copyFrameSize]
		}
		if err := sendFrame(w.fd, MsgTypeStdout, chunk); err != nil {
			w.err = err
			w.once.Do(w.onError)
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// sendFrame is sendMessage with error reporting.
func sendFrame(fd int, msgType uint8, data []byte) error {
	frame := make([]byte, 5+len(data))
	frame[0] = msgType
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)

	mu := frameWriteLock(fd)
	mu.Lock()
	defer mu.Unlock()
	return writeFull(fd, frame)
}
