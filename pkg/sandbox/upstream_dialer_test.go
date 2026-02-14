package sandbox

import (
	"errors"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestCreateUpstreamDialer_DefaultSystemDialer(t *testing.T) {
	d, err := createUpstreamDialer(nil, t.TempDir(), "vm-test")
	require.NoError(t, err)
	require.NotNil(t, d)
	require.NoError(t, d.Close())
}

func TestCreateUpstreamDialer_TailscaleMissingAuthKey(t *testing.T) {
	t.Setenv("MATCHLOCK_TEST_TS_AUTH_KEY", "")

	_, err := createUpstreamDialer(&api.NetworkConfig{
		Tailscale: &api.TailscaleConfig{
			Enabled:    true,
			AuthKeyEnv: "MATCHLOCK_TEST_TS_AUTH_KEY",
		},
	}, t.TempDir(), "vm-test")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMissingTailscaleAuthKey))
}
