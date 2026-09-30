package vsock

import (
	"errors"
	"io"
	"math"
	"net"
	"time"

	"golang.org/x/sys/windows"
)

// conn uses blocking Winsock calls, which viosock's provider serves itself.
// The provider's handles don't work with Go's I/O completion port poller,
// so deadlines are unsupported and closing cancels blocked calls instead.
type conn struct {
	socket     *socket
	localPort  uint32
	remotePort uint32
}

func (conn *conn) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	var received uint32

	err := conn.socket.do(func(handle windows.Handle) error {
		wsaBuf := windows.WSABuf{Len: uint32(min(len(buf), math.MaxInt32)), Buf: &buf[0]}
		var flags uint32

		return windows.WSARecv(handle, &wsaBuf, 1, &received, &flags, nil, nil)
	})
	if err != nil {
		return 0, conn.error(err)
	}
	if received == 0 {
		return 0, io.EOF
	}

	return int(received), nil
}

func (conn *conn) Write(buf []byte) (int, error) {
	var written int

	for written < len(buf) {
		var sent uint32

		err := conn.socket.do(func(handle windows.Handle) error {
			chunk := buf[written:]
			wsaBuf := windows.WSABuf{Len: uint32(min(len(chunk), math.MaxInt32)), Buf: &chunk[0]}

			return windows.WSASend(handle, &wsaBuf, 1, &sent, 0, nil, nil)
		})
		if err != nil {
			return written, conn.error(err)
		}

		written += int(sent)
	}

	return written, nil
}

func (conn *conn) SetDeadline(_ time.Time) error {
	return nil
}

func (conn *conn) SetReadDeadline(_ time.Time) error {
	return nil
}

func (conn *conn) SetWriteDeadline(_ time.Time) error {
	return nil
}

func (conn *conn) LocalAddr() net.Addr {
	return &addr{port: conn.localPort}
}

func (conn *conn) RemoteAddr() net.Addr {
	return &addr{port: conn.remotePort}
}

func (conn *conn) Close() error {
	return conn.socket.close()
}

func (conn *conn) error(err error) error {
	// A call canceled by Close
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) || errors.Is(err, windows.WSAEINTR) {
		return net.ErrClosed
	}

	return err
}
