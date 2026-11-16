package service

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// LockDir lets only SYSTEM and the Administrators into dir, the install
// folder: it holds config.json (camera password, Resend key), session.key
// (anyone who can read it can forge a login) and the exe the service runs as
// SYSTEM. A new folder on C:\ lets every Windows account in, so any program
// on the PC could read or replace them.
//
// keepOpen, when not empty, is a folder inside dir (the recordings) that
// keeps the access Windows gives new folders (signed-in users can read, copy
// and delete), so videos can still be taken off the PC by hand.
//
// It does nothing once dir is locked (protected DACL), so it runs at every
// start. It returns whether it changed anything.
//
// shortcut: keepOpen is only set the first time; a recordings folder moved
// inside dir later stays admin-only. Fix by hand if that ever matters.
func LockDir(dir, keepOpen string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	if ctl, _, err := sd.Control(); err == nil && ctl&windows.SE_DACL_PROTECTED != 0 {
		return false, nil
	}
	if keepOpen != "" {
		// First, so the recordings never lose access, even if what follows fails.
		if err := os.MkdirAll(keepOpen, 0o755); err != nil {
			return false, err
		}
		// 0x1301bf = Modify. Added to what the folder inherits.
		if err := setDACL(keepOpen, "D:(A;OICI;0x1301bf;;;AU)", windows.UNPROTECTED_DACL_SECURITY_INFORMATION); err != nil {
			return false, fmt.Errorf("%s: %w", keepOpen, err)
		}
	}
	// FA = full control, OICI = also for every file and folder inside.
	if err := setDACL(dir, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", windows.PROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		return false, fmt.Errorf("%s: %w", dir, err)
	}
	return true, nil
}

func setDACL(path, sddl string, flags windows.SECURITY_INFORMATION) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|flags, nil, nil, dacl, nil)
}
