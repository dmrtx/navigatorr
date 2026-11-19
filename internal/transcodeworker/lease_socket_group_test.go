package transcodeworker

import (
	"context"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// build_socket_group opens the socket to one group (0660, that group) in a
// directory the operator chose; without it the socket stays owner-only. A
// group the worker cannot use, or a group without build_socket, starts no
// socket at all.
func TestLeaseSocketGroup(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nbg")
	if err != nil {
		t.Skip("no /tmp")
	}
	defer os.RemoveAll(dir)
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skipf("no name for gid %d: %v", os.Getgid(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(dir, "g.sock")
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: sock, BuildSocketGroup: g.Name})
	if err := w.StartBuildLeases(ctx); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket opened to the group: %v %v", fi, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || strconv.Itoa(int(st.Gid)) != g.Gid {
		t.Fatalf("socket group: %v, want %s", fi.Sys(), g.Gid)
	}
	if c, err := net.DialTimeout("unix", sock, time.Second); err != nil {
		t.Fatal(err)
	} else {
		_ = c.Close()
	}

	bad := filepath.Join(dir, "bad.sock")
	nw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: bad, BuildSocketGroup: "no-such-group-navigatorr"})
	if err := nw.StartBuildLeases(ctx); err == nil {
		t.Fatal("an unknown group must be refused")
	}
	if _, err := os.Lstat(bad); err == nil {
		t.Fatal("no socket for an unknown group")
	}

	def := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "st"), BuildNode: "nodeA", BuildSocketGroup: g.Name})
	if err := def.StartBuildLeases(ctx); err == nil {
		t.Fatal("a group needs build_socket")
	}
	if _, err := os.Lstat(def.LeaseSocketPath()); err == nil {
		t.Fatal("no socket in the private default directory for a group")
	}
}
