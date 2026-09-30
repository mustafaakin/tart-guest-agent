//go:build !windows

package vdagent

import "os"

const serialPortPath = "/dev/tty.com.redhat.spice.0"

func openSerialPort() (*os.File, error) {
	return os.OpenFile(serialPortPath, os.O_RDWR, 0)
}

func textToGuest(text []byte) []byte {
	return text
}

func textFromGuest(text []byte) []byte {
	return text
}
