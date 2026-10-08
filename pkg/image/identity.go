package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Identity is the stable content + relevant OCI configuration identity of a
// locally resolved image. It is produced without booting a VM, pulling the
// image from a registry, or changing the selected local tag. It intentionally
// does NOT include a mutable host pathname (e.g. a blob/RootfsPath), because a
// pathname is not a stable identity.
type Identity struct {
	// Tag is the image reference as stored (e.g. "igorhvr/bedlam-ubuntu").
	Tag string `json:"tag"`
	// Digest is the OCI content digest (the manifest/image digest) for the image.
	Digest string `json:"digest"`
	// ConfigDigest is a deterministic fingerprint of the relevant OCI
	// configuration (user, working_dir, entrypoint, cmd, env). It pins the
	// configuration semantics that a manifest digest alone can omit when the
	// runtime resolves a squashed rootfs from a different layer set.
	ConfigDigest string `json:"config_digest,omitempty"`
	// Source is the store scope/import source ("local", "registry", "import", ...).
	Source string `json:"source,omitempty"`
	// Size is the total layer size in bytes.
	Size int64 `json:"size,omitempty"`
	// OCI is the relevant OCI configuration summary, when present.
	OCI *OCIConfig `json:"oci,omitempty"`
}

// Resolve returns the stable identity of the image referenced by tag from the
// local image store (falling back to the registry cache). It never pulls
// remotely and never mutates the selected local tag. A missing/unknown image
// returns an error wrapping ErrImageNotFound.
func Resolve(tag string) (*Identity, error) {
	if tag == "" {
		return nil, errx.With(ErrImageNotFound, ": empty tag")
	}

	store := NewStore("")
	if id, err := store.ResolveIdentity(tag); err == nil {
		return id, nil
	}

	if id, err := resolveRegistryCacheIdentity(tag); err == nil {
		return id, nil
	}

	return nil, errx.With(ErrImageNotFound, ": %q not found in local store or registry cache", tag)
}

// ResolveIdentity resolves a tag from the local store only.
func (s *Store) ResolveIdentity(tag string) (*Identity, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	result, err := s.Get(tag)
	if err != nil {
		return nil, err
	}
	id := IdentityFromResult(result)
	id.Tag = tag
	return id, nil
}

func resolveRegistryCacheIdentity(tag string) (*Identity, error) {
	result, err := GetRegistryCache(tag, "")
	if err != nil {
		return nil, err
	}
	id := IdentityFromResult(result)
	if id.Source == "" {
		id.Source = "registry"
	}
	id.Tag = tag
	return id, nil
}

// IdentityFromResult derives a stable identity from a BuildResult. It only
// depends on fields already carried by the store's image metadata (content
// digest + relevant OCI configuration), never on a mutable pathname.
func IdentityFromResult(result *BuildResult) *Identity {
	if result == nil {
		return nil
	}
	id := &Identity{
		Digest: result.Digest,
		Size:   result.Size,
		Source: result.Source,
		OCI:    result.OCI,
	}
	if result.OCI != nil {
		id.ConfigDigest = configDigest(result.OCI)
	}
	return id
}

// configDigest returns a deterministic fingerprint of the relevant OCI
// configuration. It uses Go's canonical struct JSON ordering (and map key
// sorting for the Env map), so equal configurations always yield the same
// digest. An empty configuration yields an empty digest.
func configDigest(oci *OCIConfig) string {
	if oci == nil {
		return ""
	}
	data, err := json.Marshal(oci)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Verify reports whether the actual resolved identity satisfies the pinned
// expected identity. A nil expected identity indicates legacy behavior (no
// pin) and returns nil. A non-nil identity with no digest and no config digest
// is rejected (fail closed) because it pins nothing yet explicitly requests a
// check. Any non-empty expected field that does not match the actual identity
// returns a descriptive error.
func (id *Identity) Verify(actual *Identity) error {
	if id == nil {
		return nil
	}
	if id.Digest == "" && id.ConfigDigest == "" {
		return errx.With(ErrImageIdentity, ": expected identity pins nothing (set digest and/or config_digest)")
	}
	if actual == nil {
		return errx.With(ErrImageIdentity, ": actual image identity is empty")
	}
	if id.Digest != "" && actual.Digest != id.Digest {
		return errx.With(ErrImageIdentity, ": digest mismatch: expected %q, actual %q", id.Digest, actual.Digest)
	}
	if id.ConfigDigest != "" && actual.ConfigDigest != id.ConfigDigest {
		return errx.With(ErrImageIdentity, ": config_digest mismatch: expected %q, actual %q", id.ConfigDigest, actual.ConfigDigest)
	}
	if id.Tag != "" && actual.Tag != "" && id.Tag != actual.Tag {
		return errx.With(ErrImageIdentity, ": tag mismatch: expected %q, actual %q", id.Tag, actual.Tag)
	}
	return nil
}

// IsIdentityMismatch reports whether err is an identity verification failure.
func IsIdentityMismatch(err error) bool {
	return errors.Is(err, ErrImageIdentity)
}
