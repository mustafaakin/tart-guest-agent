package rpc

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// checkExecutable requires an .exe, since Windows runs batch files through cmd.exe,
// which would reinterpret the arguments, and checks that the agent may execute it.
func checkExecutable(path string) error {
	if !strings.EqualFold(filepath.Ext(path), ".exe") {
		return errors.New("must be an .exe file on Windows")
	}

	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}

	handle, err := windows.CreateFile(pathUTF16, windows.FILE_EXECUTE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}

	return windows.CloseHandle(handle)
}
