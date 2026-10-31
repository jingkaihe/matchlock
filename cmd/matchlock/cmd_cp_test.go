package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
