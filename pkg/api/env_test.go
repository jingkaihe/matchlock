package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEnvVarInlineValue(t *testing.T) {
	name, value, err := ParseEnvVar("FOO=bar")
	require.NoError(t, err)
	assert.Equal(t, "FOO", name)
	assert.Equal(t, "bar", value)
}

func TestParseEnvVarInlineEmptyValue(t *testing.T) {
	name, value, err := ParseEnvVar("EMPTY=")
	require.NoError(t, err)
	assert.Equal(t, "EMPTY", name)
	assert.Equal(t, "", value)
}

func TestParseEnvVarInlineValueWithEquals(t *testing.T) {
	name, value, err := ParseEnvVar("TOKEN=a=b=c")
	require.NoError(t, err)
	assert.Equal(t, "TOKEN", name)
	assert.Equal(t, "a=b=c", value)
}

func TestParseEnvVarFromHostEnv(t *testing.T) {
	t.Setenv("FROM_HOST", "host-value")
	name, value, err := ParseEnvVar("FROM_HOST")
	require.NoError(t, err)
	assert.Equal(t, "FROM_HOST", name)
	assert.Equal(t, "host-value", value)
}

func TestParseEnvVarFromHostEnvMissing(t *testing.T) {
	const key = "MATCHLOCK_TEST_ENV_NOT_SET_123"
	_ = os.Unsetenv(key)

	_, _, err := ParseEnvVar(key)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrEnvVarNotSet)
}

func TestParseEnvVarEmptyName(t *testing.T) {
	_, _, err := ParseEnvVar("=value")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrEnvNameEmpty)
}

func TestParseEnvFileParsesLinesAndIgnoresComments(t *testing.T) {
	t.Setenv("FROM_HOST_FILE", "from-host")

	dir := t.TempDir()
	path := filepath.Join(dir, "app.env")
	content := "# comment\n\nFOO=bar\nFROM_HOST_FILE\nQUX=quux\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	env, err := ParseEnvFile(path)
	require.NoError(t, err)

	assert.Equal(t, "bar", env["FOO"])
	assert.Equal(t, "from-host", env["FROM_HOST_FILE"])
	assert.Equal(t, "quux", env["QUX"])
}

func TestParseEnvFileReturnsLineNumberOnParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.env")
	require.NoError(t, os.WriteFile(path, []byte("GOOD=1\n=bad\n"), 0644))

	_, err := ParseEnvFile(path)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrEnvFileLine)
	assert.Contains(t, err.Error(), ":2:")
}

func TestParseEnvsPrecedence(t *testing.T) {
	t.Setenv("HOST_ONLY", "host-value")

	dir := t.TempDir()
	file1 := filepath.Join(dir, "one.env")
	file2 := filepath.Join(dir, "two.env")

	require.NoError(t, os.WriteFile(file1, []byte("A=1\nB=from-file1\n"), 0644))
	require.NoError(t, os.WriteFile(file2, []byte("B=from-file2\nC=3\n"), 0644))

	env, err := ParseEnvs(
		[]string{"C=from-flag", "D=4", "HOST_ONLY"},
		[]string{file1, file2},
	)
	require.NoError(t, err)

	assert.Equal(t, "1", env["A"])
	assert.Equal(t, "from-file2", env["B"])
	assert.Equal(t, "from-flag", env["C"])
	assert.Equal(t, "4", env["D"])
	assert.Equal(t, "host-value", env["HOST_ONLY"])
}

func TestParseEnvsNoInputReturnsNil(t *testing.T) {
	env, err := ParseEnvs(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, env)
}
