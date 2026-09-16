package transcodeworker

// Phase 6A storage-ops tests: staging policy parsing, external-path boundary
// semantics, deterministic per-job local paths, and atomic staging /
// finalization behavior. All tests use temp dirs only.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func mustWriteFile(t *testing.T, path string, content []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func TestParseStagingPolicy(t *testing.T) {
	cases := []struct {
		in   string
		want StagingPolicy
		ok   bool
	}{
		{"auto", StagingPolicyAuto, true},
		{"never", StagingPolicyNever, true},
		{"always", StagingPolicyAlways, true},
		{"  auto  ", StagingPolicyAuto, true},
		{"", "", false},
		{"AUTO", "", false},
		{"Sometimes", "", false},
		{"alwaysx", "", false},
	}
	for _, tc := range cases {
		got, err := ParseStagingPolicy(tc.in)
		if tc.ok {
			if err != nil {
				t.Fatalf("ParseStagingPolicy(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseStagingPolicy(%q) = %q, want %q", tc.in, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Fatalf("ParseStagingPolicy(%q) expected error", tc.in)
		}
		if !errors.Is(err, ErrInvalidStagingPolicy) {
			t.Fatalf("ParseStagingPolicy(%q) error = %v, want ErrInvalidStagingPolicy", tc.in, err)
		}
	}

	if DefaultStagingPolicy() != StagingPolicyAuto {
		t.Fatalf("DefaultStagingPolicy() = %q, want auto", DefaultStagingPolicy())
	}
}

func TestIsExternalPathBoundary(t *testing.T) {
	base := t.TempDir()
	media := filepath.Join(base, "media")
	media2 := filepath.Join(base, "media2")
	medi := filepath.Join(base, "medi")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root itself", media, true},
		{"direct child", filepath.Join(media, "movie.mkv"), true},
		{"nested child", filepath.Join(media, "sub", "dir", "movie.mkv"), true},
		{"prefix sibling", media2, false},
		{"prefix sibling child", filepath.Join(media2, "movie.mkv"), false},
		{"shorter prefix", medi, false},
		{"escapes root", filepath.Join(media, "..", "outside.mkv"), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := IsExternalPath(tc.path, []string{media}); got != tc.want {
			t.Fatalf("%s: IsExternalPath(%q) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}

	if IsExternalPath(media, nil) {
		t.Fatal("IsExternalPath with no roots must be false")
	}
	if IsExternalPath(media, []string{"", "   "}) {
		t.Fatal("IsExternalPath with blank roots must be false")
	}
}

func TestShouldStageInput(t *testing.T) {
	base := t.TempDir()
	extRoot := filepath.Join(base, "net")
	externalFile := filepath.Join(extRoot, "movie.mkv")
	localFile := filepath.Join(base, "local.mkv")
	roots := []string{extRoot}

	check := func(policy StagingPolicy, path string, want bool) {
		t.Helper()
		got, err := ShouldStageInput(policy, path, roots)
		if err != nil {
			t.Fatalf("ShouldStageInput(%q, %q) unexpected error: %v", policy, path, err)
		}
		if got != want {
			t.Fatalf("ShouldStageInput(%q, %q) = %v, want %v", policy, path, got, want)
		}
	}

	check(StagingPolicyAlways, localFile, true)
	check(StagingPolicyAlways, externalFile, true)
	check(StagingPolicyNever, externalFile, false)
	check(StagingPolicyNever, localFile, false)
	check(StagingPolicyAuto, externalFile, true)
	check(StagingPolicyAuto, localFile, false)
	check(StagingPolicyAuto, extRoot, true)

	// Unknown policy must fail closed, never silently behave like auto.
	for _, bad := range []StagingPolicy{"", "bogus", "AUTO"} {
		got, err := ShouldStageInput(bad, externalFile, roots)
		if err == nil {
			t.Fatalf("ShouldStageInput(%q) expected error", bad)
		}
		if !errors.Is(err, ErrInvalidStagingPolicy) {
			t.Fatalf("ShouldStageInput(%q) error = %v, want ErrInvalidStagingPolicy", bad, err)
		}
		if got {
			t.Fatalf("ShouldStageInput(%q) = true, must fail closed to false", bad)
		}
	}
}

func TestSafeLocalPathHelpers(t *testing.T) {
	work := t.TempDir()
	source := filepath.Join(work, "incoming", "clip.MP4")

	staged, err := StagedInputPath(work, "job-1", source)
	if err != nil {
		t.Fatalf("StagedInputPath: %v", err)
	}
	wantStaged := filepath.Join(work, "job-1", "input.MP4")
	if staged != wantStaged {
		t.Fatalf("StagedInputPath = %q, want %q", staged, wantStaged)
	}

	candidate, err := LocalCandidatePath(work, "job-1", filepath.Join(work, "out", "clip.mkv"))
	if err != nil {
		t.Fatalf("LocalCandidatePath: %v", err)
	}
	wantCandidate := filepath.Join(work, "job-1", "candidate.mkv")
	if candidate != wantCandidate {
		t.Fatalf("LocalCandidatePath = %q, want %q", candidate, wantCandidate)
	}

	// Deterministic for the same inputs.
	again, err := StagedInputPath(work, "job-1", source)
	if err != nil || again != staged {
		t.Fatalf("StagedInputPath not deterministic: %q (%v)", again, err)
	}

	// No extension.
	noExt, err := StagedInputPath(work, "job-2", filepath.Join(work, "noext"))
	if err != nil || noExt != filepath.Join(work, "job-2", "input") {
		t.Fatalf("no-extension staged path = %q (%v)", noExt, err)
	}

	// Untrusted extension is sanitized away rather than used verbatim.
	weird, err := StagedInputPath(work, "job-3", filepath.Join(work, "x.we!rd"))
	if err != nil || weird != filepath.Join(work, "job-3", "input") {
		t.Fatalf("weird-extension staged path = %q (%v)", weird, err)
	}

	// Malicious job ids must be rejected by both helpers.
	badIDs := []string{"", ".", "..", "../evil", "../../etc/passwd", "a/b", `a\b`, "/abs", "job\x00x", " spaced ", "./x"}
	for _, id := range badIDs {
		if _, err := StagedInputPath(work, id, source); err == nil || !IsInvalidJobID(err) {
			t.Fatalf("StagedInputPath accepted job id %q: %v", id, err)
		}
		if _, err := LocalCandidatePath(work, id, source); err == nil || !IsInvalidJobID(err) {
			t.Fatalf("LocalCandidatePath accepted job id %q: %v", id, err)
		}
	}

	if _, err := StagedInputPath("   ", "job-1", source); err == nil {
		t.Fatal("StagedInputPath must reject empty work dir")
	}
}

func TestStageInputAtomicSuccess(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "src.mov")
	content := []byte("hello staging world")
	mustWriteFile(t, source, content, 0o640)

	stagedFinal := filepath.Join(base, "work", "job-1", "input.mov")
	if err := StageInputAtomic(context.Background(), source, stagedFinal); err != nil {
		t.Fatalf("StageInputAtomic: %v", err)
	}

	got, err := os.ReadFile(stagedFinal)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("staged content = %q, want %q", got, content)
	}

	fi, err := os.Stat(stagedFinal)
	if err != nil {
		t.Fatalf("stat staged: %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Fatalf("staged size = %d, want %d", fi.Size(), len(content))
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("staged mode = %o, want 640", fi.Mode().Perm())
	}

	names := readDirNames(t, filepath.Dir(stagedFinal))
	if len(names) != 1 || names[0] != "input.mov" {
		t.Fatalf("unexpected staging leftovers: %v", names)
	}
}

func TestStageInputAtomicRejectsPreexistingFinal(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "src.mov")
	mustWriteFile(t, source, []byte("new data"), 0o644)

	final := filepath.Join(base, "input.mov")
	mustWriteFile(t, final, []byte("original"), 0o644)

	err := StageInputAtomic(context.Background(), source, final)
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("preexisting final was modified: %q", got)
	}
}

func TestStageInputAtomicSourceMissing(t *testing.T) {
	base := t.TempDir()
	final := filepath.Join(base, "input.mov")

	err := StageInputAtomic(context.Background(), filepath.Join(base, "nope.mov"), final)
	if !IsSourceMissing(err) {
		t.Fatalf("error = %v, want ErrSourceMissing", err)
	}
	if exists, _ := pathExists(final); exists {
		t.Fatal("final must not exist after missing-source failure")
	}
}

func TestStageInputAtomicSourceNotRegular(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "adir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(base, "input.mov")

	err := StageInputAtomic(context.Background(), dir, final)
	if !IsSourceInvalid(err) {
		t.Fatalf("error = %v, want ErrSourceInvalid", err)
	}
	if exists, _ := pathExists(final); exists {
		t.Fatal("final must not exist after invalid-source failure")
	}
}

func TestStageInputAtomicCancellationLeavesNoArtifacts(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "src.mov")
	mustWriteFile(t, source, []byte("data"), 0o644)
	final := filepath.Join(base, "work", "job-1", "input.mov")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := StageInputAtomic(ctx, source, final)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if exists, _ := pathExists(final); exists {
		t.Fatal("final must not exist after cancellation")
	}
	if names := readDirNames(t, filepath.Dir(final)); len(names) != 0 {
		t.Fatalf("temp/final leaks after cancellation: %v", names)
	}
}

func TestStageInputAtomicHookFailureLeavesNoArtifacts(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "src.mov")
	content := []byte("data")
	mustWriteFile(t, source, content, 0o644)
	final := filepath.Join(base, "work", "job-1", "input.mov")
	sentinel := errors.New("boom")

	observedFinalAbsent := false
	observedTempPresent := false
	err := stageInputAtomic(context.Background(), source, final, func(tempPath string) error {
		if exists, _ := pathExists(final); !exists {
			observedFinalAbsent = true
		} else {
			t.Error("final visible before rename")
		}
		if got, rerr := os.ReadFile(tempPath); rerr == nil && bytes.Equal(got, content) {
			observedTempPresent = true
		} else {
			t.Errorf("temp not fully written at hook: %q (%v)", got, rerr)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if !observedFinalAbsent || !observedTempPresent {
		t.Fatalf("hook observations: finalAbsent=%v tempPresent=%v", observedFinalAbsent, observedTempPresent)
	}
	if exists, _ := pathExists(final); exists {
		t.Fatal("final must not exist after hook failure")
	}
	if names := readDirNames(t, filepath.Dir(final)); len(names) != 0 {
		t.Fatalf("temp/final leaks after hook failure: %v", names)
	}
}

func TestFinalizeOutputAtomicSuccess(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := []byte("encoded output bytes")
	mustWriteFile(t, candidate, content, 0o640)
	destination := filepath.Join(base, "dest", "final.mkv")

	if err := FinalizeOutputAtomic(context.Background(), candidate, destination, "job-1"); err != nil {
		t.Fatalf("FinalizeOutputAtomic: %v", err)
	}

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("destination content = %q, want %q", got, content)
	}
	fi, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat destination: %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Fatalf("destination size = %d, want %d", fi.Size(), len(content))
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("destination mode = %o, want 640", fi.Mode().Perm())
	}

	partial := destination + ".partial.job-1"
	if exists, _ := pathExists(partial); exists {
		t.Fatal("own partial must be gone after success")
	}
	names := readDirNames(t, filepath.Dir(destination))
	if len(names) != 1 || names[0] != "final.mkv" {
		t.Fatalf("unexpected destination leftovers: %v", names)
	}
}

func TestFinalizeOutputAtomicRejectsPreexistingFinal(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("new"), 0o644)

	destination := filepath.Join(base, "final.mkv")
	mustWriteFile(t, destination, []byte("existing"), 0o644)

	err := FinalizeOutputAtomic(context.Background(), candidate, destination, "job-1")
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != "existing" {
		t.Fatalf("preexisting final was overwritten: %q", got)
	}
	if exists, _ := pathExists(destination + ".partial.job-1"); exists {
		t.Fatal("partial must not be created when destination preexists")
	}
}

func TestFinalizeOutputAtomicReplacesOwnStalePartial(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := []byte("fresh output")
	mustWriteFile(t, candidate, content, 0o644)

	destination := filepath.Join(base, "final.mkv")
	partial := destination + ".partial.job-1"
	mustWriteFile(t, partial, []byte("stale partial"), 0o644)

	if err := FinalizeOutputAtomic(context.Background(), candidate, destination, "job-1"); err != nil {
		t.Fatalf("FinalizeOutputAtomic: %v", err)
	}

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("destination content = %q, want %q", got, content)
	}
	if exists, _ := pathExists(partial); exists {
		t.Fatal("stale own partial must be replaced/removed")
	}
}

func TestFinalizeOutputAtomicLeavesUnrelatedPartial(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("fresh output"), 0o644)

	destination := filepath.Join(base, "final.mkv")
	unrelated := destination + ".partial.other"
	mustWriteFile(t, unrelated, []byte("keep me"), 0o644)

	if err := FinalizeOutputAtomic(context.Background(), candidate, destination, "job-1"); err != nil {
		t.Fatalf("FinalizeOutputAtomic: %v", err)
	}

	got, err := os.ReadFile(unrelated)
	if err != nil {
		t.Fatalf("read unrelated partial: %v", err)
	}
	if string(got) != "keep me" {
		t.Fatalf("unrelated partial was modified: %q", got)
	}
}

func TestFinalizeOutputAtomicCancellationLeavesNoArtifacts(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("data"), 0o644)
	destination := filepath.Join(base, "dest", "final.mkv")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := FinalizeOutputAtomic(ctx, candidate, destination, "job-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if exists, _ := pathExists(destination); exists {
		t.Fatal("destination must not exist after cancellation")
	}
	if exists, _ := pathExists(destination + ".partial.job-1"); exists {
		t.Fatal("own partial must not survive cancellation")
	}
}

func TestFinalizeOutputAtomicHookFailureLeavesNoArtifacts(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := []byte("data bytes")
	mustWriteFile(t, candidate, content, 0o644)
	destination := filepath.Join(base, "dest", "final.mkv")
	partial := destination + ".partial.job-1"
	sentinel := errors.New("boom")

	observedFinalAbsent := false
	observedPartialPresent := false
	err := finalizeOutputAtomic(context.Background(), candidate, destination, "job-1", func(p string) error {
		if p != partial {
			t.Errorf("hook partial = %q, want %q", p, partial)
		}
		if exists, _ := pathExists(destination); !exists {
			observedFinalAbsent = true
		} else {
			t.Error("destination visible before commit rename")
		}
		if got, rerr := os.ReadFile(partial); rerr == nil && bytes.Equal(got, content) {
			observedPartialPresent = true
		} else {
			t.Errorf("partial not fully written at hook: %q (%v)", got, rerr)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if !observedFinalAbsent || !observedPartialPresent {
		t.Fatalf("hook observations: finalAbsent=%v partialPresent=%v", observedFinalAbsent, observedPartialPresent)
	}
	if exists, _ := pathExists(destination); exists {
		t.Fatal("destination must not exist after hook failure")
	}
	if exists, _ := pathExists(partial); exists {
		t.Fatal("own partial must be cleaned after hook failure")
	}
	if names := readDirNames(t, filepath.Dir(destination)); len(names) != 0 {
		t.Fatalf("destination leaks after hook failure: %v", names)
	}
}

func TestFinalizeOutputAtomicNotVisibleBeforeCommit(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := []byte("payload")
	mustWriteFile(t, candidate, content, 0o644)
	destination := filepath.Join(base, "final.mkv")
	partial := destination + ".partial.job-42"

	err := finalizeOutputAtomic(context.Background(), candidate, destination, "job-42", func(p string) error {
		if p != partial {
			t.Fatalf("partial path = %q, want exact %q", p, partial)
		}
		if exists, _ := pathExists(destination); exists {
			t.Fatal("destination must not be visible before the commit rename")
		}
		got, rerr := os.ReadFile(partial)
		if rerr != nil || !bytes.Equal(got, content) {
			t.Fatalf("partial at hook = %q (%v), want %q", got, rerr, content)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("finalizeOutputAtomic: %v", err)
	}

	if exists, _ := pathExists(partial); exists {
		t.Fatal("partial must not remain after commit")
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("destination content = %q, want %q", got, content)
	}
}

func TestFinalizeOutputAtomicCandidateMissing(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "final.mkv")

	err := FinalizeOutputAtomic(context.Background(), filepath.Join(base, "nope.mkv"), destination, "job-1")
	if !IsSourceMissing(err) {
		t.Fatalf("error = %v, want ErrSourceMissing", err)
	}
	if exists, _ := pathExists(destination); exists {
		t.Fatal("destination must not exist after missing-candidate failure")
	}
}

func TestFinalizeOutputAtomicRejectsMaliciousJobID(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("data"), 0o644)
	destination := filepath.Join(base, "final.mkv")

	badIDs := []string{"", ".", "..", "../evil", "a/b", `a\b`, "/abs", " spaced "}
	for _, id := range badIDs {
		err := FinalizeOutputAtomic(context.Background(), candidate, destination, id)
		if !IsInvalidJobID(err) {
			t.Fatalf("job id %q: error = %v, want ErrInvalidJobID", id, err)
		}
	}
	if exists, _ := pathExists(destination); exists {
		t.Fatal("destination must not be created for malicious job ids")
	}
}

func TestStageInputAtomicNoClobberRaceCommit(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "src.mov")
	mustWriteFile(t, source, []byte("new bytes"), 0o644)
	final := filepath.Join(base, "work", "job-1", "input.mov")
	sentinel := []byte("sentinel winner")

	err := stageInputAtomic(context.Background(), source, final, func(tempPath string) error {
		// A concurrent publisher wins the destination after our pre-check.
		return os.WriteFile(final, sentinel, 0o600)
	})
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	got, rerr := os.ReadFile(final)
	if rerr != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("existing destination clobbered: %q (%v)", got, rerr)
	}
	if names := readDirNames(t, filepath.Dir(final)); len(names) != 1 || names[0] != "input.mov" {
		t.Fatalf("temp leak after failed no-clobber commit: %v", names)
	}
}

func TestFinalizeOutputAtomicNoClobberRaceCommit(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("new bytes"), 0o644)
	destination := filepath.Join(base, "dest", "final.mkv")
	partial := destination + ".partial.job-1"
	unrelated := destination + ".partial.other"
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, unrelated, []byte("unrelated"), 0o644)
	sentinel := []byte("sentinel winner")

	err := finalizeOutputAtomic(context.Background(), candidate, destination, "job-1", func(p string) error {
		// A concurrent publisher wins the destination after our pre-check.
		return os.WriteFile(destination, sentinel, 0o600)
	})
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	got, rerr := os.ReadFile(destination)
	if rerr != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("existing destination clobbered: %q (%v)", got, rerr)
	}
	if exists, _ := pathExists(partial); exists {
		t.Fatal("own partial must be cleaned after failed no-clobber commit")
	}
	unrel, uerr := os.ReadFile(unrelated)
	if uerr != nil || string(unrel) != "unrelated" {
		t.Fatalf("unrelated partial altered: %q (%v)", unrel, uerr)
	}
}

func TestStageInputAtomicCancellationAtCommit(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "src.mov")
	mustWriteFile(t, source, []byte("data"), 0o644)
	final := filepath.Join(base, "work", "job-1", "input.mov")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := stageInputAtomic(ctx, source, final, func(tempPath string) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if exists, _ := pathExists(final); exists {
		t.Fatal("final must be absent after commit-boundary cancellation")
	}
	if names := readDirNames(t, filepath.Dir(final)); len(names) != 0 {
		t.Fatalf("temp leak after commit-boundary cancellation: %v", names)
	}
}

func TestFinalizeOutputAtomicCancellationAtCommit(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("data"), 0o644)
	destination := filepath.Join(base, "dest", "final.mkv")
	partial := destination + ".partial.job-1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := finalizeOutputAtomic(ctx, candidate, destination, "job-1", func(p string) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if exists, _ := pathExists(destination); exists {
		t.Fatal("destination must be absent after commit-boundary cancellation")
	}
	if exists, _ := pathExists(partial); exists {
		t.Fatal("own partial must be cleaned after commit-boundary cancellation")
	}
}

// TestLinkNoReplaceFallback covers the portable hard-link publication used when
// no native rename-no-replace is available: it must publish atomically, consume
// its source, and fail with fs.ErrExist without touching an existing
// destination.
func TestLinkNoReplaceFallback(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	content := []byte("published bytes")
	mustWriteFile(t, src, content, 0o600)

	if err := linkNoReplace(src, dst); err != nil {
		t.Fatalf("linkNoReplace: %v", err)
	}
	if exists, _ := pathExists(src); exists {
		t.Fatal("source must be consumed by the link publication")
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published = %q (%v), want %q", got, err, content)
	}

	// No-clobber: an existing destination is never replaced.
	existing := []byte("sentinel winner")
	mustWriteFile(t, dst, existing, 0o600)
	src2 := filepath.Join(base, "src2")
	mustWriteFile(t, src2, []byte("new bytes"), 0o600)

	err = linkNoReplace(src2, dst)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("error = %v, want fs.ErrExist", err)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil || !bytes.Equal(got, existing) {
		t.Fatalf("destination was clobbered: %q (%v)", got, rerr)
	}
	if exists, _ := pathExists(src2); !exists {
		t.Fatal("source must survive a failed no-clobber publication")
	}
}

// unsupportedRename and unsupportedLink simulate a filesystem (for example a
// macOS SMB/smbfs mount) where neither an exclusive rename nor hard links are
// supported, forcing the exclusive-copy publication path.
func unsupportedRename(_, _ string) error { return errNoReplaceUnsupported }
func unsupportedLink(_, _ string) error   { return errors.New("operation not supported") }

func smbCommitFallback() commitFunc {
	return func(ctx context.Context, src, dst string) error {
		return commitNoReplaceWith(ctx, src, dst, unsupportedRename, unsupportedLink)
	}
}

func TestCommitNoReplaceWithHardLinkSupported(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	content := []byte("link published bytes")
	mustWriteFile(t, src, content, 0o640)

	if err := commitNoReplaceWith(context.Background(), src, dst, unsupportedRename, linkNoReplace); err != nil {
		t.Fatalf("commitNoReplaceWith: %v", err)
	}
	if exists, _ := pathExists(src); exists {
		t.Fatal("source must be consumed by the link publication")
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published = %q (%v), want %q", got, err, content)
	}
}

func TestCommitNoReplaceFallsBackToExclusiveCopyWhenHardLinksUnsupported(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	content := []byte("smb published bytes")
	mustWriteFile(t, src, content, 0o640)

	if err := commitNoReplaceWith(context.Background(), src, dst, unsupportedRename, unsupportedLink); err != nil {
		t.Fatalf("commitNoReplaceWith: %v", err)
	}
	if exists, _ := pathExists(src); exists {
		t.Fatal("source must be consumed by the exclusive-copy publication")
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published = %q (%v), want %q", got, err, content)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("destination mode = %v (%v), want 640", fi.Mode().Perm(), err)
	}
	if names := readDirNames(t, base); len(names) != 1 || names[0] != "dst" {
		t.Fatalf("unexpected leftovers after exclusive-copy publish: %v", names)
	}
}

func TestCommitNoReplaceExclusiveCopyNeverClobbersExistingDestination(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	mustWriteFile(t, src, []byte("new bytes"), 0o600)
	sentinel := []byte("sentinel winner")
	mustWriteFile(t, dst, sentinel, 0o600)

	err := commitNoReplaceWith(context.Background(), src, dst, unsupportedRename, unsupportedLink)
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("existing destination clobbered: %q (%v)", got, rerr)
	}
	if exists, _ := pathExists(src); !exists {
		t.Fatal("source must survive a failed no-clobber publication")
	}
}

// TestCopyNoReplaceCleansUpFailedCopyAndAllowsRetry proves that a mid-copy
// failure leaves no partial destination (so incomplete output is never exposed
// as a completed file), preserves the source, and allows a clean retry.
func TestCopyNoReplaceCleansUpFailedCopyAndAllowsRetry(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	content := []byte("retry bytes")
	mustWriteFile(t, src, content, 0o640)
	sentinel := errors.New("mid-copy boom")

	err := copyNoReplaceWith(context.Background(), src, dst, func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if exists, _ := pathExists(dst); exists {
		t.Fatal("a failed exclusive copy must not leave a partial destination")
	}
	if exists, _ := pathExists(src); !exists {
		t.Fatal("source must be preserved after a failed copy")
	}

	if err := copyNoReplace(context.Background(), src, dst); err != nil {
		t.Fatalf("retry copyNoReplace: %v", err)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil || !bytes.Equal(got, content) {
		t.Fatalf("retried published = %q (%v), want %q", got, rerr, content)
	}
	if exists, _ := pathExists(src); exists {
		t.Fatal("source must be consumed after a successful retry")
	}
}

func TestFinalizeOutputAtomicExclusiveCopyFallbackPublishes(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := []byte("encoded output bytes")
	mustWriteFile(t, candidate, content, 0o640)
	destination := filepath.Join(base, "dest", "final.mkv")

	if err := finalizeOutputAtomicWith(context.Background(), candidate, destination, "job-1", nil, smbCommitFallback()); err != nil {
		t.Fatalf("finalizeOutputAtomicWith: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination = %q (%v), want %q", got, err, content)
	}
	if exists, _ := pathExists(PartialPathFor(destination, "job-1")); exists {
		t.Fatal("own partial must be consumed by the exclusive-copy commit")
	}
}

func TestFinalizeOutputAtomicExclusiveCopyFallbackNoClobberRace(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("new bytes"), 0o644)
	destination := filepath.Join(base, "dest", "final.mkv")
	partial := PartialPathFor(destination, "job-1")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("sentinel winner")

	err := finalizeOutputAtomicWith(context.Background(), candidate, destination, "job-1", func(p string) error {
		// A concurrent publisher wins the destination after our pre-check; the
		// exclusive create in the fallback must refuse to overwrite it.
		return os.WriteFile(destination, sentinel, 0o600)
	}, smbCommitFallback())
	if !IsDestinationExists(err) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	got, rerr := os.ReadFile(destination)
	if rerr != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("existing destination clobbered: %q (%v)", got, rerr)
	}
	if exists, _ := pathExists(partial); exists {
		t.Fatal("own partial must be cleaned after failed exclusive-copy commit")
	}
}
