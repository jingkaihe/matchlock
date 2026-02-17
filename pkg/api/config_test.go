package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComposeCommand_NilImageConfig(t *testing.T) {
	var ic *ImageConfig
	got := ic.ComposeCommand([]string{"echo", "hello"})
	assert.Equal(t, []string{"echo", "hello"}, got)
}

func TestComposeCommand_EntrypointOnly(t *testing.T) {
	ic := &ImageConfig{Entrypoint: []string{"python3"}}
	got := ic.ComposeCommand(nil)
	assert.Equal(t, []string{"python3"}, got)
}

func TestComposeCommand_CmdOnly(t *testing.T) {
	ic := &ImageConfig{Cmd: []string{"sh"}}
	got := ic.ComposeCommand(nil)
	assert.Equal(t, []string{"sh"}, got)
}

func TestComposeCommand_EntrypointAndCmd(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}
	got := ic.ComposeCommand(nil)
	assert.Equal(t, []string{"python3", "-c", "print('hi')"}, got)
}

func TestComposeCommand_UserArgsReplaceCmd(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}
	got := ic.ComposeCommand([]string{"script.py"})
	assert.Equal(t, []string{"python3", "script.py"}, got)
}

func TestComposeCommand_UserArgsNoCmdNoEntrypoint(t *testing.T) {
	ic := &ImageConfig{}
	got := ic.ComposeCommand([]string{"echo", "hello"})
	assert.Equal(t, []string{"echo", "hello"}, got)
}

func TestComposeCommand_EmptyEntrypointAndCmd(t *testing.T) {
	ic := &ImageConfig{}
	got := ic.ComposeCommand(nil)
	assert.Nil(t, got)
}

func TestComposeCommand_NoMutation(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}

	_ = ic.ComposeCommand([]string{"script.py"})
	_ = ic.ComposeCommand([]string{"other.py"})

	assert.Equal(t, []string{"python3"}, ic.Entrypoint)
	assert.Equal(t, []string{"-c", "print('hi')"}, ic.Cmd)
}

func TestComposeCommand_RepeatedCallsConsistent(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"app.py"},
	}

	for i := 0; i < 10; i++ {
		got := ic.ComposeCommand(nil)
		assert.Equal(t, []string{"python3", "app.py"}, got)
	}
}

func TestGetHostname_UsesConfiguredNetworkHostname(t *testing.T) {
	cfg := &Config{
		Network: &NetworkConfig{Hostname: "override.internal"},
	}

	assert.Equal(t, "override.internal", cfg.GetHostname())
}

func TestGetHostname_NilNetworkFallsBackToGeneratedID(t *testing.T) {
	cfg := &Config{}

	hostname := cfg.GetHostname()
	assert.Regexp(t, `^vm-[0-9a-f]{8}$`, hostname)
	assert.Equal(t, hostname, cfg.ID)
}
