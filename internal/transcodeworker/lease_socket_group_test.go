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

	// A group the worker's user is not in: chown fails, nothing is left.
	if os.Getuid() != 0 {
		if other := foreignGroup(dir); other != "" {
			fw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: filepath.Join(dir, "f.sock"), BuildSocketGroup: other})
			if err := fw.StartBuildLeases(ctx); err == nil {
				t.Fatalf("group %s, which this user is not in, must be refused", other)
			}
			if _, err := os.Lstat(filepath.Join(dir, "f.sock")); err == nil {
				t.Fatal("no socket after a failed chown")
			}
		}
	}

	// A stale socket stays put when the group is refused.
	stale := filepath.Join(dir, "stale.sock")
	if l, err := net.Listen("unix", stale); err == nil {
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = l.Close()
	}
	sw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: stale, BuildSocketGroup: "no-such-group-navigatorr"})
	if err := sw.StartBuildLeases(ctx); err == nil {
		t.Fatal("an unknown group must be refused")
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Fatal("a refused group must not remove the old socket")
	}

	// Binding happens in a temporary directory; none is left behind.
	if m, _ := filepath.Glob(filepath.Join(dir, ".l*")); len(m) != 0 {
		t.Fatalf("temporary bind directories left: %v", m)
	}

	def := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "st"), BuildNode: "nodeA", BuildSocketGroup: g.Name})
	if err := def.StartBuildLeases(ctx); err == nil {
		t.Fatal("a group needs build_socket")
	}
	if _, err := os.Lstat(def.LeaseSocketPath()); err == nil {
		t.Fatal("no socket in the private default directory for a group")
	}
}

// foreignGroup names a local group this user is not in, other than dir's own
// group (a new file already has it, so a chown to it is a no-op), or "" when
// none is known.
func foreignGroup(dir string) string {
	mine, err := os.Getgroups()
	if err != nil {
		return ""
	}
	in := map[int]bool{os.Getgid(): true}
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			in[int(st.Gid)] = true
		}
	}
	for _, g := range mine {
		in[g] = true
	}
	for _, name := range []string{"wheel", "daemon", "nogroup", "nobody", "kmem", "sys"} {
		g, err := user.LookupGroup(name)
		if err != nil {
			continue
		}
		if id, err := strconv.Atoi(g.Gid); err == nil && !in[id] {
			return name
		}
	}
	return ""
}
