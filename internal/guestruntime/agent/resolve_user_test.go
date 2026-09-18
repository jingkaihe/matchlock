//go:build linux

package guestagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestLookupPasswdEntryByName(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)

	tests := []struct {
		name   string
		want   passwdEntry
		wantOK bool
	}{
		{
			name:   "root",
			want:   passwdEntry{Name: "root", UID: 0, GID: 0, Home: "/root", Shell: "/bin/bash"},
			wantOK: true,
		},
		{
			name:   "testuser",
			want:   passwdEntry{Name: "testuser", UID: 1000, GID: 1000, Home: "/home/testuser", Shell: "/bin/sh"},
			wantOK: true,
		},
		{
			name:   "noshell",
			want:   passwdEntry{Name: "noshell", UID: 1001, GID: 1001, Home: "/home/noshell", Shell: ""},
			wantOK: true,
		},
		{name: "nonexistent", want: passwdEntry{}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lookupPasswdEntryByNameFrom(tt.name, passwd)
			require.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLookupPasswdEntryByName_SkipsCommentsAndBlankLines(t *testing.T) {
	passwd := writeTempFile(t, "passwd", "\n# root:x:0:0:root:/root:/bin/bash\n   \nreal:x:42:43:Real:/home/real:/bin/zsh\n")

	_, ok := lookupPasswdEntryByNameFrom("root", passwd)
	require.False(t, ok, "commented root line must not match")

	got, ok := lookupPasswdEntryByNameFrom("real", passwd)
	require.True(t, ok)
	assert.Equal(t, passwdEntry{Name: "real", UID: 42, GID: 43, Home: "/home/real", Shell: "/bin/zsh"}, got)
}

func TestLookupPasswdEntryByName_MissingFile(t *testing.T) {
	got, ok := lookupPasswdEntryByNameFrom("root", "/nonexistent/passwd")
	assert.False(t, ok)
	assert.Equal(t, passwdEntry{}, got)
}

func TestLookupPasswdEntryByUID(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)

	tests := []struct {
		name   string
		uid    int
		want   passwdEntry
		wantOK bool
	}{
		{name: "root", uid: 0, want: passwdEntry{Name: "root", UID: 0, GID: 0, Home: "/root", Shell: "/bin/bash"}, wantOK: true},
		{name: "named_user", uid: 1000, want: passwdEntry{Name: "testuser", UID: 1000, GID: 1000, Home: "/home/testuser", Shell: "/bin/sh"}, wantOK: true},
		{name: "empty_shell", uid: 1001, want: passwdEntry{Name: "noshell", UID: 1001, GID: 1001, Home: "/home/noshell", Shell: ""}, wantOK: true},
		{name: "unknown_uid", uid: 9999, want: passwdEntry{}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lookupPasswdEntryByUIDFrom(tt.uid, passwd)
			require.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLookupPasswdEntryByUID_SkipsCommentsAndBlankLines(t *testing.T) {
	passwd := writeTempFile(t, "passwd", "\n# fake:x:1000:1000:Fake:/home/fake:/bin/false\n\nreal:x:1000:1000:Real:/home/real:/bin/bash\n")

	got, ok := lookupPasswdEntryByUIDFrom(1000, passwd)
	require.True(t, ok)
	assert.Equal(t, "real", got.Name, "should skip commented and blank lines")
	assert.Equal(t, 1000, got.GID)
	assert.Equal(t, "/home/real", got.Home)
	assert.Equal(t, "/bin/bash", got.Shell)
}

func TestLookupPasswdEntryByUID_MissingFile(t *testing.T) {
	got, ok := lookupPasswdEntryByUIDFrom(0, "/nonexistent/passwd")
	assert.False(t, ok)
	assert.Equal(t, passwdEntry{}, got)
}

func TestLookupPasswdWrappersMatchEntryLookups(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)

	nameEntry, ok := lookupPasswdEntryByNameFrom("testuser", passwd)
	require.True(t, ok)
	uid, gid, home, ok := lookupPasswdByNameFrom("testuser", passwd)
	require.True(t, ok)
	assert.Equal(t, nameEntry.UID, uid)
	assert.Equal(t, nameEntry.GID, gid)
	assert.Equal(t, nameEntry.Home, home)

	uidEntry, ok := lookupPasswdEntryByUIDFrom(1000, passwd)
	require.True(t, ok)
	gid, shell, home := lookupPasswdByUIDFrom(1000, passwd)
	assert.Equal(t, uidEntry.GID, gid)
	assert.Equal(t, uidEntry.Shell, shell)
	assert.Equal(t, uidEntry.Home, home)
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

func TestResolveUserEnvDefaultsFrom(t *testing.T) {
	passwd := writeTempFile(t, "passwd", testPasswd)
	missing := filepath.Join(t.TempDir(), "no-such-passwd")

	tests := []struct {
		name       string
		user       string
		passwdPath string
		want       map[string]string
	}{
		{
			name:       "empty user means root",
			user:       "",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/root", "USER": "root", "LOGNAME": "root", "SHELL": "/bin/bash"},
		},
		{
			name:       "root by name",
			user:       "root",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/root", "USER": "root", "LOGNAME": "root", "SHELL": "/bin/bash"},
		},
		{
			name:       "named user",
			user:       "testuser",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/home/testuser", "USER": "testuser", "LOGNAME": "testuser", "SHELL": "/bin/sh"},
		},
		{
			name:       "numeric uid",
			user:       "1000",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/home/testuser", "USER": "testuser", "LOGNAME": "testuser", "SHELL": "/bin/sh"},
		},
		{
			name:       "uid colon gid uses uid part",
			user:       "1000:65534",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/home/testuser", "USER": "testuser", "LOGNAME": "testuser", "SHELL": "/bin/sh"},
		},
		{
			name:       "unknown uid yields only slash home",
			user:       "9999",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/"},
		},
		{
			name:       "unknown name yields only slash home",
			user:       "ghost",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/"},
		},
		{
			name:       "empty shell field omits shell",
			user:       "1001",
			passwdPath: passwd,
			want:       map[string]string{"HOME": "/home/noshell", "USER": "noshell", "LOGNAME": "noshell"},
		},
		{
			name:       "missing passwd file yields slash home for root",
			user:       "",
			passwdPath: missing,
			want:       map[string]string{"HOME": "/"},
		},
		{
			name:       "missing passwd file yields slash home for named user",
			user:       "root",
			passwdPath: missing,
			want:       map[string]string{"HOME": "/"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveUserEnvDefaultsFrom(tt.user, tt.passwdPath)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveUserEnvDefaultsFrom_EmptyPasswdHomeFallsBackToSlash(t *testing.T) {
	passwd := writeTempFile(t, "passwd", "emptyhome:x:1002:1002:Empty Home::/bin/sh\n")
	got := resolveUserEnvDefaultsFrom("1002", passwd)
	assert.Equal(t, "/", got["HOME"], "empty passwd home must fall back to /")
	assert.Equal(t, "emptyhome", got["USER"])
	assert.Equal(t, "emptyhome", got["LOGNAME"])
	assert.Equal(t, "/bin/sh", got["SHELL"])
}

// lastEnvValue returns the effective value for key in an env slice, i.e. the
// value of its last occurrence (exec semantics).
func lastEnvValue(env []string, key string) (string, bool) {
	prefix := key + "="
	value := ""
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = strings.TrimPrefix(kv, prefix)
			found = true
		}
	}
	return value, found
}

func TestMergeExecEnv(t *testing.T) {
	t.Run("defaults override the kernel HOME in base", func(t *testing.T) {
		base := []string{"PATH=/usr/bin", "HOME=/"}
		defaults := map[string]string{"HOME": "/root", "USER": "root", "LOGNAME": "root", "SHELL": "/bin/bash"}

		env := mergeExecEnv(base, defaults, nil)

		home, ok := lastEnvValue(env, "HOME")
		require.True(t, ok)
		assert.Equal(t, "/root", home, "passwd HOME must override kernel HOME=/")
		user, ok := lastEnvValue(env, "USER")
		require.True(t, ok)
		assert.Equal(t, "root", user)
		path, ok := lastEnvValue(env, "PATH")
		require.True(t, ok)
		assert.Equal(t, "/usr/bin", path, "unrelated base entries are preserved")
	})

	t.Run("request env wins over defaults", func(t *testing.T) {
		base := []string{"HOME=/"}
		defaults := map[string]string{"HOME": "/root", "USER": "root", "LOGNAME": "root", "SHELL": "/bin/bash"}
		request := map[string]string{"HOME": "/tmp/x"}

		env := mergeExecEnv(base, defaults, request)

		home, ok := lastEnvValue(env, "HOME")
		require.True(t, ok)
		assert.Equal(t, "/tmp/x", home, "request HOME must win over the passwd default")
		// The passwd default HOME must not be appended when the request defines it.
		count := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, "HOME=") {
				count++
			}
		}
		assert.Equal(t, 2, count, "only base HOME and request HOME remain")
	})

	t.Run("request env wins for USER and SHELL", func(t *testing.T) {
		defaults := map[string]string{"HOME": "/root", "USER": "root", "LOGNAME": "root", "SHELL": "/bin/bash"}
		request := map[string]string{"USER": "custom", "SHELL": "/bin/zsh"}

		env := mergeExecEnv(nil, defaults, request)

		user, ok := lastEnvValue(env, "USER")
		require.True(t, ok)
		assert.Equal(t, "custom", user)
		shell, ok := lastEnvValue(env, "SHELL")
		require.True(t, ok)
		assert.Equal(t, "/bin/zsh", shell)
		logname, ok := lastEnvValue(env, "LOGNAME")
		require.True(t, ok)
		assert.Equal(t, "root", logname)
		home, ok := lastEnvValue(env, "HOME")
		require.True(t, ok)
		assert.Equal(t, "/root", home)
	})

	t.Run("request env is always appended", func(t *testing.T) {
		env := mergeExecEnv([]string{"A=1"}, map[string]string{"A": "default"}, map[string]string{"A": "request", "B": "2"})
		a, ok := lastEnvValue(env, "A")
		require.True(t, ok)
		assert.Equal(t, "request", a)
		b, ok := lastEnvValue(env, "B")
		require.True(t, ok)
		assert.Equal(t, "2", b)
	})
}

func TestApplyUserEnv(t *testing.T) {
	t.Run("request HOME wins and defaults are merged", func(t *testing.T) {
		cmd := exec.Command("true")
		applyUserEnv(cmd, "", map[string]string{"HOME": "/tmp/x"})

		home, ok := lastEnvValue(cmd.Env, "HOME")
		require.True(t, ok)
		assert.Equal(t, "/tmp/x", home)
		// HOME always has a passwd-derived default, so a home value exists even
		// when the request does not define one.
		cmd2 := exec.Command("true")
		applyUserEnv(cmd2, "", nil)
		home, ok = lastEnvValue(cmd2.Env, "HOME")
		require.True(t, ok)
		assert.NotEmpty(t, home)
	})

	t.Run("MATCHLOCK_USER is exported only for a requested user", func(t *testing.T) {
		cmd := exec.Command("true")
		applyUserEnv(cmd, "1000:1000", nil)
		user, ok := lastEnvValue(cmd.Env, "MATCHLOCK_USER")
		require.True(t, ok)
		assert.Equal(t, "1000:1000", user)

		cmd = exec.Command("true")
		applyUserEnv(cmd, "", nil)
		_, ok = lastEnvValue(cmd.Env, "MATCHLOCK_USER")
		assert.False(t, ok, "MATCHLOCK_USER must not be exported without a requested user")
	})
}
