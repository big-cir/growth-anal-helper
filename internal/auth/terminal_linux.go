package auth

import "syscall"

const (
	ioctlGet = syscall.TCGETS
	ioctlSet = syscall.TCSETS
)
