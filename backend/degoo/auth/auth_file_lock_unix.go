//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func lockAuthFileOS(ctx context.Context, authFile string) (func(), error) {
	lockPath := authFile + ".lock"
	if _, err := checkPrivateParent(lockPath, true); err != nil {
		return nil, err
	}

	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_CREAT | unix.O_EXCL
	fd, err := unix.Open(lockPath, flags, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(lockPath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open private degoo auth lock: %w", err)
	}
	closeOnError := func(err error) (func(), error) {
		_ = unix.Close(fd)
		return nil, err
	}
	if created {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return closeOnError(fmt.Errorf("secure private degoo auth lock: %w", err))
		}
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return closeOnError(fmt.Errorf("inspect private degoo auth lock: %w", err))
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0o7777 != 0o600 || info.Nlink != 1 || info.Uid != uint32(unix.Geteuid()) {
		return closeOnError(errors.New("degoo auth lock must be a private owner-only regular file"))
	}
	for {
		if err := ctx.Err(); err != nil {
			return closeOnError(err)
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			break
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return closeOnError(err)
		}
	}
	if err != nil {
		return closeOnError(fmt.Errorf("lock degoo auth state: %w", err))
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}, nil
}
