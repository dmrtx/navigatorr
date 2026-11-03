package transcodeworker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func newBudgetWorker(t *testing.T, maxJobs int) *Worker {
	t.Helper()
	return &Worker{cfg: &WorkerConfig{MaxParallelJobs: maxJobs, StateDir: t.TempDir()}}
}

// A core budget sizes x265 from the budget, not from every CPU, and keeps a
// single encode inside it too.
func TestCoreBudgetSizesX265FromTheBudget(t *testing.T) {
	withHostCPUs(t, 10)
	inUse := 1
	w := newBudgetWorker(t, 2)
	w.videoInUseFn = func() (int, bool) { return inUse, true }
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("no budget: a lone job keeps x265's default, got %q", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: 6}); err != nil {
		t.Fatal(err)
	}
	if got := w.x265ThreadParams(); got != "pools=6:frame-threads=3" {
		t.Errorf("a lone job takes the whole budget: %q", got)
	}
	inUse = 2
	if got := w.x265ThreadParams(); got != "pools=4:frame-threads=3" {
		t.Errorf("two jobs get 6/2=3 threads, raised to the %d floor: %q", minX265PoolThreads, got)
	}
	// An explicit x265_pools above the budget share is brought down to it.
	w.cfg.X265Pools = 8
	inUse = 1
	if got := w.x265ThreadParams(); got != "pools=6:frame-threads=3" {
		t.Errorf("explicit pools over the budget: %q", got)
	}
	// And one below the share stays what the operator asked for.
	w.cfg.X265Pools = 5
	if got := w.x265ThreadParams(); got != "pools=5:frame-threads=3" {
		t.Errorf("explicit pools under the budget: %q", got)
	}
	w.cfg.X265Pools = -1
	if got := w.x265ThreadParams(); got != "" {
		t.Errorf("x265_pools -1 still turns partitioning off: %q", got)
	}
}

// The self-check proves each partition the budget can hand out, including the
// lone-encode one.
func TestCoreBudgetPartitionCandidatesIncludeALoneEncode(t *testing.T) {
	withHostCPUs(t, 12)
	w := newBudgetWorker(t, 3)
	if got := w.x265PartitionCandidates(); strings.Join(got, ",") != "pools=6:frame-threads=3,pools=4:frame-threads=3" {
		t.Fatalf("no budget: %v", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: 8}); err != nil {
		t.Fatal(err)
	}
	if got := w.x265PartitionCandidates(); strings.Join(got, ",") != "pools=8:frame-threads=3,pools=4:frame-threads=3" {
		t.Fatalf("budget 8 on 12 CPUs: %v", got)
	}
	if key := w.x265PartitionKey(); key != "pools=8:frame-threads=3,pools=4:frame-threads=3" {
		t.Fatalf("the proved set must follow the budget: %q", key)
	}
}

func TestCoreBudgetIsValidatedStoredAndReported(t *testing.T) {
	withHostCPUs(t, 10)
	w := newBudgetWorker(t, 2)
	for name, budget := range map[string]int{"below the x265 floor": 3, "not below the host": 10, "above the host": 16, "negative": -1} {
		if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: budget}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(w.slotSplitPath()); !os.IsNotExist(err) {
		t.Fatal("a rejected budget was stored")
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: 6}); err != nil {
		t.Fatal(err)
	}
	status := w.slotsStatus()
	if !status.CoreBudgetCapable() || status.Cores() != 6 {
		t.Fatalf("budget not reported: %+v", status.CoreBudget)
	}
	other := NewWorker(&WorkerConfig{StateDir: w.cfg.StateDir, MaxParallelJobs: 2})
	if got := other.slotLimits().CoreBudget; got != 6 {
		t.Fatalf("another process missed the budget: %d", got)
	}
	// A budget alone keeps the file; clearing everything removes it.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{}); err != nil {
		t.Fatal(err)
	}
	if got := w.slotsStatus(); !got.CoreBudgetCapable() || got.Cores() != 0 {
		t.Fatalf("cleared budget: %+v", got.CoreBudget)
	}
	if _, err := os.Stat(w.slotSplitPath()); !os.IsNotExist(err) {
		t.Fatal("a cleared record was kept")
	}
}

// A stored budget the host no longer fits (fewer CPUs after a move) is
// ignored rather than applied.
func TestCoreBudgetThatNoLongerFitsTheHostIsIgnored(t *testing.T) {
	withHostCPUs(t, 10)
	w := newBudgetWorker(t, 2)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: 8}); err != nil {
		t.Fatal(err)
	}
	withHostCPUs(t, 8)
	if got := w.slotLimits().CoreBudget; got != 0 {
		t.Fatalf("budget %d on 8 CPUs must be ignored", got)
	}
	if got := w.cpuBudget(); got != 8 {
		t.Fatalf("cpu budget without a limit is every CPU: %d", got)
	}
}

func TestCoreBudgetLimitsQualityCheckThreads(t *testing.T) {
	withHostCPUs(t, 10)
	w := newBudgetWorker(t, 2)
	w.videoInUseFn = func() (int, bool) { return 1, true }
	if got := w.metricThreads(2); got != 5 {
		t.Fatalf("no budget: %d", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: 6}); err != nil {
		t.Fatal(err)
	}
	if got := w.metricThreads(2); got != 3 {
		t.Errorf("a lone job splits 6 cores over 2 metrics: %d threads", got)
	}
	if got := w.finalValidationConcurrency(6); got != 3 {
		t.Errorf("6 cores at 2 threads a window: %d windows", got)
	}
}

func TestSlotSplitWarnsWhenTheBudgetCannotGiveEveryEncodeItsFloor(t *testing.T) {
	warnings, err := transcode.ValidateSlotSplit(transcode.SlotSplit{CoreBudget: 6}, 3, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "6 cores") {
		t.Fatalf("warnings: %v", warnings)
	}
	if warnings, _ := transcode.ValidateSlotSplit(transcode.SlotSplit{CoreBudget: 8}, 2, 12); len(warnings) != 0 {
		t.Fatalf("two encodes fit 8 cores: %v", warnings)
	}
	if warnings, _ := transcode.ValidateSlotSplit(transcode.SlotSplit{CoreBudget: 5}, 1, 12); len(warnings) != 0 {
		t.Fatalf("one encode needs no split: %v", warnings)
	}
}

// With a budget the job process, and so everything it runs, starts at a lower
// priority; without one it is left alone.
func TestJobProcessRunsAtLowerPriorityUnderACoreBudget(t *testing.T) {
	withHostCPUs(t, 10)
	exe := filepath.Join(t.TempDir(), "runner.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nice := func(w *Worker) string {
		t.Helper()
		pid, _, err := w.spawnTranscodeProcess(exe, "", "job-1")
		if err != nil {
			t.Fatal(err)
		}
		defer killTranscodeProcess(pid)
		deadline := time.Now().Add(2 * time.Second)
		for {
			out, err := exec.Command("ps", "-o", "nice=", "-p", strconv.Itoa(pid)).Output()
			if err == nil && strings.TrimSpace(string(out)) != "" {
				return strings.TrimSpace(string(out))
			}
			if time.Now().After(deadline) {
				t.Fatalf("no priority for pid %d: %v", pid, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	w := newBudgetWorker(t, 2)
	if got := nice(w); got != "0" {
		t.Fatalf("no budget: nice %s", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{CoreBudget: 6}); err != nil {
		t.Fatal(err)
	}
	if got := nice(w); got != strconv.Itoa(coreBudgetNice) {
		t.Fatalf("budget: nice %s, want %d", got, coreBudgetNice)
	}
}
