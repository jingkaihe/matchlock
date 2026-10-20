package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestKernelIPDNSSuffixIPv4Only pins the ip= DNS suffix rule: the ip= argument
// is colon-delimited, so an IPv6 resolver literal (which contains colons) must
// never be rendered into it — it would corrupt every field after it. IPv6
// resolvers still reach the guest through matchlock.dns= (KernelDNSParam).
func TestKernelIPDNSSuffixIPv4Only(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"single v4", []string{"8.8.8.8"}, ":8.8.8.8"},
		{"two v4", []string{"8.8.8.8", "8.8.4.4"}, ":8.8.8.8:8.8.4.4"},
		{"cap at two v4 after filtering", []string{"8.8.8.8", "8.8.4.4", "1.1.1.1"}, ":8.8.8.8:8.8.4.4"},
		{"v6 skipped", []string{"2606:4700:4700::1111"}, ""},
		{"v6 first then v4", []string{"2606:4700:4700::1111", "8.8.8.8"}, ":8.8.8.8"},
		{"v4 first then v6", []string{"8.8.8.8", "2606:4700:4700::1111"}, ":8.8.8.8"},
		{"v6 interleaved", []string{"2606:4700:4700::1111", "8.8.8.8", "2620:fe::fe", "8.8.4.4"}, ":8.8.8.8:8.8.4.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, KernelIPDNSSuffix(tt.in))
		})
	}
}

// TestKernelDNSParamCarriesFullList pins the counterpart: the matchlock.dns=
// field (consumed by guest-init) is comma-delimited and carries every resolver,
// IPv4 and IPv6 alike, in the configured order.
func TestKernelDNSParamCarriesFullList(t *testing.T) {
	assert.Equal(t, "2606:4700:4700::1111,8.8.8.8", KernelDNSParam([]string{"2606:4700:4700::1111", "8.8.8.8"}))
}
