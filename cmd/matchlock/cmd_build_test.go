package main

import (
	"strings"
	"testing"
)

func TestBuildScriptSnapshotterAndDockerfileDir(t *testing.T) {
	got := buildScript("overlayfs", "/workspace/dockerfile", "", "")
	for _, want := range []string{
		"--oci-worker-snapshotter overlayfs",
		"--local dockerfile=/workspace/dockerfile",
		"set +e\nbuildctl",       // RC must be captured after the command
		"if [ $RC -ne 0 ]; then", // failure path dumps buildkitd log and exits RC
		"== buildkitd log ===",
		"exit $RC",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("buildScript missing %q\nscript:\n%s", want, got)
		}
	}
}

func TestBuildScriptNoCacheEmitted(t *testing.T) {
	got := buildScript("native", "/workspace/dockerfile", "", "  --no-cache \\\n")
	if !strings.Contains(got, "  --no-cache \\\n") {
		t.Errorf("buildScript should emit the --no-cache continuation when noCacheOpt set\nscript:\n%s", got)
	}
	// And it must still be a well-formed continuation feeding buildctl.
	if !strings.Contains(got, "--no-cache \\\n  --output") {
		t.Errorf("buildScript --no-cache continuation must lead into --output\nscript:\n%s", got)
	}
}

func TestBuildScriptNoCacheAbsentByDefault(t *testing.T) {
	got := buildScript("native", "/workspace/dockerfile", "", "")
	if strings.Contains(got, "--no-cache") {
		t.Errorf("buildScript must NOT emit --no-cache when noCacheOpt empty\nscript:\n%s", got)
	}
}

func TestBuildScriptFilenameOpt(t *testing.T) {
	got := buildScript("native", "/workspace/dockerfile", "  --opt filename=Dockerfile.dev \\\n", "")
	if !strings.Contains(got, "  --opt filename=Dockerfile.dev \\\n") {
		t.Errorf("buildScript should emit the filename opt continuation when set\nscript:\n%s", got)
	}
}

// Guard the build defaults so the plain `matchlock build -t <tag> .` works for
// heavy images without hand-tuned flags: overlayfs snapshotter + a large cache
// disk (avoids the native-snapshotter ENOSPC at the ext4 reserved-block floor).
func TestBuildDefaultsForHeavyImages(t *testing.T) {
	if defaultBuildSnapshotter != "overlayfs" {
		t.Errorf("default --build-snapshotter = %q, want overlayfs (native is space-heavy and ENOSPCs on big images)", defaultBuildSnapshotter)
	}
	if defaultBuildCacheSize < 40960 {
		t.Errorf("default --build-cache-size = %d MB, want >= 40960 (bedlam-size images need a large cache disk)", defaultBuildCacheSize)
	}
	if defaultBuildDisk < 20480 {
		t.Errorf("default --build-disk = %d MB, want >= 20480", defaultBuildDisk)
	}
}

func TestBuildTimeoutSecondsMapsZeroToSentinel(t *testing.T) {
	if got := buildTimeoutSeconds(0); got != 86400 {
		t.Errorf("buildTimeoutSeconds(0) = %d, want 86400 (no-timeout sentinel)", got)
	}
	if got := buildTimeoutSeconds(3600); got != 3600 {
		t.Errorf("buildTimeoutSeconds(3600) = %d, want 3600", got)
	}
}
