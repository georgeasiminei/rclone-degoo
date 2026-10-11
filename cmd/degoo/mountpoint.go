//go:build linux

package degoo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type mountpointGuard struct {
	file      *os.File
	mode      os.FileMode
	immutable bool
}

const fsImmutableFlag = 0x10 // FS_IMMUTABLE_FL from linux/fs.h.

const immutableMountHelper = "/usr/local/libexec/rclone-degoo/fusermount3"

func protectMountpoint(path string) (*mountpointGuard, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, mountpointSetupError(path, err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			return nil, mountpointSetupError(path, err)
		}
		return nil, fmt.Errorf("open mount directory for protection: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*mountpointGuard, error) {
		_ = file.Close()
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fail(fmt.Errorf("inspect mount directory: %w", err))
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fail(mountpointSetupError(path, errors.New("mount directory must be owned by the current user")))
	}
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return fail(fmt.Errorf("inspect mount directory filesystem: %w", err))
	}
	if filesystem.Type == unix.FUSE_SUPER_MAGIC {
		return fail(errors.New("mount directory is already on a FUSE filesystem; refusing to change its permissions"))
	}
	guard := &mountpointGuard{file: file, mode: os.FileMode(stat.Mode & 0777)}
	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err == nil {
		guard.immutable = flags&fsImmutableFlag != 0
	}
	if err := guard.protect(); err != nil {
		return fail(err)
	}
	return guard, nil
}

func mountpointSetupError(path string, err error) error {
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		return fmt.Errorf("prepare mount directory: %w", err)
	}
	quoted := "'" + strings.ReplaceAll(absolute, "'", "'\"'\"'") + "'"
	return fmt.Errorf("prepare mount directory: %w\nCreate a private mount directory and protect it while unmounted:\nsudo mkdir -p -- %s\nsudo chown -- %d:%d %s\nsudo chmod 700 -- %s\nsudo chattr +i -- %s", err, quoted, os.Getuid(), os.Getgid(), quoted, quoted, quoted)
}

func (guard *mountpointGuard) prepare() error {
	names, err := guard.file.Readdirnames(1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("inspect unmounted directory: %w", err)
	}
	if len(names) != 0 {
		return errors.New("mount directory contains ordinary local files that are not queued for upload; copy them to a safe location, remove them from this directory, then mount and copy them back")
	}
	if guard.immutable {
		return nil
	}
	return guard.file.Chmod(guard.mode | 0200)
}

func validateImmutableMountHelper(path string) error {
	current := filepath.Clean(path)
	for {
		var stat unix.Stat_t
		if err := unix.Lstat(current, &stat); err != nil {
			return err
		}
		if stat.Uid != 0 || stat.Mode&unix.S_IFMT == unix.S_IFLNK || stat.Mode&0022 != 0 {
			return fmt.Errorf("%s must be root-owned without symlinks or group/other write access", current)
		}
		if current == path {
			if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&unix.S_ISUID == 0 || stat.Mode&0111 != 0111 {
				return errors.New("immutable mount helper must be a regular setuid executable")
			}
		} else if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("%s is not a directory", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func (guard *mountpointGuard) useImmutableMountHelper() (func(), error) {
	if !guard.immutable {
		return func() {}, nil
	}
	path, err := filepath.Abs(guard.file.Name())
	if err != nil {
		return nil, err
	}
	if filepath.Clean(path) != "/mnt/degoo" {
		return nil, errors.New("immutable mounting supports /mnt/degoo; migrate the contained install with degoo-rc install --mountpoint /mnt/degoo while preserving its cache")
	}
	if err := validateImmutableMountHelper(immutableMountHelper); err != nil {
		return nil, fmt.Errorf("immutable mountpoint needs the administrator-installed helper at %s: %w; run the installed degoo-rc mount-setup command with sudo, then run degoo-rc service enable as your normal user with the immutable flag retained", immutableMountHelper, err)
	}
	oldPath, present := os.LookupEnv("PATH")
	if err := os.Setenv("PATH", filepath.Dir(immutableMountHelper)+":"+oldPath); err != nil {
		return nil, err
	}
	return func() {
		if present {
			_ = os.Setenv("PATH", oldPath)
		} else {
			_ = os.Unsetenv("PATH")
		}
	}, nil
}

func (guard *mountpointGuard) protect() error {
	// The descriptor refers to the underlying directory even after FUSE covers it.
	if guard.immutable {
		return nil
	}
	if err := guard.file.Chmod(guard.mode &^ 0222); err != nil {
		return fmt.Errorf("protect unmounted directory against local writes: %w", err)
	}
	return nil
}

func (guard *mountpointGuard) Close() error {
	err := guard.protect()
	return errors.Join(err, guard.file.Close())
}
