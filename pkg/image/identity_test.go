package image

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigDigestDeterministic(t *testing.T) {
	oci := &OCIConfig{
		User:       "root",
		WorkingDir: "/root",
		Entrypoint: []string{"/bin/zsh"},
		Cmd:        []string{"-l"},
		Env: map[string]string{
			"HOME": "/root",
			"PATH": "/usr/bin",
		},
	}
	a := configDigest(oci)
	b := configDigest(oci)
	assert.Equal(t, a, b, "identical config must produce the same digest")
	require.NotEmpty(t, a)
	assert.Equal(t, "sha256:", a[:7])

	// Order of map entries must not change the digest (encoding/json sorts map keys).
	oci.Env = map[string]string{
		"PATH": "/usr/bin",
		"HOME": "/root",
	}
	assert.Equal(t, a, configDigest(oci), "map key order must not change the digest")

	// A config change must change the digest.
	oci.User = "nobody"
	assert.NotEqual(t, a, configDigest(oci))
}

func TestConfigDigestNil(t *testing.T) {
	assert.Empty(t, configDigest(nil))
}

func TestIdentityFromResult(t *testing.T) {
	result := &BuildResult{
		Digest: "sha256:manifest",
		Size:   1234,
		OCI: &OCIConfig{
			User: "root",
			Cmd:  []string{"bash"},
		},
	}
	id := IdentityFromResult(result)
	require.NotNil(t, id)
	assert.Equal(t, "sha256:manifest", id.Digest)
	assert.NotEmpty(t, id.ConfigDigest)
	assert.Equal(t, int64(1234), id.Size)
	assert.Empty(t, id.Tag, "IdentityFromResult must not infer a tag")
}

func TestIdentityFromResultNilNoOCI(t *testing.T) {
	result := &BuildResult{Digest: "sha256:x"}
	id := IdentityFromResult(result)
	require.NotNil(t, id)
	assert.Equal(t, "sha256:x", id.Digest)
	assert.Empty(t, id.ConfigDigest, "no OCI config implies no config digest")
}

func TestStoreResolveIdentity(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer := writeTestLayer(t, cacheRoot, "sha256:abc", "rootfs")

	meta := ImageMeta{
		Digest:    "sha256:image",
		Source:    "import",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		OCI: &OCIConfig{
			User: "root",
			Env:  map[string]string{"HOME": "/root"},
		},
	}
	require.NoError(t, store.Save("bedlam:ubuntu", []LayerRef{layer}, meta))

	id, err := store.ResolveIdentity("bedlam:ubuntu")
	require.NoError(t, err)
	assert.Equal(t, "bedlam:ubuntu", id.Tag)
	assert.Equal(t, "sha256:image", id.Digest)
	assert.NotEmpty(t, id.ConfigDigest)
	assert.Equal(t, "import", id.Source)
	require.NotNil(t, id.OCI)
	assert.Equal(t, "root", id.OCI.User)
}

func TestStoreResolveIdentityNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.ResolveIdentity("does:not-exist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageNotFound))
}

func TestResolveEmptyTag(t *testing.T) {
	_, err := Resolve("")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageNotFound))
}

func TestIdentityVerifyNilExpectedIsLegacy(t *testing.T) {
	var expected *Identity
	require.NoError(t, expected.Verify(&Identity{Digest: "sha256:x"}))
}

func TestIdentityVerifyExactMatch(t *testing.T) {
	expected := &Identity{
		Tag:          "bedlam:ubuntu",
		Digest:       "sha256:image",
		ConfigDigest: "sha256:config",
	}
	actual := &Identity{
		Tag:          "bedlam:ubuntu",
		Digest:       "sha256:image",
		ConfigDigest: "sha256:config",
	}
	require.NoError(t, expected.Verify(actual))
}

func TestIdentityVerifyDigestMismatch(t *testing.T) {
	expected := &Identity{Digest: "sha256:expected"}
	actual := &Identity{Digest: "sha256:actual", ConfigDigest: "sha256:c"}
	err := expected.Verify(actual)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageIdentity))
	assert.Contains(t, err.Error(), `expected "sha256:expected"`)
}

func TestIdentityVerifyConfigDigestMismatch(t *testing.T) {
	expected := &Identity{Digest: "sha256:image", ConfigDigest: "sha256:expected-cfg"}
	actual := &Identity{Digest: "sha256:image", ConfigDigest: "sha256:actual-cfg"}
	err := expected.Verify(actual)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageIdentity))
	assert.Contains(t, err.Error(), "config_digest mismatch")
}

func TestIdentityVerifyEmptyExpectedFailsClosed(t *testing.T) {
	expected := &Identity{}
	actual := &Identity{Digest: "sha256:image"}
	err := expected.Verify(actual)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageIdentity))
}

func TestIdentityVerifyNilActualFailsClosed(t *testing.T) {
	expected := &Identity{Digest: "sha256:image"}
	err := expected.Verify(nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageIdentity))
}

func TestIdentityVerifyTagMismatch(t *testing.T) {
	expected := &Identity{Digest: "sha256:image", Tag: "a:latest"}
	actual := &Identity{Digest: "sha256:image", Tag: "b:latest"}
	err := expected.Verify(actual)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImageIdentity))
}

func TestIdentityVerifyPartialFields(t *testing.T) {
	// Only the digest is pinned; config digest is ignored when not expected.
	expected := &Identity{Digest: "sha256:image"}
	actual := &Identity{Digest: "sha256:image", ConfigDigest: "sha256:whatever"}
	require.NoError(t, expected.Verify(actual))

	// Only config digest is pinned.
	expected2 := &Identity{ConfigDigest: "sha256:cfg"}
	actual2 := &Identity{Digest: "sha256:image", ConfigDigest: "sha256:cfg"}
	require.NoError(t, expected2.Verify(actual2))
}

func TestIdentityRoundTripDoesNotPinPath(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer := writeTestLayer(t, cacheRoot, "sha256:abc", "rootfs")

	meta := ImageMeta{
		Digest:            "sha256:image",
		CreatedAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		OCI:               &OCIConfig{User: "root"},
		RuntimeRootfsPath: layer.Path,
	}
	require.NoError(t, store.Save("bedlam:ubuntu", []LayerRef{layer}, meta))

	id, err := store.ResolveIdentity("bedlam:ubuntu")
	require.NoError(t, err)
	// The identity must pin content/config digests, not the mutable runtime
	// blob path that lives on the host.
	assert.Equal(t, "bedlam:ubuntu", id.Tag)
	assert.Equal(t, "sha256:image", id.Digest)
	assert.NotEqual(t, layer.Path, id.Digest)
	assert.NotEmpty(t, id.ConfigDigest)
}
