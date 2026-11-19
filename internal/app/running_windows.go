//go:build windows

package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// requireExecutableIdle refuses to continue while another process is running
// the executable about to be replaced. The Windows loader opens a running
// image with delete sharing, so the upgrade's rename succeeds and strands that
// process on the backup instead of replacing what it runs. The backup then
// cannot be removed until the process exits, and the next upgrade fails on the
// stale backup. Stopping before the build keeps the install out of that state.
//
// The backup is checked as well as the executable: someone who already
// upgraded over a running orr has the instance on "<executable>~", and the
// clear running-instance message beats a backup-removal failure after a build.
func requireExecutableIdle(executable string) error {
	pids, err := otherOrrProcesses(executable)
	if err != nil {
		return fmt.Errorf("check for running orr processes: %w", err)
	}
	if len(pids) == 0 {
		return nil
	}
	ids := make([]string, len(pids))
	for i, pid := range pids {
		ids[i] = strconv.FormatUint(uint64(pid), 10)
	}
	instance := "it"
	if len(pids) > 1 {
		instance = "them"
	}
	return fmt.Errorf("orr is still running (PID %s); close %s and run the upgrade again", strings.Join(ids, ", "), instance)
}

// otherOrrProcesses lists the processes, other than this one, that were
// started from this install's executable or its upgrade backup.
func otherOrrProcesses(executable string) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, err
	}
	self := windows.GetCurrentProcessId()
	identities := installIdentities(executable)
	var pids []uint32
	for {
		if entry.ProcessID != self && processImageMatches(entry.ProcessID, executable, identities) {
			pids = append(pids, entry.ProcessID)
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, err
		}
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	return pids, nil
}

// processImageMatches reports whether pid was started from this install. A
// process that cannot be queried — another user's, or one that exited between
// the snapshot and the query — is not counted: the instance that blocks an
// upgrade is the operator's own.
func processImageMatches(pid uint32, executable string, identities []fileIdentity) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return false
	}
	image := windows.UTF16ToString(buffer[:size])
	if matchesInstallImage(image, executable) {
		return true
	}
	// The image keeps the name it was started with, which can spell the same
	// file differently from this install's resolved path — an 8.3 short name
	// under a temp directory, a junction, a mapped drive. Identity is what the
	// comparison needs, and the path is only how the file is reached.
	identity, ok := fileIdentityOf(image)
	if !ok {
		return false
	}
	for _, candidate := range identities {
		if candidate == identity {
			return true
		}
	}
	return false
}

// fileIdentity names a file independently of how its path is spelled.
type fileIdentity struct {
	volume uint32
	index  uint64
}

// installIdentities resolves the executable and the backup an earlier upgrade
// may have left behind. A missing backup simply has no identity.
func installIdentities(executable string) []fileIdentity {
	var identities []fileIdentity
	for _, candidate := range []string{executable, executable + "~"} {
		if identity, ok := fileIdentityOf(candidate); ok {
			identities = append(identities, identity)
		}
	}
	return identities
}

func fileIdentityOf(path string) (fileIdentity, bool) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileIdentity{}, false
	}
	handle, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fileIdentity{}, false
	}
	defer windows.CloseHandle(handle)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fileIdentity{}, false
	}
	return fileIdentity{
		volume: info.VolumeSerialNumber,
		index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, true
}

// matchesInstallImage reports whether an image path belongs to this install:
// the executable itself, or the backup an earlier upgrade left behind.
func matchesInstallImage(path, executable string) bool {
	return sameExecutablePath(path, executable) ||
		sameExecutablePath(path, executable+"~")
}

// sameExecutablePath compares two image paths the way the Windows filesystem
// does, ignoring case, redundant separators, and the long-path prefix a
// process started through a long path carries into its image name.
func sameExecutablePath(left, right string) bool {
	return strings.EqualFold(normalizeWindowsPath(left), normalizeWindowsPath(right))
}

func normalizeWindowsPath(path string) string {
	if len(path) >= 4 && strings.EqualFold(path[:4], `\\?\`) {
		path = path[4:]
		if len(path) >= 4 && strings.EqualFold(path[:4], `UNC\`) {
			path = `\\` + path[4:]
		}
	}
	return filepath.Clean(path)
}
