//go:build acceptance

package acceptance

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageRegression_TagAliasDoesNotDuplicateBlobs(t *testing.T) {
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

func TestStorageRegression_RepeatForcePullStableUsage(t *testing.T) {
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

func TestStartupLatencyHarness(t *testing.T) {
	if os.Getenv("MATCHLOCK_RUN_REGRESSION") != "1" {
		t.Skip("set MATCHLOCK_RUN_REGRESSION=1 to run startup latency regression harness")
	}

	const iterations = 3
	var total time.Duration
	for i := 0; i < iterations; i++ {
		start := time.Now()
		stdout, stderr, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "--", "true")
		require.Equalf(t, 0, exitCode, "run failed (iter=%d): stdout=%s stderr=%s", i, stdout, stderr)
		total += time.Since(start)
	}
	avg := total / iterations
	t.Logf("startup latency avg=%s over %d runs", avg, iterations)

	if maxMsStr := strings.TrimSpace(os.Getenv("MATCHLOCK_MAX_STARTUP_MS")); maxMsStr != "" {
		maxMs, err := strconv.Atoi(maxMsStr)
		require.NoError(t, err, "invalid MATCHLOCK_MAX_STARTUP_MS")
		assert.LessOrEqual(t, avg.Milliseconds(), int64(maxMs), "startup latency regression threshold exceeded")
	}
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
