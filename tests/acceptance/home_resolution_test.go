//go:build acceptance

package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// homeResolutionExecTimeout bounds an ordinary guest exec call so a wedged
	// VM fails fast instead of hanging the whole acceptance run.
	homeResolutionExecTimeout = 30 * time.Second
	// homeResolutionZshExecTimeout bounds the interactive zsh call. Under the
	// QEMU TCG fallback the system-wide /etc/zsh/zshrc compinit sweep can take
	// well over 30s, so the interactive step gets a generous budget.
	homeResolutionZshExecTimeout = 180 * time.Second
)

func execHomeResolution(t *testing.T, client *sdk.Client, command string) *sdk.ExecResult {
	t.Helper()
	return execHomeResolutionTimeout(t, client, command, homeResolutionExecTimeout)
}

func execHomeResolutionTimeout(t *testing.T, client *sdk.Client, command string, timeout time.Duration) *sdk.ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	result, err := client.Exec(ctx, command)
	require.NoErrorf(t, err, "Exec %q", command)
	return result
}

// TestHomeResolutionDefaultRoot locks the runc/docker behavior for the default
// root user: HOME, USER and SHELL come from the passwd entry for uid 0 even
// though the PID 1 guest-agent environment only provides HOME=/.
func TestHomeResolutionDefaultRoot(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("igorhvr/bedlam-ubuntu"))

	result := execHomeResolution(t, client, `echo "HOME=$HOME"; echo "USER=$USER"; echo "SHELL=$SHELL"`)
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 3, "stdout: %q", result.Stdout)
	assert.Equal(t, "HOME=/root", lines[0], "bedlam root home")
	assert.Equal(t, "USER=root", lines[1], "bedlam root user")
	assert.Equal(t, "SHELL=/usr/bin/zsh", lines[2], "bedlam root shell")
}

// TestHomeResolutionEnvOverride verifies an explicit request/image environment
// value (-e HOME=... via builder.WithEnv) wins over the passwd-derived HOME.
func TestHomeResolutionEnvOverride(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("igorhvr/bedlam-ubuntu").WithEnv("HOME", "/tmp/x"))

	result := execHomeResolution(t, client, "echo $HOME")
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	assert.Equal(t, "/tmp/x", strings.TrimSpace(result.Stdout))
}

// TestHomeResolutionImageEnv verifies an OCI image ENV HOME wins over the
// passwd-derived HOME, matching docker/runc image-config precedence.
func TestHomeResolutionImageEnv(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("igorhvr/bedlam-ubuntu").
		WithImageConfig(&sdk.ImageConfig{Env: map[string]string{"HOME": "/custom-home"}}))

	result := execHomeResolution(t, client, "echo $HOME")
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	assert.Equal(t, "/custom-home", strings.TrimSpace(result.Stdout))
}

// TestHomeResolutionUserPath verifies the MATCHLOCK_USER privilege-drop path is
// unchanged: --user nobody still switches to uid 65534 and exposes a HOME.
func TestHomeResolutionUserPath(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("alpine:latest").WithUser("nobody"))

	result := execHomeResolution(t, client, "id -u; echo HOME=$HOME")
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 2, "stdout: %q", result.Stdout)
	assert.Equal(t, "65534", lines[0], "nobody uid")
	home := strings.TrimPrefix(lines[1], "HOME=")
	assert.NotEmpty(t, home, "HOME should be set for nobody")
}

// TestHomeResolutionCdAndZshRc verifies `cd` with no argument lands in the
// passwd home and an interactive zsh sources /root/.zshrc from that home.
func TestHomeResolutionCdAndZshRc(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("igorhvr/bedlam-ubuntu"))

	write := execHomeResolution(t, client, `printf 'export PROMPT_SOURCED=yes\n' > /root/.zshrc`)
	require.Equal(t, 0, write.ExitCode, "write .zshrc stderr: %s", write.Stderr)

	cdResult := execHomeResolution(t, client, "cd && pwd")
	require.Equal(t, 0, cdResult.ExitCode, "stderr: %s", cdResult.Stderr)
	assert.Equal(t, "/root", strings.TrimSpace(cdResult.Stdout))

	// stdin is redirected from /dev/null: an interactive zsh otherwise waits on
	// the exec pipe (which never reaches EOF) even though -c was supplied.
	zshResult := execHomeResolutionTimeout(t, client, `zsh -ic 'echo $HOME; echo $PROMPT_SOURCED' </dev/null`, homeResolutionZshExecTimeout)
	require.Equal(t, 0, zshResult.ExitCode, "stderr: %s", zshResult.Stderr)
	assert.Contains(t, zshResult.Stdout, "/root", "zsh should see HOME=/root")
	assert.Contains(t, zshResult.Stdout, "yes", "interactive zsh should source /root/.zshrc")
}
