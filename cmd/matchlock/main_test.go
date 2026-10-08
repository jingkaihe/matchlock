package main

import (
	"errors"
	"os/exec"
	"testing"
)

// exitCodeForError must NOT silently swallow a wrapped subprocess failure that
// happens to expose ExitCode() (e.g. *exec.ExitError). If it did, a failing
// image-prep subprocess (mkfs.erofs, mke2fs, etc.) would exit with no
// diagnostics at all — exactly the silent-failure bug this guards against.
func TestExitCodeForErrorPrintsWrappedExitError(t *testing.T) {
	exitErr := &exec.ExitError{}
	// exec.ExitError implements ExitCode() int, so a naive errors.As on the
	// interface would match it. The helper must NOT — it returns default 1.
	code := exitCodeForError(exitErr)
	if code != 1 {
		t.Fatalf("wrapped *exec.ExitError: got exit code %d, want 1 (must not be silently swallowed)", code)
	}
}

func TestExitCodeForErrorPreservesIntentionalCode(t *testing.T) {
	code := exitCodeForError(&exitCodeError{code: 7})
	if code != 7 {
		t.Fatalf("*exitCodeError{7}: got %d, want 7", code)
	}
}

func TestExitCodeForErrorZeroIsNoop(t *testing.T) {
	if code := exitCodeForError(nil); code != 0 {
		t.Fatalf("nil: got %d, want 0", code)
	}
}

func TestExitCodeForErrorGeneric(t *testing.T) {
	if code := exitCodeForError(errors.New("boom")); code != 1 {
		t.Fatalf("generic error: got %d, want 1", code)
	}
}

func TestExitCodeForErrorWrappedExitCodeError(t *testing.T) {
	// An *exitCodeError wrapped by errors.Join/WithMessage must still be
	// recognized (errors.As unwraps).
	wrapped := errors.Join(&exitCodeError{code: 3}, errors.New("context"))
	if code := exitCodeForError(wrapped); code != 3 {
		t.Fatalf("wrapped *exitCodeError: got %d, want 3", code)
	}
}
