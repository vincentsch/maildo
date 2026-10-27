//go:build windows

package auth

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows file modes cannot make a file private, so this checks the file's
// access control list (DACL): only the current user, the system and
// administrators may have access.
func privateReadPermissions(file *os.File, _ os.FileInfo) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("credential owner is invalid")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("credential DACL is not private")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("credential DACL entry is unsupported")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask != 0 && !sid.Equals(user.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return fmt.Errorf("credential DACL is not private")
		}
	}
	return nil
}

func openRelativeRead(parent windows.Handle, name string, directory bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ|windows.READ_CONTROL|windows.SYNCHRONIZE, &attrs, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0)
	if code, ok := err.(windows.NTStatus); ok {
		err = code.Errno()
	}
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return 0, fmt.Errorf("managed path is a reparse point")
	}
	return handle, nil
}

func openManagedRead(root string, dirs []string, name string) (_ *managedRead, err error) {
	rootFile, err := os.Open(root) // The user-owned root may itself be an alias.
	if err != nil {
		return nil, err
	}
	parents := []windows.Handle{windows.Handle(rootFile.Fd())}
	closeParents := func() {
		for i := len(parents) - 1; i > 0; i-- {
			_ = windows.CloseHandle(parents[i])
		}
		_ = rootFile.Close()
	}
	defer func() {
		if err != nil {
			closeParents()
		}
	}()
	for _, dir := range dirs {
		handle, openErr := openRelativeRead(parents[len(parents)-1], dir, true)
		if openErr != nil {
			return nil, openErr
		}
		parents = append(parents, handle)
	}
	handle, err := openRelativeRead(parents[len(parents)-1], name, false)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), name)
	validate := func() error {
		for i, component := range append(append([]string(nil), dirs...), name) {
			entry, err := openRelativeRead(parents[i], component, i < len(dirs))
			if err != nil {
				return err
			}
			opened := handle
			if i < len(dirs) {
				opened = parents[i+1]
			}
			var a, b windows.ByHandleFileInformation
			err = windows.GetFileInformationByHandle(entry, &a)
			if err == nil {
				err = windows.GetFileInformationByHandle(opened, &b)
			}
			_ = windows.CloseHandle(entry)
			if err != nil {
				return err
			}
			if a.VolumeSerialNumber != b.VolumeSerialNumber || a.FileIndexHigh != b.FileIndexHigh || a.FileIndexLow != b.FileIndexLow {
				return fmt.Errorf("managed path changed")
			}
		}
		return nil
	}
	r := &managedRead{file: file, validatePath: validate, closeParents: closeParents}
	if err = r.validate(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return r, nil
}
