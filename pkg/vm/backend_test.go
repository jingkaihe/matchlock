package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSwapDisks(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *VMConfig
		wantErr bool
	}{
		{
			name: "nil config",
			cfg:  nil,
		},
		{
			name: "swap disk without custom kernel args",
			cfg: &VMConfig{
				ExtraDisks: []DiskConfig{{HostPath: "/swap.raw", Swap: true}},
			},
		},
		{
			name: "custom kernel args without swap disk",
			cfg: &VMConfig{
				KernelArgs: "console=ttyS0",
				ExtraDisks: []DiskConfig{{HostPath: "/data.ext4", GuestMount: "/mnt/data"}},
			},
		},
		{
			name: "custom kernel args with only normal disks",
			cfg: &VMConfig{
				KernelArgs: "console=ttyS0 matchlock.disk.vdb=/mnt",
				ExtraDisks: []DiskConfig{{HostPath: "/data.ext4", GuestMount: "/mnt"}},
			},
		},
		{
			name: "custom kernel args with swap disk",
			cfg: &VMConfig{
				KernelArgs: "console=ttyS0",
				ExtraDisks: []DiskConfig{
					{HostPath: "/data.ext4", GuestMount: "/mnt"},
					{HostPath: "/swap.raw", Swap: true},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSwapDisks(tt.cfg)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrSwapCustomKernelArgs)
				return
			}
			assert.NoError(t, err)
		})
	}
}
