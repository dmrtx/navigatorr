package action

import "syscall"

func zeroCtime(st *syscall.Stat_t) { st.Ctimespec = syscall.Timespec{} }
