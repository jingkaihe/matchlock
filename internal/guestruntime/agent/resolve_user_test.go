//go:build linux

package guestagent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPasswd = `root:x:0:0:root:/root:/bin/bash
# comment line
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
testuser:x:1000:1000:Test User:/home/testuser:/bin/sh
noshell:x:1001:1001:No Shell:/home/noshell
`

const testGroup = `root:x:0:
daemon:x:1:
# comment
nogroup:x:65534:
testgroup:x:1000:testuser
docker:x:999:testuser
`

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLookupPasswdByName(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)

	tests := []struct {
		name    string
		wantUID int
		wantGID int
		wantDir string
		wantOK  bool
	}{
		{"root", 0, 0, "/root", true},
		{"nobody", 65534, 65534, "/nonexistent", true},
		{"testuser", 1000, 1000, "/home/testuser", true},
		{"nonexistent", 0, 0, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid, dir, ok := lookupPasswdByNameFrom(tt.name, passwd)
			require.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantUID, uid)
			assert.Equal(t, tt.wantGID, gid)
			assert.Equal(t, tt.wantDir, dir)
		})
	}
}

func TestLookupPasswdByName_SkipsComments(t *testing.T) {
	passwd := writeTempFile(t, "passwd", "# root:x:0:0:root:/root:/bin/bash\n")
	_, _, _, ok := lookupPasswdByNameFrom("root", passwd)
	assert.False(t, ok, "should not match commented-out line")
}

func TestLookupPasswdByName_MissingFile(t *testing.T) {
	_, _, _, ok := lookupPasswdByNameFrom("root", "/nonexistent/passwd")
	assert.False(t, ok, "should return false for missing file")
}

func TestLookupPasswdByUID(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)

	tests := []struct {
		uid     int
		wantGID int
		wantDir string
		wantSh  string
	}{
		{0, 0, "/root", "/bin/bash"},
		{65534, 65534, "/nonexistent", "/usr/sbin/nologin"},
		{1000, 1000, "/home/testuser", "/bin/sh"},
		{1001, 1001, "/home/noshell", ""},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			gid, shell, dir := lookupPasswdByUIDFrom(tt.uid, passwd)
			assert.Equal(t, tt.wantGID, gid)
			assert.Equal(t, tt.wantDir, dir)
			assert.Equal(t, tt.wantSh, shell)
		})
	}
}

func TestLookupPasswdByUID_NotFound_DefaultsGIDToUID(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	gid, shell, dir := lookupPasswdByUIDFrom(9999, passwd)
	assert.Equal(t, 9999, gid, "gid should default to uid 9999")
	assert.Empty(t, shell)
	assert.Empty(t, dir)
}

func TestLookupPasswdByUID_SkipsComments(t *testing.T) {
	passwd := writeTempFile(t, "passwd", "# commented:x:1000:1000:Commented:/home/c:/bin/sh\nreal:x:1000:1000:Real:/home/real:/bin/bash\n")
	gid, shell, dir := lookupPasswdByUIDFrom(1000, passwd)
	assert.Equal(t, "/home/real", dir, "should skip commented line")
	assert.Equal(t, 1000, gid)
	assert.Equal(t, "/bin/bash", shell)
}

func TestResolveGID_Numeric(t *testing.T) {
	group := writeTempFile(t, "group", testGroup)
	gid, err := resolveGIDFrom("42", group)
	require.NoError(t, err)
	assert.Equal(t, 42, gid)
}

func TestResolveGID_ByName(t *testing.T) {
	group := writeTempFile(t, "group", testGroup)

	tests := []struct {
		name    string
		wantGID int
		wantErr bool
	}{
		{"root", 0, false},
		{"nogroup", 65534, false},
		{"docker", 999, false},
		{"nonexistent", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gid, err := resolveGIDFrom(tt.name, group)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantGID, gid)
			}
		})
	}
}

func TestResolveGID_SkipsComments(t *testing.T) {
	group := writeTempFile(t, "group", testGroup)
	_, err := resolveGIDFrom("comment", group)
	assert.Error(t, err, "should not match comment line")
}

func TestResolveUser_ByUsername(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	uid, gid, dir, err := resolveUserFrom("testuser", passwd, group)
	require.NoError(t, err)
	assert.Equal(t, 1000, uid)
	assert.Equal(t, 1000, gid)
	assert.Equal(t, "/home/testuser", dir)
}

func TestResolveUser_ByNumericUID(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	uid, gid, dir, err := resolveUserFrom("1000", passwd, group)
	require.NoError(t, err)
	assert.Equal(t, 1000, uid)
	assert.Equal(t, 1000, gid)
	assert.Equal(t, "/home/testuser", dir)
}

func TestResolveUser_ByNumericUID_NotInPasswd(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	uid, gid, dir, err := resolveUserFrom("9999", passwd, group)
	require.NoError(t, err)
	assert.Equal(t, 9999, uid)
	assert.Equal(t, 9999, gid, "gid should default to uid")
	assert.Empty(t, dir)
}

func TestResolveUser_UIDColonGID_Numeric(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	uid, gid, _, err := resolveUserFrom("1000:65534", passwd, group)
	require.NoError(t, err)
	assert.Equal(t, 1000, uid)
	assert.Equal(t, 65534, gid)
}

func TestResolveUser_UIDColonGID_ByNames(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	uid, gid, _, err := resolveUserFrom("testuser:docker", passwd, group)
	require.NoError(t, err)
	assert.Equal(t, 1000, uid)
	assert.Equal(t, 999, gid)
}

func TestResolveUser_NotFound(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	_, _, _, err := resolveUserFrom("nonexistent", passwd, group)
	assert.Error(t, err, "should fail for unknown username")
}

func TestResolveUser_BadGIDInColonFormat(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	_, _, _, err := resolveUserFrom("1000:badgroup", passwd, group)
	assert.Error(t, err, "should fail for unknown group name")
}

func TestResolveUser_BadUIDInColonFormat(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	group := writeTempFile(t, "group", testGroup)

	_, _, _, err := resolveUserFrom("baduser:1000", passwd, group)
	assert.Error(t, err, "should fail for unknown user name in uid:gid format")
}
