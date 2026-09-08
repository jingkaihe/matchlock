package rpc

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
)

func TestResolveImageRPCSuccess(t *testing.T) {
	stdinR, stdinW := pipe()
	stdoutR, stdoutW := pipe()

	resolver := func(ctx context.Context, tag string) (*image.Identity, error) {
		assert.Equal(t, "igorhvr/bedlam-ubuntu", tag)
		return &image.Identity{
			Tag:          "igorhvr/bedlam-ubuntu",
			Digest:       "sha256:ccc",
			ConfigDigest: "sha256:cfg",
			Source:       "import",
			Size:         1234,
		}, nil
	}

	h := NewHandler(func(ctx context.Context, config *api.Config) (VM, error) {
		return &mockVM{id: "vm-x"}, nil
	}, stdinR, stdoutW, WithImageResolver(resolver))
	rpc := newTestRPCWithHandler(h, stdinW, stdoutR)
	defer rpc.close()

	rpc.send("resolve_image", 7, map[string]string{"tag": "igorhvr/bedlam-ubuntu"})
	msg := rpc.read()
	require.Nil(t, msg.Error, "resolve_image should succeed")
	require.NotNil(t, msg.Result)

	var id image.Identity
	require.NoError(t, jsonUnmarshal(msg.Result, &id))
	assert.Equal(t, "igorhvr/bedlam-ubuntu", id.Tag)
	assert.Equal(t, "sha256:ccc", id.Digest)
	assert.Equal(t, "sha256:cfg", id.ConfigDigest)
	assert.Equal(t, "import", id.Source)
}

func TestResolveImageRPCNoResolver(t *testing.T) {
	stdinR, stdinW := pipe()
	stdoutR, stdoutW := pipe()

	h := NewHandler(func(ctx context.Context, config *api.Config) (VM, error) {
		return &mockVM{id: "vm-x"}, nil
	}, stdinR, stdoutW)
	rpc := newTestRPCWithHandler(h, stdinW, stdoutR)
	defer rpc.close()

	rpc.send("resolve_image", 8, map[string]string{"tag": "alpine:latest"})
	msg := rpc.read()
	require.NotNil(t, msg.Error)
	assert.Equal(t, ErrCodeInvalidRequest, msg.Error.Code)
}

func TestResolveImageRPCNotFound(t *testing.T) {
	stdinR, stdinW := pipe()
	stdoutR, stdoutW := pipe()

	resolver := func(ctx context.Context, tag string) (*image.Identity, error) {
		return nil, image.ErrImageNotFound
	}

	h := NewHandler(func(ctx context.Context, config *api.Config) (VM, error) {
		return &mockVM{id: "vm-x"}, nil
	}, stdinR, stdoutW, WithImageResolver(resolver))
	rpc := newTestRPCWithHandler(h, stdinW, stdoutR)
	defer rpc.close()

	rpc.send("resolve_image", 9, map[string]string{"tag": "does:not-exist"})
	msg := rpc.read()
	require.NotNil(t, msg.Error)
	assert.Equal(t, ErrCodeInvalidParams, msg.Error.Code)
}

func TestResolveImageRPCMissingTag(t *testing.T) {
	stdinR, stdinW := pipe()
	stdoutR, stdoutW := pipe()

	resolver := func(ctx context.Context, tag string) (*image.Identity, error) {
		return &image.Identity{Tag: tag}, nil
	}

	h := NewHandler(func(ctx context.Context, config *api.Config) (VM, error) {
		return &mockVM{id: "vm-x"}, nil
	}, stdinR, stdoutW, WithImageResolver(resolver))
	rpc := newTestRPCWithHandler(h, stdinW, stdoutR)
	defer rpc.close()

	rpc.send("resolve_image", 10, map[string]string{})
	msg := rpc.read()
	require.NotNil(t, msg.Error)
	assert.Equal(t, ErrCodeInvalidParams, msg.Error.Code)
}

func pipe() (r *io.PipeReader, w *io.PipeWriter) {
	return io.Pipe()
}

// jsonUnmarshal is a tiny indirection to keep the test readable.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
