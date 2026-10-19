//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris)

package auth

import "context"

func lockAuthFileOS(context.Context, string) (func(), error) {
	return func() {}, nil
}
