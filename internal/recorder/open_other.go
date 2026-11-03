//go:build !windows

package recorder

import "os"

// openShared opens a file for reading. Other systems never lock open files.
func openShared(path string) (*os.File, error) { return os.Open(path) }
