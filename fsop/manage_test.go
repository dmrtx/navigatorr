package fsop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMkdirCreatesAndIsIdempotent(t *testing.T) {
	r, root, _ := testResolver(t)
	dir := filepath.Join(root, "Movies", "New (2020)")
	res, err := r.Mkdir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 || res.Existed {
		t.Fatalf("created = %v existed = %v", res.Created, res.Existed)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("folder missing: %v", err)
	}
	again, err := r.Mkdir(dir)
	if err != nil || !again.Existed || len(again.Created) != 0 {
		t.Fatalf("second call = %+v, %v", again, err)
	}
}

func TestMkdirRefusals(t *testing.T) {
	r, root, outside := testResolver(t)
	put(t, filepath.Join(root, "file.mkv"), 5, time.Hour)
	os.Symlink(outside, filepath.Join(root, "link"))
	for name, path := range map[string]string{
		"outside the roots": filepath.Join(outside, "x"),
		"over a file":       filepath.Join(root, "file.mkv", "sub"),
		"as a file":         filepath.Join(root, "file.mkv"),
		"hidden name":       filepath.Join(root, ".hidden"),
		"system name":       filepath.Join(root, "#recycle"),
		"through a link":    filepath.Join(root, "link", "x"),
	} {
		if _, err := r.Mkdir(path); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was created outside the roots: %v", entries)
	}
}

func TestMkdirRefusesWhenRootLostItsMount(t *testing.T) {
	r, root, _ := testResolver(t)
	r.writeMounts[root] = true
	old := mountPointFn
	mountPointFn = func(string) (bool, error) { return false, nil }
	defer func() { mountPointFn = old }()
	if _, err := r.Mkdir(filepath.Join(root, "x")); err == nil || !strings.Contains(err.Error(), "mounted") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Fatal("folder created in an unmounted root")
	}
}

func removeFixture(t *testing.T) (*Resolver, string) {
	t.Helper()
	r, root, _ := testResolver(t)
	day := 48 * time.Hour
	put(t, filepath.Join(root, "Movie (1997)", "Stray", "big.mkv"), 300, day)
	put(t, filepath.Join(root, "Movie (1997)", "Stray", "big.en.srt"), 10, day)
	put(t, filepath.Join(root, "Movie (1997)", "Movie (1997).mkv"), 100, day)
	ageDirs(t, root, day)
	return r, root
}

func TestPlanRemoveDescribesAFolderAndNeedsRecursive(t *testing.T) {
	r, root := removeFixture(t)
	stray := filepath.Join(root, "Movie (1997)", "Stray")
	plan, err := r.PlanRemove(stray, false, RemovePolicy{MinAge: DefaultRemoveAge})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != "folder" || plan.Files != 2 || plan.Dirs != 1 || plan.Bytes != 310 {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0] != BlockedNotEmpty {
		t.Fatalf("blocked = %v", plan.Blocked)
	}
	rec, err := r.PlanRemove(stray, true, RemovePolicy{MinAge: DefaultRemoveAge})
	if err != nil || len(rec.Blocked) != 0 || rec.Digest == "" {
		t.Fatalf("recursive plan = %+v, %v", rec, err)
	}
	if rec.Sample[0].Path != filepath.Join("big.mkv") {
		t.Fatalf("sample should lead with the biggest file: %+v", rec.Sample)
	}
	if _, err := os.Stat(filepath.Join(stray, "big.mkv")); err != nil {
		t.Fatal("a plan removed something")
	}
}

func TestRemoveFolderWithDigest(t *testing.T) {
	r, root := removeFixture(t)
	stray := filepath.Join(root, "Movie (1997)", "Stray")
	pol := RemovePolicy{MinAge: DefaultRemoveAge}
	plan, _ := r.PlanRemove(stray, true, pol)
	res := r.Remove(stray, true, plan.Digest, pol)
	if res.Status != "removed" || res.Files != 2 || res.Dirs != 1 || res.Bytes != 310 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("folder still there")
	}
	if _, err := os.Stat(filepath.Join(root, "Movie (1997)", "Movie (1997).mkv")); err != nil {
		t.Fatal("a neighbour was removed")
	}
	if again := r.Remove(stray, true, plan.Digest, pol); again.Status != "already_absent" {
		t.Fatalf("second removal = %+v", again)
	}
}

func TestRemoveSingleFile(t *testing.T) {
	r, root := removeFixture(t)
	file := filepath.Join(root, "Movie (1997)", "Stray", "big.mkv")
	pol := RemovePolicy{MinAge: DefaultRemoveAge}
	plan, err := r.PlanRemove(file, false, pol)
	if err != nil || plan.Kind != "file" || plan.Files != 1 || len(plan.Blocked) != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if res := r.Remove(file, false, plan.Digest, pol); res.Status != "removed" || res.Bytes != 300 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "Movie (1997)", "Stray", "big.en.srt")); err != nil {
		t.Fatal("the sibling went too")
	}
}

func TestRemoveStopsWhenTheTreeChanged(t *testing.T) {
	r, root := removeFixture(t)
	stray := filepath.Join(root, "Movie (1997)", "Stray")
	pol := RemovePolicy{MinAge: DefaultRemoveAge}
	plan, _ := r.PlanRemove(stray, true, pol)
	put(t, filepath.Join(stray, "new.mkv"), 5, 48*time.Hour)
	ageDirs(t, root, 48*time.Hour)
	res := r.Remove(stray, true, plan.Digest, pol)
	if res.Status != "skipped" || !strings.Contains(res.Reason, "changed since the preview") {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(stray, "big.mkv")); err != nil {
		t.Fatal("something was removed")
	}
	if res := r.Remove(stray, true, "", pol); res.Status != "skipped" {
		t.Fatalf("no digest: %+v", res)
	}
	if res := r.Remove(stray, true, "deadbeef", pol); res.Status != "skipped" {
		t.Fatalf("wrong digest: %+v", res)
	}
}

func TestRemoveBlockedCases(t *testing.T) {
	r, root := removeFixture(t)
	day := 48 * time.Hour
	put(t, filepath.Join(root, ".navigatorr-candidates", "c.mkv"), 5, day)
	put(t, filepath.Join(root, "Fresh", "f.mkv"), 5, 0)
	put(t, filepath.Join(root, "WithHidden", "x.mkv"), 5, day)
	put(t, filepath.Join(root, "WithHidden", "@eaDir", "y"), 5, day)
	os.Symlink(filepath.Join(root, "Movie (1997)"), filepath.Join(root, "Link"))
	put(t, filepath.Join(root, "WithLink", "x.mkv"), 5, day)
	os.Symlink(filepath.Join(root, "Movie (1997)"), filepath.Join(root, "WithLink", "inner"))
	ageDirs(t, root, day)
	pol := RemovePolicy{MinAge: DefaultRemoveAge}
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"the root":         {root, "allowed root"},
		"hidden folder":    {filepath.Join(root, ".navigatorr-candidates"), "hidden and system"},
		"inside hidden":    {filepath.Join(root, ".navigatorr-candidates", "c.mkv"), "hidden and system"},
		"hidden inside":    {filepath.Join(root, "WithHidden"), "hidden and system"},
		"recent":           {filepath.Join(root, "Fresh", "f.mkv"), "less than"},
		"symlink":          {filepath.Join(root, "Link"), "symlink"},
		"contains symlink": {filepath.Join(root, "WithLink"), "symlink"},
	} {
		plan, err := r.PlanRemove(tc.path, true, pol)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(plan.Blocked) == 0 || !strings.Contains(strings.Join(plan.Blocked, "|"), tc.want) {
			t.Errorf("%s: blocked = %v, want %q", name, plan.Blocked, tc.want)
		}
		if res := r.Remove(tc.path, true, plan.Digest, pol); res.Status == "removed" {
			t.Errorf("%s: was removed", name)
		}
	}
	if _, err := r.PlanRemove(filepath.Join(root, "..", "elsewhere"), true, pol); err == nil {
		t.Error("a path outside the roots must be an error")
	}
}

func TestRemoveRespectsDownloadClientAndLibraryClaims(t *testing.T) {
	r, root := removeFixture(t)
	movie := filepath.Join(root, "Movie (1997)")
	tracked := filepath.Join(movie, "Movie (1997).mkv")
	base := RemovePolicy{MinAge: DefaultRemoveAge}

	cases := map[string]struct {
		path   string
		policy RemovePolicy
		blocks bool
		want   string
	}{
		"stray beside the tracked file": {filepath.Join(movie, "Stray"),
			RemovePolicy{MinAge: base.MinAge, Tracked: []Claim{{Path: movie, Owner: "radarr", Folder: true}, {Path: tracked, Owner: "radarr"}}}, false, ""},
		"the tracked file": {tracked,
			RemovePolicy{MinAge: base.MinAge, Tracked: []Claim{{Path: tracked, Owner: "radarr"}}}, true, "radarr tracks the file"},
		"the movie folder": {movie,
			RemovePolicy{MinAge: base.MinAge, Tracked: []Claim{{Path: movie, Owner: "radarr", Folder: true}}}, true, "library folder"},
		"owned by the client": {filepath.Join(movie, "Stray"),
			RemovePolicy{MinAge: base.MinAge, Owned: []string{filepath.Join(movie, "Stray", "big.mkv")}}, true, "still owns"},
		"owner is the folder": {filepath.Join(movie, "Stray", "big.mkv"),
			RemovePolicy{MinAge: base.MinAge, Owned: []string{filepath.Join(movie, "Stray")}}, true, "still owns"},
		"client saves inside": {movie,
			RemovePolicy{MinAge: base.MinAge, Keep: []string{filepath.Join(movie, "Stray")}}, true, "saves into"},
	}
	for name, tc := range cases {
		plan, err := r.PlanRemove(tc.path, true, tc.policy)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got := strings.Join(plan.Blocked, "|")
		if tc.blocks != (len(plan.Blocked) > 0) || !strings.Contains(got, tc.want) {
			t.Errorf("%s: blocked = %v", name, plan.Blocked)
		}
	}
}

func TestPathsOverlap(t *testing.T) {
	if !PathsOverlap("/a/b", "/A/b/c") || !PathsOverlap("/a/b/c", "/a/B") {
		t.Error("nesting either way, ignoring case, must overlap")
	}
	if PathsOverlap("/a/bc", "/a/b") || PathsOverlap("", "/a") {
		t.Error("siblings with a shared prefix must not overlap")
	}
}
