package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePortForwardRemoteOnly(t *testing.T) {
	pf, err := ParsePortForward("8080")
	require.NoError(t, err)
	assert.Equal(t, 8080, pf.LocalPort)
	assert.Equal(t, 8080, pf.RemotePort)
}

func TestParsePortForwardLocalAndRemote(t *testing.T) {
	pf, err := ParsePortForward("18080:8080")
	require.NoError(t, err)
	assert.Equal(t, 18080, pf.LocalPort)
	assert.Equal(t, 8080, pf.RemotePort)
}

func TestParsePortForwards(t *testing.T) {
	pfs, err := ParsePortForwards([]string{"8080", "19090:9090"})
	require.NoError(t, err)
	require.Len(t, pfs, 2)
	assert.Equal(t, PortForward{LocalPort: 8080, RemotePort: 8080}, pfs[0])
	assert.Equal(t, PortForward{LocalPort: 19090, RemotePort: 9090}, pfs[1])
}

func TestParsePortForwardInvalidSpec(t *testing.T) {
	_, err := ParsePortForward("1:2:3")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPortForwardSpecFormat)
}

func TestParsePortForwardInvalidPort(t *testing.T) {
	_, err := ParsePortForward("0:8080")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPortForwardPort)
}
