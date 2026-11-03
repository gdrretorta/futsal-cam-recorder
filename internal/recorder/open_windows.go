package recorder

import (
	"os"

	"golang.org/x/sys/windows"
)

// openShared opens a file for reading without getting in the way of anyone
// else: ffmpeg keeps writing it, and finalize can still rename over it or
// delete it (FILE_SHARE_DELETE, which os.Open doesn't ask for).
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
