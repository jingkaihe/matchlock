//go:build linux

package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveArch(t *testing.T) {
	cases := []struct {
		in      string
		want    Arch
		wantErr bool
	}{
		{"arm64", ArchAarch64, false},
		{"aarch64", ArchAarch64, false},
		{"amd64", ArchX86_64, false},
		{"x86_64", ArchX86_64, false},
		{"riscv64", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := ResolveArch(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("ResolveArch(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.want {
			t.Fatalf("ResolveArch(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestProvenance(t *testing.T) {
	if PinnedVersion() != "v1.10.1" {
		t.Fatalf("PinnedVersion()=%q want v1.10.1", PinnedVersion())
	}
	wantCommit := "1fcdaec088da9e30c2c111f9feb2649118488361"
	if ReleaseCommit() != wantCommit {
		t.Fatalf("ReleaseCommit()=%q want %q", ReleaseCommit(), wantCommit)
	}
}

func TestVerifyArchive(t *testing.T) {
	for _, arch := range []Arch{ArchAarch64, ArchX86_64} {
		if err := VerifyArchive(arch); err != nil {
			t.Fatalf("VerifyArchive(%s) failed: %v", arch, err)
		}
	}
	if err := VerifyArchive(Arch("mips64")); err == nil {
		t.Fatal("VerifyArchive(mips64) should fail (unsupported arch)")
	}
}

func TestInstallBoth(t *testing.T) {
	for _, arch := range []Arch{ArchAarch64, ArchX86_64} {
		dir := t.TempDir()
		if err := InstallBoth(arch, dir); err != nil {
			t.Fatalf("InstallBoth(%s) failed: %v", arch, err)
		}
		for _, name := range []string{BinaryFirecracker, BinaryJailer} {
			p := filepath.Join(dir, name)
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if fi.Mode().Perm() != 0o755 {
				t.Fatalf("%s mode=%v want 0755", p, fi.Mode().Perm())
			}
			if fi.Size() == 0 {
				t.Fatalf("%s is empty", p)
			}
		}
	}
}

func TestInstallInvalidBinary(t *testing.T) {
	err := Install(ArchAarch64, "bogus", filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("expected error for invalid binary name")
	}
	if !errors.Is(err, ErrInvalidBinaryName) {
		t.Fatalf("got %v want ErrInvalidBinaryName", err)
	}
}

func TestInstallReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, BinaryFirecracker)
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(ArchAarch64, BinaryFirecracker, dst); err != nil {
		t.Fatalf("replace install failed: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 3 { // "old"
		t.Fatal("destination was not replaced")
	}
}

func TestInstallNoTempLeak(t *testing.T) {
	dir := t.TempDir()
	if err := InstallBoth(ArchAarch64, dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 installed binaries, found %d", len(entries))
	}
	for _, e := range entries {
		if e.Name() != BinaryFirecracker && e.Name() != BinaryJailer {
			t.Fatalf("unexpected temp/extra file: %s", e.Name())
		}
	}
}

func TestArchiveChecksumFailure(t *testing.T) {
	_, data, err := loadArchive(ArchAarch64)
	if err != nil {
		t.Fatalf("load embedded archive: %v", err)
	}
	// Tamper a copy (never the shared embedded store) and confirm the production
	// checksum gate rejects it with ErrChecksumMismatch.
	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-1] ^= 0xff
	if err := verifyChecksum(tampered, recordedSha(ArchAarch64)); err == nil {
		t.Fatal("verifyChecksum accepted a tampered archive")
	} else if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("got %v want ErrChecksumMismatch", err)
	}
	// The untampered data must still pass.
	if err := verifyChecksum(data, recordedSha(ArchAarch64)); err != nil {
		t.Fatalf("verifyChecksum rejected the untampered archive: %v", err)
	}
}

// recordedSha returns the manifest's recorded checksum for an arch.
func recordedSha(arch Arch) string {
	a, _, err := loadArchive(arch)
	if err != nil {
		return ""
	}
	return a.Sha256
}

func TestExtractMissingMember(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("hello")
	if err := tw.WriteHeader(&tar.Header{Name: "release/foo", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	err := extractMember(buf.Bytes(), "release/bar", dest)
	if err == nil {
		t.Fatal("expected error for missing member")
	}
	if !errors.Is(err, ErrMissingMember) {
		t.Fatalf("got %v want ErrMissingMember", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("destination should not exist after missing-member failure")
	}
}

func TestExtractMalformedGzip(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	err := extractMember([]byte("this is not gzip"), "release/foo", dest)
	if err == nil {
		t.Fatal("expected error for malformed gzip")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("destination should not exist after malformed-archive failure")
	}
}
