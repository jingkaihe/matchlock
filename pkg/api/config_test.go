package api

import (
	"testing"
)

func TestComposeCommand_NilImageConfig(t *testing.T) {
	var ic *ImageConfig
	got := ic.ComposeCommand([]string{"echo", "hello"})
	assertSliceEqual(t, got, []string{"echo", "hello"})
}

func TestComposeCommand_EntrypointOnly(t *testing.T) {
	ic := &ImageConfig{Entrypoint: []string{"python3"}}
	got := ic.ComposeCommand(nil)
	assertSliceEqual(t, got, []string{"python3"})
}

func TestComposeCommand_CmdOnly(t *testing.T) {
	ic := &ImageConfig{Cmd: []string{"sh"}}
	got := ic.ComposeCommand(nil)
	assertSliceEqual(t, got, []string{"sh"})
}

func TestComposeCommand_EntrypointAndCmd(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}
	got := ic.ComposeCommand(nil)
	assertSliceEqual(t, got, []string{"python3", "-c", "print('hi')"})
}

func TestComposeCommand_UserArgsReplaceCmd(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}
	got := ic.ComposeCommand([]string{"script.py"})
	assertSliceEqual(t, got, []string{"python3", "script.py"})
}

func TestComposeCommand_UserArgsNoCmdNoEntrypoint(t *testing.T) {
	ic := &ImageConfig{}
	got := ic.ComposeCommand([]string{"echo", "hello"})
	assertSliceEqual(t, got, []string{"echo", "hello"})
}

func TestComposeCommand_EmptyEntrypointAndCmd(t *testing.T) {
	ic := &ImageConfig{}
	got := ic.ComposeCommand(nil)
	assertSliceEqual(t, got, nil)
}

func TestComposeCommand_NoMutation(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}

	_ = ic.ComposeCommand([]string{"script.py"})
	_ = ic.ComposeCommand([]string{"other.py"})

	assertSliceEqual(t, ic.Entrypoint, []string{"python3"})
	assertSliceEqual(t, ic.Cmd, []string{"-c", "print('hi')"})
}

func TestComposeCommand_RepeatedCallsConsistent(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"app.py"},
	}

	for i := 0; i < 10; i++ {
		got := ic.ComposeCommand(nil)
		assertSliceEqual(t, got, []string{"python3", "app.py"})
	}
}

func assertSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
