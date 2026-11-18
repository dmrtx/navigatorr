package transcode

import (
	"slices"
	"testing"
)

type aliasExecutor struct{ Executor }

func TestWorkerAliasesResolveToTheCurrentName(t *testing.T) {
	set, err := NewWorkerSet([]NamedWorker{
		{Name: "m1pro", Aliases: []string{"m1"}, Executor: aliasExecutor{}, Podcast: true},
		{Name: "m4mini", Aliases: []string{"m4", "mac-mini"}, Executor: aliasExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"m1": "m1pro", "m1pro": "m1pro", "m4": "m4mini", "mac-mini": "m4mini", "m4mini": "m4mini"} {
		if w, ok := set.Get(name); !ok || w.Name != want {
			t.Fatalf("Get(%q) = %q, %v; want %q", name, w.Name, ok, want)
		}
		if got := set.Canonical(name); got != want {
			t.Fatalf("Canonical(%q) = %q; want %q", name, got, want)
		}
	}
	if _, ok := set.Get("m9"); ok || set.Canonical("m9") != "m9" {
		t.Fatal("an unknown name is not a worker and stays as it is")
	}
	if got := set.Renames(); !slices.Equal(got[0][:], []string{"m1", "m1pro"}) || len(got) != 3 {
		t.Fatal(got)
	}
	var none *WorkerSet
	if none.Canonical("m1") != "m1" || none.Renames() != nil {
		t.Fatal("a nil set has no aliases")
	}
}

func TestWorkerAliasesAreChecked(t *testing.T) {
	w := func(name string, aliases ...string) NamedWorker {
		return NamedWorker{Name: name, Aliases: aliases, Executor: aliasExecutor{}}
	}
	for name, list := range map[string][]NamedWorker{
		"an alias that is another worker's name": {func() NamedWorker { x := w("a", "b"); x.Podcast = true; return x }(), w("b")},
		"an alias used twice":                    {func() NamedWorker { x := w("a", "x"); x.Podcast = true; return x }(), w("b", "x")},
		"an invalid alias":                       {func() NamedWorker { x := w("a", "Not Valid"); x.Podcast = true; return x }()},
		"video-worker as an alias":               {func() NamedWorker { x := w("a", DefaultWorkerName); x.Podcast = true; return x }()},
		"an alias that is the worker's own name": {func() NamedWorker { x := w("a", "a"); x.Podcast = true; return x }()},
	} {
		if _, err := NewWorkerSet(list); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}
