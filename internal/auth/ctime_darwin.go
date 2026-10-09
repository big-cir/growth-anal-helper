package auth

import "syscall"

func ctimeNs(st *syscall.Stat_t) int64 { return st.Ctimespec.Nano() }
