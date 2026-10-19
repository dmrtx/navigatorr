package action

import "syscall"

func zeroCtime(st *syscall.Stat_t) { st.Ctim = syscall.Timespec{} }
