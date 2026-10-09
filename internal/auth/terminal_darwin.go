package auth

import "syscall"

const (
	ioctlGet = syscall.TIOCGETA
	ioctlSet = syscall.TIOCSETA
)
