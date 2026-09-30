package vdagent

import (
	"bytes"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// serialPortPath is where virtio-win's vioserial driver exposes the SPICE agent port,
// the same path spice-vdagent-win opens.
const serialPortPath = `\\.\Global\com.redhat.spice.0`

// openSerialPort opens the port for overlapped I/O, which lets os.File
// use the runtime poller and so support read deadlines.
func openSerialPort() (*os.File, error) {
	path, err := windows.UTF16PtrFromString(serialPortPath)
	if err != nil {
		return nil, err
	}

	handle, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s, is the virtio-win vioserial driver installed? %w",
			serialPortPath, err)
	}

	return os.NewFile(uintptr(handle), serialPortPath), nil
}

// textToGuest converts clipboard text from the host to Windows line endings.
func textToGuest(text []byte) []byte {
	// Don't double existing CRLFs
	text = bytes.ReplaceAll(text, []byte("\r\n"), []byte("\n"))

	return bytes.ReplaceAll(text, []byte("\n"), []byte("\r\n"))
}

// textFromGuest converts clipboard text from Windows to the host's line endings.
func textFromGuest(text []byte) []byte {
	return bytes.ReplaceAll(text, []byte("\r\n"), []byte("\n"))
}
