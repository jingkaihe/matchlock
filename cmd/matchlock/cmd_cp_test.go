package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCp(t *testing.T) {
	vm := copyArg{vmID: "vm-1", path: "/w"}
	host := copyArg{path: "./out"}
	stdio := copyArg{path: "-"}
	tests := []struct {
		name      string
		src, dest copyArg
		flags     cpFlags
		wantErr   bool
	}{
		{name: "copy in", src: host, dest: vm, flags: cpFlags{owner: "app", exclude: []string{".env"}}},
		{name: "copy out with limits", src: vm, dest: host, flags: cpFlags{limitsSet: true}},
		{name: "tar out with exclude", src: vm, dest: stdio, flags: cpFlags{exclude: []string{".env"}}},
		{name: "between sandboxes", src: vm, dest: vm, wantErr: true},
		{name: "no sandbox", src: host, dest: host, wantErr: true},
		{name: "owner when copying out", src: vm, dest: host, flags: cpFlags{owner: "app"}, wantErr: true},
		{name: "exclude with tar from stdin", src: stdio, dest: vm, flags: cpFlags{exclude: []string{".env"}}, wantErr: true},
		{name: "follow-link with tar from stdin", src: stdio, dest: vm, flags: cpFlags{followLink: true}, wantErr: true},
		{name: "limits with tar to stdout", src: vm, dest: stdio, flags: cpFlags{limitsSet: true}, wantErr: true},
		{name: "limits when copying in", src: host, dest: vm, flags: cpFlags{limitsSet: true}, wantErr: true},
		{name: "relative sandbox path", src: host, dest: copyArg{vmID: "vm-1", path: "w"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCp(tt.src, tt.dest, tt.flags)
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrInvalidCopy)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseCopyArgsAndSpecs(t *testing.T) {
	for arg, want := range map[string]copyArg{
		"vm-abc:/workspace": {vmID: "vm-abc", path: "/workspace"},
		"./a:b":             {path: "./a:b"},
		"/abs/a:b":          {path: "/abs/a:b"},
		"-":                 {path: "-"},
		"plain":             {path: "plain"},
	} {
		assert.Equal(t, want, parseCopyArg(arg), arg)
	}

	in, err := parseCopySpecs("copy-in", []string{"./a:b:/workspace"}, false)
	require.NoError(t, err)
	assert.Equal(t, []copySpec{{src: "./a:b", dest: "/workspace"}}, in)
	out, err := parseCopySpecs("copy-out", []string{"/workspace/dist:./out:x"}, true)
	require.NoError(t, err)
	assert.Equal(t, []copySpec{{src: "/workspace/dist", dest: "./out:x"}}, out)

	for value, guestFirst := range map[string]bool{"./a:relative": false, "./a": false, "relative:./out": true, "/x:": true} {
		_, err := parseCopySpecs("copy", []string{value}, guestFirst)
		assert.ErrorIs(t, err, ErrInvalidCopy, value)
	}
}
