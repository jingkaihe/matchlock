//go:build darwin

package darwin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSocketPairCloseIgnoresAlreadyClosedHostFile(t *testing.T) {
	sp, err := createSocketPair()
	require.NoError(t, err)

	require.NoError(t, sp.HostFile().Close())
	require.NoError(t, sp.Close())
}

func TestSocketPairCloseIsIdempotent(t *testing.T) {
	sp, err := createSocketPair()
	require.NoError(t, err)

	require.NoError(t, sp.Close())
	require.NoError(t, sp.Close())
}
