//go:build acceptance

package acceptance

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIPull(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", "alpine:latest")
	require.Equalf(t, 0, exitCode, "stdout: %s", stdout)
	assert.Contains(t, stdout, "Digest:")
}

func TestCLIPullMissingImage(t *testing.T) {
	_, _, exitCode := runCLI(t, "pull")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for missing image arg")
}

func TestCLIBuildMissingContext(t *testing.T) {
	_, _, exitCode := runCLI(t, "build")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for missing context arg")
}

func TestCLIDockerfileBuild(t *testing.T) {
	for _, name := range []string{"context-dockerfile", "external-dockerfile"} {
		t.Run(name, func(t *testing.T) {
			contextDir := t.TempDir()
			dockerfile := filepath.Join(contextDir, "Dockerfile")
			content := strings.Repeat("hello from streamed context\n", 10000)
			require.NoError(t, os.WriteFile(filepath.Join(contextDir, "hello.txt"), []byte(content), 0644))
			require.NoError(t, os.Symlink("hello.txt", filepath.Join(contextDir, "link.txt")))
			require.NoError(t, os.WriteFile(filepath.Join(contextDir, "ignored.txt"), []byte("do not upload"), 0644))
			require.NoError(t, os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("ignored.txt\n"), 0644))

			if name == "external-dockerfile" {
				dockerfileDir := filepath.Join(t.TempDir(), "external dockerfile")
				require.NoError(t, os.MkdirAll(dockerfileDir, 0755))
				dockerfile = filepath.Join(dockerfileDir, "Dockerfile.custom")
				// The selected external Dockerfile must win over a same-named context file.
				require.NoError(t, os.WriteFile(filepath.Join(contextDir, "Dockerfile.custom"), []byte("INVALID dockerfile\n"), 0644))
				// Dockerfile-specific ignores take precedence over the context .dockerignore.
				require.NoError(t, os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("hello.txt\n"), 0644))
				require.NoError(t, os.WriteFile(dockerfile+".dockerignore", []byte("ignored.txt\nDockerfile.custom\n"), 0644))
				require.NoError(t, os.WriteFile(filepath.Join(dockerfileDir, "host-only.txt"), []byte("not part of the context"), 0644))
			}
			require.NoError(t, os.WriteFile(dockerfile, []byte(`FROM busybox:latest
COPY . /context
RUN test -f /context/hello.txt && test -L /context/link.txt && test ! -e /context/ignored.txt && test ! -e /context/host-only.txt && test ! -e /context/Dockerfile.custom
`), 0644))

			tag := "matchlock-test-build-" + name + ":latest"
			t.Cleanup(func() { runCLI(t, "image", "rm", tag) })
			args := []string{
				"build", "--build-cpus", "1", "--build-memory", "1024", "--build-disk", "2048",
				"-f", dockerfile, "-t", tag,
			}
			if name == "external-dockerfile" {
				args = append(args, "--no-cache")
			}
			args = append(args, contextDir)
			stdout, stderr, exitCode := runCLIWithTimeout(t, 10*time.Minute, args...)
			require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
			assert.Contains(t, stdout, "Successfully built and tagged")

			imgStdout, _, imgExitCode := runCLI(t, "image", "ls")
			require.Equal(t, 0, imgExitCode)
			assert.Contains(t, imgStdout, tag)

			runStdout, runStderr, runExitCode := runCLIWithTimeout(t, 2*time.Minute,
				"run", "--image", tag, "--no-network", "--", "sh", "-c",
				"test ! -e /opt/matchlock/guest-fused && ! grep -q ' fuse' /proc/mounts && cat /context/link.txt",
			)
			require.Equalf(t, 0, runExitCode, "stdout: %s\nstderr: %s", runStdout, runStderr)
			assert.Equal(t, content, runStdout)
		})
	}
}

func TestCLIImageLsShowsHeader(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "image", "ls")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "TAG")
	assert.Contains(t, stdout, "SOURCE")
}

func TestCLIImageRmNonExistent(t *testing.T) {
	_, _, exitCode := runCLI(t, "image", "rm", "nonexistent:tag")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for non-existent image")
}

func TestCLIImageRmNoArgs(t *testing.T) {
	_, _, exitCode := runCLI(t, "image", "rm")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no tag provided")
}

func TestCLIImagePullAndRm(t *testing.T) {
	const img = "alpine:latest"

	// Pull the image (ensures it's in the registry cache)
	_, _, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", img)
	require.Equal(t, 0, exitCode)

	// Verify it appears in image ls
	stdout, _, exitCode := runCLI(t, "image", "ls")
	require.Equal(t, 0, exitCode)
	require.Containsf(t, stdout, img, "image ls should contain %q", img)

	// Remove it
	stdout, _, exitCode = runCLI(t, "image", "rm", img)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Removed")

	// Verify it's gone from image ls
	stdout, _, exitCode = runCLI(t, "image", "ls")
	require.Equal(t, 0, exitCode)
	assert.NotContainsf(t, stdout, img, "image ls should not contain %q after rm", img)
}

func TestCLIImageRmIdempotent(t *testing.T) {
	const img = "alpine:latest"

	// Pull, then remove twice - second remove should fail
	runCLIWithTimeout(t, 5*time.Minute, "pull", img)

	_, _, exitCode := runCLI(t, "image", "rm", img)
	require.Equal(t, 0, exitCode)

	_, _, exitCode = runCLI(t, "image", "rm", img)
	assert.NotEqual(t, 0, exitCode, "second image rm should fail for already-removed image")
}

func TestCLIImageGC(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "image", "gc")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Removed")
}

func TestImageBlobUsageTagAliasDoesNotDuplicateBlobs(t *testing.T) {
	const (
		baseTag  = "alpine:latest"
		aliasTag = "alpine:regression-alias"
	)

	_, stderr, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", "--force", baseTag)
	require.Equalf(t, 0, exitCode, "pull failed: %s", stderr)

	before, err := totalBlobUsageBytes()
	require.NoError(t, err)

	_, stderr, exitCode = runCLIWithTimeout(t, 5*time.Minute, "pull", "-t", aliasTag, baseTag)
	require.Equalf(t, 0, exitCode, "tag alias pull failed: %s", stderr)
	defer runCLI(t, "image", "rm", aliasTag)

	after, err := totalBlobUsageBytes()
	require.NoError(t, err)
	assert.Equal(t, before, after, "tag alias should not duplicate layer blobs")
}

func TestImageBlobUsageRepeatForcePullStableUsage(t *testing.T) {
	const tag = "alpine:latest"

	_, stderr, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", "--force", tag)
	require.Equalf(t, 0, exitCode, "first force pull failed: %s", stderr)
	firstUsage, err := blobUsageForTag("registry", tag)
	require.NoError(t, err)

	_, stderr, exitCode = runCLIWithTimeout(t, 5*time.Minute, "pull", "--force", tag)
	require.Equalf(t, 0, exitCode, "second force pull failed: %s", stderr)
	secondUsage, err := blobUsageForTag("registry", tag)
	require.NoError(t, err)

	assert.Equal(t, firstUsage, secondUsage, "repeat force pull should keep referenced blob usage stable")
}

func totalBlobUsageBytes() (int64, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	blobsDir := filepath.Join(home, ".cache", "matchlock", "images", "blobs")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return 0, err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
			total += st.Blocks * 512
		} else {
			total += fi.Size()
		}
	}
	return total, nil
}

func blobUsageForTag(scope, tag string) (int64, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	cacheRoot := filepath.Join(home, ".cache", "matchlock", "images")
	dbPath := filepath.Join(cacheRoot, "metadata.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT digest, COALESCE(fs_type, 'erofs')
		   FROM image_layers
		  WHERE scope = ? AND tag = ?
		  ORDER BY ordinal ASC`,
		scope,
		tag,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total int64
	for rows.Next() {
		var digest, fsType string
		if err := rows.Scan(&digest, &fsType); err != nil {
			return 0, err
		}
		name := strings.ReplaceAll(strings.ReplaceAll(digest, ":", "_"), "/", "_") + "." + fsType
		fi, err := os.Stat(filepath.Join(cacheRoot, "blobs", name))
		if err != nil {
			return 0, err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
			total += st.Blocks * 512
		} else {
			total += fi.Size()
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return total, nil
}
