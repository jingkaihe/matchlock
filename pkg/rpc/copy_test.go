package rpc

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
	"github.com/jingkaihe/matchlock/pkg/api"
)

// copyVM records copy requests and serves a one-file archive for copy_out.
type copyVM struct {
	*mockVM
	copyIn  *copyarchive.CopyInRequest
	copyOut *copyarchive.CopyOutRequest
}

func (v *copyVM) CopyIn(_ context.Context, req copyarchive.CopyInRequest, archive io.Reader) error {
	v.copyIn = &req
	_, err := io.Copy(io.Discard, archive)
	if req.Dest == "/fail" {
		return errors.New("guest failure")
	}
	return err
}

func (v *copyVM) CopyOut(_ context.Context, req copyarchive.CopyOutRequest) (copyarchive.Source, io.ReadCloser, error) {
	v.copyOut = &req
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "out.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	return copyarchive.Source{Name: "out.txt"}, io.NopCloser(&buf), nil
}

func TestHandleCopy(t *testing.T) {
	host := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(host, "src"), 0o755))
	limit := int64(0)

	tests := []struct {
		name     string
		method   string
		params   copyParams
		wantCode int
		check    func(t *testing.T, vm *copyVM)
	}{
		{name: "missing dest", method: "copy_in", params: copyParams{Src: host}, wantCode: ErrCodeInvalidParams},
		{name: "relative host src", method: "copy_in", params: copyParams{Src: "src", Dest: "/w"}, wantCode: ErrCodeInvalidParams},
		{name: "owner with archive", method: "copy_in", params: copyParams{Src: host, Dest: "/w", Owner: "1", Archive: true}, wantCode: ErrCodeInvalidParams},
		{name: "relative host dest", method: "copy_out", params: copyParams{Src: "/w", Dest: "out"}, wantCode: ErrCodeInvalidParams},
		{name: "guest failure", method: "copy_in", params: copyParams{Src: host + "/src", Dest: "/fail"}, wantCode: ErrCodeFileFailed},
		{
			name: "copy in", method: "copy_in",
			params: copyParams{Src: host + "/src/.", Dest: "/w", Owner: "app", Exclude: []string{"*.log"}},
			check: func(t *testing.T, vm *copyVM) {
				require.NotNil(t, vm.copyIn)
				assert.Equal(t, "/w", vm.copyIn.Dest)
				assert.Equal(t, "app", vm.copyIn.Owner)
				assert.Equal(t, &copyarchive.Source{Name: "src", IsDir: true, ContentsOnly: true}, vm.copyIn.Source)
			},
		},
		{
			name: "copy out", method: "copy_out",
			params: copyParams{Src: "/w/out.txt", Dest: host + "/copied.txt", FollowLink: true, MaxSize: &limit},
			check: func(t *testing.T, vm *copyVM) {
				assert.Equal(t, &copyarchive.CopyOutRequest{Path: "/w/out.txt", FollowLink: true}, vm.copyOut)
				data, err := os.ReadFile(filepath.Join(host, "copied.txt"))
				require.NoError(t, err)
				assert.Equal(t, "hi", string(data))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := &copyVM{mockVM: &mockVM{id: "vm-copy"}}
			rpc := newTestRPCWithFactory(func(context.Context, *api.Config) (VM, error) { return vm, nil })
			defer rpc.close()
			rpc.send("create", 1, map[string]string{"image": "alpine:latest"})
			require.Nil(t, rpc.read().Error)

			rpc.send(tt.method, 2, tt.params)
			resp := rpc.read()
			if tt.wantCode != 0 {
				require.NotNil(t, resp.Error)
				assert.Equal(t, tt.wantCode, resp.Error.Code)
				return
			}
			require.Nil(t, resp.Error)
			assert.JSONEq(t, "{}", string(resp.Result))
			tt.check(t, vm)
		})
	}
}
