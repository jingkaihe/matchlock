package state

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const logFollowPollInterval = 100 * time.Millisecond

// ReadLog returns the current contents of a VM log.
func (m *Manager) ReadLog(id string) ([]byte, error) {
	if _, err := m.Get(id); err != nil {
		return nil, err
	}
	return ReadLogFile(m.LogPath(id))
}

// StreamLog writes the current VM log to w and, when follow is true, continues
// streaming appended data until ctx is cancelled.
func (m *Manager) StreamLog(ctx context.Context, id string, w io.Writer, follow bool) error {
	if _, err := m.Get(id); err != nil {
		return err
	}
	return CopyLogFile(ctx, m.LogPath(id), w, follow)
}

// ReadLogFile returns the current contents of path.
func ReadLogFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errx.Wrap(ErrReadLog, err)
	}
	return data, nil
}

// CopyLogFile writes the current contents of path to w and, when follow is
// true, continues streaming appended data until ctx is cancelled.
func CopyLogFile(ctx context.Context, path string, w io.Writer, follow bool) error {
	var offset int64

	for {
		nextOffset, err := copyLogChunk(path, offset, w)
		if err != nil {
			return err
		}
		offset = nextOffset

		if !follow {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(logFollowPollInterval):
		}
	}
}

func copyLogChunk(path string, offset int64, w io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return offset, nil
		}
		return offset, errx.Wrap(ErrReadLog, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return offset, errx.Wrap(ErrReadLog, err)
	}
	if info.Size() < offset {
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, errx.Wrap(ErrReadLog, err)
	}

	n, err := io.Copy(w, f)
	if err != nil {
		return offset, errx.Wrap(ErrWriteLog, err)
	}
	return offset + n, nil
}
