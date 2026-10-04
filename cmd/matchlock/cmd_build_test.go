package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

type fakeBuildExecer func(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error)

func (f fakeBuildExecer) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	return f(ctx, command, opts)
}

type fakeImageImporter func(r io.Reader, tag string) (*image.BuildResult, error)

func (f fakeImageImporter) Import(_ context.Context, r io.Reader, tag string) (*image.BuildResult, error) {
	return f(r, tag)
}

func TestBuildAndImportImageStreamsBuildOutput(t *testing.T) {
	payload := bytes.Repeat([]byte{0, 1, 255, '\n'}, 50000)
	execer := fakeBuildExecer(func(_ context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
		assert.Equal(t, "/workspace/buildkit-run.sh", command)
		for chunk := range slices.Chunk(payload, 4096) {
			if _, err := opts.Stdout.Write(chunk); err != nil {
				return nil, err
			}
		}
		return &api.ExecResult{}, nil
	})
	var imported []byte
	importer := fakeImageImporter(func(r io.Reader, tag string) (*image.BuildResult, error) {
		assert.Equal(t, "app:latest", tag)
		data, err := io.ReadAll(r)
		imported = data
		return &image.BuildResult{Size: int64(len(data))}, err
	})

	result, err := buildAndImportImage(context.Background(), execer, importer, "/workspace/buildkit-run.sh", "app:latest")
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), result.Size)
	assert.Equal(t, payload, imported)
}

func TestBuildAndImportImageBuildFailureAbortsImport(t *testing.T) {
	execer := fakeBuildExecer(func(_ context.Context, _ string, opts *api.ExecOptions) (*api.ExecResult, error) {
		_, err := opts.Stdout.Write([]byte("partial tarball"))
		assert.NoError(t, err) // runs on the build goroutine; require would be unsafe
		return &api.ExecResult{ExitCode: 2}, nil
	})
	var importErr error
	importer := fakeImageImporter(func(r io.Reader, _ string) (*image.BuildResult, error) {
		_, importErr = io.ReadAll(r)
		return nil, importErr
	})

	_, err := buildAndImportImage(context.Background(), execer, importer, "build.sh", "app:latest")
	require.ErrorIs(t, err, ErrBuildKitBuild)
	assert.Contains(t, err.Error(), "exit code 2")
	assert.NotErrorIs(t, err, ErrImportImage)
	assert.ErrorIs(t, importErr, ErrBuildKitBuild, "a failed build must not look like a complete tarball")
}

func TestBuildAndImportImageImportFailureStopsBuild(t *testing.T) {
	diskFull := errors.New("no space left on device")
	var writeErr error
	execer := fakeBuildExecer(func(_ context.Context, _ string, opts *api.ExecOptions) (*api.ExecResult, error) {
		for {
			if _, err := opts.Stdout.Write(make([]byte, 4096)); err != nil {
				writeErr = err
				return nil, errx.Wrap(vsock.ErrWriteExecOutput, err)
			}
		}
	})
	importer := fakeImageImporter(func(r io.Reader, _ string) (*image.BuildResult, error) {
		_, err := io.ReadFull(r, make([]byte, 1024))
		require.NoError(t, err)
		return nil, diskFull
	})

	_, err := buildAndImportImage(context.Background(), execer, importer, "build.sh", "app:latest")
	require.ErrorIs(t, err, ErrImportImage)
	assert.ErrorIs(t, err, diskFull)
	assert.NotErrorIs(t, err, ErrBuildKitBuild)
	assert.ErrorIs(t, writeErr, io.ErrClosedPipe, "the build output stream must be closed")
}

func TestBuildAndImportImageExecError(t *testing.T) {
	execer := fakeBuildExecer(func(context.Context, string, *api.ExecOptions) (*api.ExecResult, error) {
		return nil, context.Canceled
	})
	importer := fakeImageImporter(func(r io.Reader, _ string) (*image.BuildResult, error) {
		_, err := io.ReadAll(r)
		return nil, err
	})

	_, err := buildAndImportImage(context.Background(), execer, importer, "build.sh", "app:latest")
	assert.ErrorIs(t, err, ErrBuildKitBuild)
	assert.ErrorIs(t, err, context.Canceled)
}
