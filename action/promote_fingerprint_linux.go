//go:build linux

package action

import "syscall"

func promotionCtimeNs(st *syscall.Stat_t) int64 { return st.Ctim.Nano() }
