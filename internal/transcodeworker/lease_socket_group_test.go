package transcodeworker

import (
	"context"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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

	// staleSocket leaves a socket file nobody listens on, as a worker that died does.
	staleSocket := func(p string) {
		t.Helper()
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = l.Close()
	}

	// A group the worker's user is not in: chown fails, nothing new is left and
	// the old socket stays.
	if os.Getuid() == 0 {
		t.Log("root can chown to any group: the foreign-group case is skipped")
	} else if other := foreignGroup(dir); other == "" {
		t.Log("no local group this user is not in: the foreign-group case is skipped")
	} else {
		f := filepath.Join(dir, "f.sock")
		staleSocket(f)
		fw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: f, BuildSocketGroup: other})
		if err := fw.StartBuildLeases(ctx); err == nil {
			t.Fatalf("group %s, which this user is not in, must be refused", other)
		}
		if fi, err := os.Lstat(f); err != nil || fi.Mode()&os.ModeSocket == 0 {
			t.Fatal("a failed chown must keep the old socket")
		}
	}

	// A stale socket stays put when the group is unknown.
	stale := filepath.Join(dir, "stale.sock")
	staleSocket(stale)
	sw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: stale, BuildSocketGroup: "no-such-group-navigatorr"})
	if err := sw.StartBuildLeases(ctx); err == nil {
		t.Fatal("an unknown group must be refused")
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Fatal("a refused group must not remove the old socket")
	}

	// Without a group the stale socket is replaced by an owner-only one.
	octx, ocancel := context.WithCancel(ctx)
	ow := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: stale})
	if err := ow.StartBuildLeases(octx); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(stale); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("owner-only socket over a stale one: %v %v", fi, err)
	}
	if c, err := net.DialTimeout("unix", stale, time.Second); err != nil {
		t.Fatal(err)
	} else {
		_ = c.Close()
	}
	// Stopping removes the socket.
	ocancel()
	for i := 0; ; i++ {
		if _, err := os.Lstat(stale); err != nil {
			break
		}
		if i == 50 {
			t.Fatal("the socket must go when the worker stops")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A second worker on a served socket is refused and leaves it alone.
	before, _ := os.Lstat(sock)
	tw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: sock, BuildSocketGroup: g.Name})
	if err := tw.StartBuildLeases(ctx); err == nil {
		t.Fatal("a second worker must not take a served socket")
	}
	if after, err := os.Lstat(sock); err != nil || !os.SameFile(before, after) {
		t.Fatalf("the first worker's socket must stay: %v", err)
	}
	if c, err := net.DialTimeout("unix", sock, time.Second); err != nil {
		t.Fatal(err)
	} else {
		_ = c.Close()
	}

	// Bind directories of this socket's killed start are cleared; anything
	// else stays (other sockets', other names, a non-socket "s").
	deadSocket := func(d string) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", filepath.Join(d, "s"))
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = l.Close()
	}
	dead := filepath.Join(dir, ".m.sock.b123")
	deadSocket(dead)
	deadEmpty := filepath.Join(dir, ".m.sock.b456")
	if err := os.Mkdir(deadEmpty, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := []string{filepath.Join(dir, ".other.sock.b789"), filepath.Join(dir, ".m.sock.bX1"), filepath.Join(dir, ".logs"), filepath.Join(dir, ".m.sock.b321")}
	deadSocket(keep[0])
	for _, k := range keep[1:3] {
		if err := os.Mkdir(k, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(keep[3], "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Stopping leaves a file put at the path since the start.
	moved := filepath.Join(dir, "m.sock")
	mctx, mcancel := context.WithCancel(ctx)
	mw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: moved})
	if err := mw.StartBuildLeases(mctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dead, deadEmpty} {
		if _, err := os.Lstat(d); err == nil {
			t.Fatalf("a dead bind directory must be cleared: %s", d)
		}
	}
	for _, k := range keep {
		if _, err := os.Lstat(k); err != nil {
			t.Fatalf("%s must stay: %v", k, err)
		}
		_ = os.RemoveAll(k)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(moved, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	mcancel()
	for i := 0; ; i++ {
		if l, err := lockLeaseSocket(moved); err == nil {
			unlockLeaseSocket(l)
			break
		}
		if i == 50 {
			t.Fatal("the worker must release the socket lock when it stops")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if b, err := os.ReadFile(moved); err != nil || string(b) != "other" {
		t.Fatalf("stopping must not remove a file it did not link: %q %v", b, err)
	}

	// A file that is not a socket is never replaced.
	reg := filepath.Join(dir, "reg")
	if err := os.WriteFile(reg, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	rw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: reg, BuildSocketGroup: g.Name})
	if err := rw.StartBuildLeases(ctx); err == nil {
		t.Fatal("a regular file at build_socket must refuse the socket")
	}
	if b, err := os.ReadFile(reg); err != nil || string(b) != "keep" {
		t.Fatalf("the regular file must stay: %q %v", b, err)
	}

	// A directory too long to bind in is refused before anything is made.
	long := filepath.Join(dir, strings.Repeat("d", 95-len(dir)))
	if err := os.Mkdir(long, 0o700); err == nil {
		lw := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: filepath.Join(long, "s")})
		if err := lw.StartBuildLeases(ctx); err == nil || !strings.Contains(err.Error(), "too long") {
			t.Fatalf("a directory too long to bind in: %v", err)
		}
		if m, _ := os.ReadDir(long); len(m) != 0 {
			t.Fatalf("nothing may be made in it: %v", m)
		}
	}

	// Binding happens in a temporary directory; none is left behind.
	if m, _ := filepath.Glob(filepath.Join(dir, ".*.b[0-9]*")); len(m) != 0 {
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
