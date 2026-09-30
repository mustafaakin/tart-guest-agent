//nolint:testpackage
package vdagent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClipboardLineEndings(t *testing.T) {
	require.Equal(t, []byte("a\r\nb\r\n\r\nc"), textToGuest([]byte("a\nb\r\n\nc")))
	require.Equal(t, []byte("a\nb\n\nc"), textFromGuest([]byte("a\r\nb\r\n\r\nc")))
	require.Equal(t, []byte("lone\rcr"), textFromGuest(textToGuest([]byte("lone\rcr"))))
}
