package image

import (
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

func fakeImage(t *testing.T, user, workdir string, entrypoint, cmd, env []string) v1.Image {
	t.Helper()
	base := empty.Image
	cfg, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Config.User = user
	cfg.Config.WorkingDir = workdir
	cfg.Config.Entrypoint = entrypoint
	cfg.Config.Cmd = cmd
	cfg.Config.Env = env
	img, err := mutate.ConfigFile(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestExtractOCIConfig_Normal(t *testing.T) {
	img := fakeImage(t, "nobody", "/app",
		[]string{"python3"}, []string{"app.py"},
		[]string{"PATH=/usr/bin", "FOO=bar=baz"})

	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig")
	}
	if oci.User != "nobody" {
		t.Errorf("User = %q, want %q", oci.User, "nobody")
	}
	if oci.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want %q", oci.WorkingDir, "/app")
	}
	assertStrSlice(t, "Entrypoint", oci.Entrypoint, []string{"python3"})
	assertStrSlice(t, "Cmd", oci.Cmd, []string{"app.py"})
	if oci.Env["PATH"] != "/usr/bin" {
		t.Errorf("Env[PATH] = %q, want %q", oci.Env["PATH"], "/usr/bin")
	}
	if oci.Env["FOO"] != "bar=baz" {
		t.Errorf("Env[FOO] = %q, want %q (should preserve = in value)", oci.Env["FOO"], "bar=baz")
	}
}

func TestExtractOCIConfig_EmptyConfig(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, nil)
	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig even for empty config")
	}
	if oci.User != "" {
		t.Errorf("User = %q, want empty", oci.User)
	}
	if len(oci.Entrypoint) != 0 {
		t.Errorf("Entrypoint = %v, want empty", oci.Entrypoint)
	}
	if len(oci.Cmd) != 0 {
		t.Errorf("Cmd = %v, want empty", oci.Cmd)
	}
}

func TestExtractOCIConfig_EnvWithoutEquals(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, []string{"NOEQUALS", "KEY=val"})
	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig")
	}
	if _, ok := oci.Env["NOEQUALS"]; ok {
		t.Error("env entry without '=' should be skipped")
	}
	if oci.Env["KEY"] != "val" {
		t.Errorf("Env[KEY] = %q, want %q", oci.Env["KEY"], "val")
	}
}

func TestExtractOCIConfig_EnvEmptyValue(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, []string{"EMPTY="})
	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig")
	}
	if v, ok := oci.Env["EMPTY"]; !ok || v != "" {
		t.Errorf("Env[EMPTY] = %q, ok=%v; want empty string, true", v, ok)
	}
}

func assertStrSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v (len %d), want %v (len %d)", name, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
