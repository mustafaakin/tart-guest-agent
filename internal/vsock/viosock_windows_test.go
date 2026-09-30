//nolint:testpackage
package vsock

import (
	"net"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestSockaddrVMMatchesViosockLayout(t *testing.T) {
	require.EqualValues(t, 12, unsafe.Sizeof(sockaddrVM{}))
	require.EqualValues(t, 4, unsafe.Offsetof(sockaddrVM{}.Port))
	require.EqualValues(t, 8, unsafe.Offsetof(sockaddrVM{}.CID))
}

func TestParseSockaddrVM(t *testing.T) {
	sa, err := parseSockaddrVM([]byte{40, 0, 0, 0, 0x90, 0x1f, 0, 0, 2, 0, 0, 0, 0xff})
	require.NoError(t, err)
	require.Equal(t, &sockaddrVM{Family: 40, Port: 8080, CID: 2}, sa)

	_, err = parseSockaddrVM(make([]byte, 11))
	require.Error(t, err)
}

func TestSocketCloseWaitsForUsers(t *testing.T) {
	socket := newSocket(0)
	socket.closed = true

	require.ErrorIs(t, socket.do(func(_ windows.Handle) error { return nil }), net.ErrClosed)
	require.ErrorIs(t, socket.close(), net.ErrClosed)
}
