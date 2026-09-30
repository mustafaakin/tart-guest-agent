package vsock

import (
	"net"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const cancelRetryInterval = 10 * time.Millisecond

// socket closes its handle only after in-flight calls return,
// since Windows may hand the same handle value to the next socket.
type socket struct {
	handle windows.Handle

	mu     sync.Mutex
	closed bool
	users  sync.WaitGroup
}

func newSocket(handle windows.Handle) *socket {
	return &socket{handle: handle}
}

func (socket *socket) do(call func(handle windows.Handle) error) error {
	socket.mu.Lock()
	if socket.closed {
		socket.mu.Unlock()

		return net.ErrClosed
	}
	socket.users.Add(1)
	socket.mu.Unlock()

	defer socket.users.Done()

	return call(socket.handle)
}

func (socket *socket) close() error {
	socket.mu.Lock()
	if socket.closed {
		socket.mu.Unlock()

		return net.ErrClosed
	}
	socket.closed = true
	socket.mu.Unlock()

	idle := make(chan struct{})
	go func() {
		socket.users.Wait()
		close(idle)
	}()

	// Blocking Winsock calls only return once their I/O is canceled, and a call that is
	// about to start its I/O may miss a single cancellation, so cancel until all return
	for {
		_ = windows.CancelIoEx(socket.handle, nil)

		select {
		case <-idle:
			return windows.Closesocket(socket.handle)
		case <-time.After(cancelRetryInterval):
		}
	}
}
