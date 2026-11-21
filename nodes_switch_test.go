package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/nodes"
)

type noNodes struct{}

func (noNodes) Node(t string) (string, bool) { return t, true }
func (noNodes) Nodes() []string              { return []string{"nodeA"} }

func switchCfg(url, keyFile string) *config.Config {
	cfg := &config.Config{}
	cfg.Nodes.URL = url
	cfg.Nodes.KeyFile = keyFile
	return cfg
}

func writeKey(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nodes.key")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// With nodes.url set, a key that cannot be used stops the server before the
// in-process registry is ever opened: there is no fallback.
func TestNodesURLWithAnUnusableKeyNeverOpensTheInProcessRegistry(t *testing.T) {
	// Built at run time so secret scanners have nothing to read.
	good := strings.Repeat("nodeskey", 5)
	unreadable := writeKey(t, good, 0o600)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(unreadable); err == nil { // running as a user that can read it anyway
		f.Close()
		unreadable = ""
	}
	cases := map[string]string{
		"no key file configured": "",
		"missing":                filepath.Join(t.TempDir(), "absent.key"),
		"empty":                  writeKey(t, "\n", 0o600),
		"too short":              writeKey(t, "short", 0o600),
		"not ascii":              writeKey(t, strings.Repeat("é", 40), 0o600),
	}
	if unreadable != "" {
		cases["unreadable"] = unreadable
	}
	for name, keyFile := range cases {
		opened := false
		b, err := openNodes(switchCfg("http://127.0.0.1:8097", keyFile), nodesInputs{DBPath: filepath.Join(t.TempDir(), "main.db"), Topology: noNodes{}},
			func(nodes.Options) (*nodes.Service, error) { opened = true; return nil, errors.New("must not open") })
		if err == nil || b != nil {
			t.Errorf("%s: accepted (%v)", name, b)
		}
		if opened {
			t.Errorf("%s: nodes.Open was called", name)
		}
		if err != nil && strings.Contains(err.Error(), good) {
			t.Errorf("%s: the error carries the key", name)
		}
	}
}

func TestNodesURLWithAGoodKeyIsRemoteOnly(t *testing.T) {
	keyFile := writeKey(t, strings.Repeat("nodeskey", 5)+"\n", 0o600)
	opened := false
	b, err := openNodes(switchCfg("http://127.0.0.1:8097", keyFile), nodesInputs{DBPath: filepath.Join(t.TempDir(), "main.db"), Topology: noNodes{}},
		func(nodes.Options) (*nodes.Service, error) { opened = true; return nil, errors.New("must not open") })
	if err != nil || opened {
		t.Fatalf("%v, opened=%v", err, opened)
	}
	defer b.Close()
	if b.Service != nil || b.Client == nil || b.Proxy == nil {
		t.Fatalf("%+v", b)
	}
	// The in-process work has nothing to start or stop.
	b.Start(filepath.Join(t.TempDir(), "main.db"))()
}

func TestNodesURLWithABadURLIsRefused(t *testing.T) {
	keyFile := writeKey(t, strings.Repeat("nodeskey", 5), 0o600)
	if _, err := openNodes(switchCfg("http://127.0.0.1:8097/some/path", keyFile), nodesInputs{Topology: noNodes{}},
		func(nodes.Options) (*nodes.Service, error) { t.Fatal("opened"); return nil, nil }); err == nil {
		t.Fatal("a url with a path was accepted")
	}
}

// No nodes.url: the registry opens in this process, at the derived path, as
// before, and a key file is not even read.
func TestEmptyNodesURLOpensInProcess(t *testing.T) {
	dir := t.TempDir()
	cfg := switchCfg("", filepath.Join(dir, "absent.key"))
	var got nodes.Options
	b, err := openNodes(cfg, nodesInputs{DBPath: filepath.Join(dir, "main.db"), Topology: noNodes{}},
		func(o nodes.Options) (*nodes.Service, error) { got = o; return nodes.Open(o) })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Service == nil || b.Client != nil || b.Proxy != nil {
		t.Fatalf("%+v", b)
	}
	if got.Path != cfg.Database.NodesDatabasePath(filepath.Join(dir, "main.db")) || got.Topology == nil {
		t.Fatalf("options: %+v", got)
	}
	// The failure of the open itself is reported with the path, as before.
	_, err = openNodes(cfg, nodesInputs{DBPath: filepath.Join(dir, "main.db")}, func(nodes.Options) (*nodes.Service, error) { return nil, errors.New("locked") })
	if err == nil || !strings.Contains(err.Error(), "locked") || !strings.Contains(err.Error(), "nodes database") {
		t.Fatal(err)
	}
}
