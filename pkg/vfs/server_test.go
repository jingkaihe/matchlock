package vfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDispatchCreateReturnsStat(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	resp := s.dispatch(&VFSRequest{
		Op:   OpCreate,
		Path: "/file.txt",
		Mode: 0644,
	})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.False(t, resp.Stat.IsDir)
	assert.Equal(t, uint32(0644), resp.Stat.Mode&0777)
	assert.NotZero(t, resp.Handle)

	release := s.dispatch(&VFSRequest{Op: OpRelease, Handle: resp.Handle})
	require.Equal(t, int32(0), release.Err)
}

func TestDispatchMkdirReturnsStat(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	resp := s.dispatch(&VFSRequest{
		Op:   OpMkdir,
		Path: "/repo",
		Mode: 0755,
	})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.True(t, resp.Stat.IsDir)
	assert.Equal(t, uint32(0755), resp.Stat.Mode&0777)
}
