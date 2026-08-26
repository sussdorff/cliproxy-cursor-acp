//go:build darwin || linux

package cursor

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openQuotaCredential opens the managed profile component by component. Each
// descriptor is checked after opening, so a later pathname replacement cannot
// redirect a quota request to credentials from another profile.
func openQuotaCredential(target QuotaTarget) (*os.File, error) {
	profilesRoot, profileName, err := quotaProfileTarget(target)
	if err != nil {
		return nil, quotaUnavailable()
	}
	rootFD, err := openPrivateDirectory(profilesRoot)
	if err != nil {
		return nil, quotaUnavailable()
	}
	defer unix.Close(rootFD)
	profileFD, err := openPrivateDirectoryAt(rootFD, profileName)
	if err != nil {
		return nil, quotaUnavailable()
	}
	defer unix.Close(profileFD)
	cursorFD, err := openPrivateDirectoryAt(profileFD, "cursor")
	if err != nil {
		return nil, quotaUnavailable()
	}
	defer unix.Close(cursorFD)
	authFD, err := unix.Openat(cursorFD, "auth.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil || !privateRegularFile(authFD) {
		if err == nil {
			_ = unix.Close(authFD)
		}
		return nil, quotaUnavailable()
	}
	credential := os.NewFile(uintptr(authFD), "cursor-auth.json")
	if credential == nil {
		_ = unix.Close(authFD)
		return nil, quotaUnavailable()
	}
	return credential, nil
}

func quotaProfileTarget(target QuotaTarget) (string, string, error) {
	profile := filepath.Clean(strings.TrimSpace(target.ProfileDir))
	profilesRoot, err := secureDirectory(strings.TrimSpace(target.ProfilesRoot))
	if err != nil || !filepath.IsAbs(profile) {
		return "", "", quotaUnavailable()
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(profile))
	if err != nil || parent != profilesRoot {
		return "", "", quotaUnavailable()
	}
	profileName := filepath.Base(profile)
	if profileName == "." || profileName == string(filepath.Separator) {
		return "", "", quotaUnavailable()
	}
	return profilesRoot, profileName, nil
}

func openPrivateDirectory(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil || !privateDirectory(fd) {
		if err == nil {
			_ = unix.Close(fd)
		}
		return -1, quotaUnavailable()
	}
	return fd, nil
}

func openPrivateDirectoryAt(parent int, name string) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil || !privateDirectory(fd) {
		if err == nil {
			_ = unix.Close(fd)
		}
		return -1, quotaUnavailable()
	}
	return fd, nil
}

func privateDirectory(fd int) bool {
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && privateModeAndOwner(stat)
}

func privateRegularFile(fd int) bool {
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && privateModeAndOwner(stat)
}

func privateModeAndOwner(stat unix.Stat_t) bool {
	return stat.Mode&0o077 == 0 && int(stat.Uid) == os.Geteuid()
}
