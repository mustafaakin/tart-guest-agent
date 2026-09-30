package vsock

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

type listener struct {
	socket *socket
	port   uint32
}

func Listen(port uint32) (net.Listener, error) {
	if err := winsockStartup(); err != nil {
		return nil, fmt.Errorf("failed to initialize Winsock: %w", err)
	}

	addressFamily, err := viosockAddressFamily()
	if err != nil {
		return nil, err
	}

	// Don't leak the socket into processes started by Exec, like CloseOnExec on Unix
	handle, err := windows.WSASocket(int32(addressFamily), windows.SOCK_STREAM, 0, nil, 0,
		windows.WSA_FLAG_NO_HANDLE_INHERIT)
	if err != nil {
		return nil, fmt.Errorf("failed to create an AF_VSOCK socket, is the viosock Winsock provider "+
			"registered (viosockwspsvc)? %w", err)
	}

	if err := bind(handle, &sockaddrVM{
		Family: addressFamily,
		Port:   port,
		CID:    vmaddrCIDAny,
	}); err != nil {
		_ = windows.Closesocket(handle)

		return nil, err
	}

	if err := windows.Listen(handle, windows.SOMAXCONN); err != nil {
		_ = windows.Closesocket(handle)

		return nil, err
	}

	return &listener{
		socket: newSocket(handle),
		port:   port,
	}, nil
}

func (listener *listener) Accept() (net.Conn, error) {
	var handle windows.Handle

	if err := listener.socket.do(func(listenerHandle windows.Handle) error {
		var err error

		handle, err = accept(listenerHandle)

		return err
	}); err != nil {
		// Report the canceled accept of a closed listener the way the net package does
		if errors.Is(err, windows.ERROR_OPERATION_ABORTED) || errors.Is(err, windows.WSAEINTR) {
			return nil, net.ErrClosed
		}

		return nil, err
	}

	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.Closesocket(handle)

		return nil, err
	}

	peerName, err := getpeername(handle)
	if err != nil {
		_ = windows.Closesocket(handle)

		return nil, fmt.Errorf("failed to get a peer name for an AF_VSOCK connection %w", err)
	}

	return &conn{
		socket:     newSocket(handle),
		localPort:  listener.port,
		remotePort: peerName.Port,
	}, nil
}

func (listener *listener) Addr() net.Addr {
	return &addr{port: listener.port}
}

func (listener *listener) Close() error {
	return listener.socket.close()
}
