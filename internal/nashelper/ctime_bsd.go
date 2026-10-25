//go:build !linux && !darwin

package nashelper

import "syscall"

func statCtimeNs(st *syscall.Stat_t) int64 { return 0 }
