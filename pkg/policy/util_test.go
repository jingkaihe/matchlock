package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		match   bool
	}{
		{"*", "anything", true},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "a.b.example.com", true}, // deep subdomains
		{"*.example.com", "example.com", false},
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "other.example.com", false},
		{"prefix.*", "prefix.com", true},
		{"prefix.*", "other.com", false},
		// Middle wildcard patterns
		{"api-*.example.com", "api-v1.example.com", true},
		{"api-*.example.com", "api-prod.example.com", true},
		{"api-*.example.com", "other.example.com", false},
		{"*-prod.example.com", "api-prod.example.com", true},
		{"*-prod.example.com", "api-dev.example.com", false},
		// Multiple wildcards
		{"*.*.example.com", "a.b.example.com", true},
		{"api-*-*.example.com", "api-v1-prod.example.com", true},
		// Edge cases
		{"", "", true},
		{"test", "test", true},
		{"test", "other", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.str, func(t *testing.T) {
			assert.Equal(t, tt.match, matchGlob(tt.pattern, tt.str))
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		host    string
		private bool
	}{
		{"192.168.1.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.private, isPrivateIP(tt.host))
		})
	}
}
