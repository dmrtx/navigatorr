package transcodeworker

import (
	"fmt"
	"runtime"
	"strings"
)

// hostCPUCount is the logical CPU count x265 sizes its default thread pool
// from. A variable so tests can pin a host shape.
var hostCPUCount = runtime.NumCPU

// minX265PoolThreads is the smallest pool the worker will hand x265. Smaller
// pools change x265's lookahead scheduling and with it the bitstream (seen
// with pools=1 and pools=2 on x265 4.3); 4 and above encode frame-identical
// output to the unpartitioned default.
const minX265PoolThreads = 4

// x265ThreadKeys are the scheduling-only x265 params the worker adds. They
// never come from a plan, so they are not part of the plan digest.
var x265ThreadKeys = []string{"pools", "frame-threads"}

// x265AutoFrameThreads is the frame-thread count x265 picks by itself for a
// wavefront encode on cpus logical CPUs (ThreadPool::getFrameThreadsCount).
// Frame threads change the bitstream, and x265 derives them from the pool
// size, so a partitioned pool must pin the count the whole host would get.
// Zero means the count depends on more than the CPU count (32+ CPUs also look
// at the source height), so the worker does not partition there.
func x265AutoFrameThreads(cpus int) int {
	switch {
	case cpus >= 32:
		return 0
	case cpus >= 16:
		return 4
	case cpus >= 8:
		return 3
	case cpus >= 4:
		return 2
	default:
		return 1
	}
}

// x265ThreadParamsFor returns the x265-params that give one job a pool of
// pools threads on a cpus-CPU host while keeping the encoded bitstream
// identical to an unpartitioned encode, or "" when partitioning is not
// possible or not needed.
func x265ThreadParamsFor(cpus, pools int) string {
	if pools < minX265PoolThreads || pools >= cpus {
		return ""
	}
	frameThreads := x265AutoFrameThreads(cpus)
	if frameThreads == 0 {
		return ""
	}
	return fmt.Sprintf("pools=%d:frame-threads=%d", pools, frameThreads)
}

// x265PoolsWarning explains why an explicit x265_pools would not partition
// anything on a cpus-CPU host, or returns "". Such a value is not an error (the
// encodes are simply unpartitioned), but it silently does not do what it says.
func x265PoolsWarning(pools, cpus int) string {
	switch {
	case pools <= 0:
		return ""
	case pools < minX265PoolThreads:
		return fmt.Sprintf("x265_pools %d is below %d and is ignored: smaller pools change the encoded bitstream, so encodes run unpartitioned", pools, minX265PoolThreads)
	case pools >= cpus:
		return fmt.Sprintf("x265_pools %d is not below the %d host CPUs and is ignored: encodes run unpartitioned", pools, cpus)
	case x265AutoFrameThreads(cpus) == 0:
		return fmt.Sprintf("x265_pools %d is ignored: partitioning is not applied on hosts with %d CPUs", pools, cpus)
	}
	return ""
}

// x265ThreadParams is the partition the worker's encodes run with: the
// configured one (configuredX265ThreadParams) unless the startup self-check
// found it changes the encoded frames, which turns it off.
func (w *Worker) x265ThreadParams() string {
	params := w.configuredX265ThreadParams()
	if params == "" || w.x265PartitionDisabled(params) {
		return ""
	}
	return params
}

// configuredX265ThreadParams partitions x265 thread pools between parallel
// jobs: x265_pools wins when positive, -1 disables partitioning, and 0 splits
// the host CPUs evenly across max_parallel_jobs.
func (w *Worker) configuredX265ThreadParams() string {
	if w == nil || w.cfg == nil || w.cfg.X265Pools < 0 {
		return ""
	}
	cpus := hostCPUCount()
	pools := w.cfg.X265Pools
	if pools == 0 {
		jobs := max(1, w.cfg.MaxParallelJobs)
		if jobs == 1 {
			return ""
		}
		pools = cpus / jobs
	}
	return x265ThreadParamsFor(cpus, pools)
}

// withX265ThreadParams merges scheduling params into a libx265 invocation:
// into its -x265-params when present (keys it already sets are kept, never
// duplicated), else as a new -x265-params right after the encoder selection.
// Non-libx265 invocations are returned unchanged.
func withX265ThreadParams(args []string, params string) []string {
	if params == "" {
		return args
	}
	encoder := -1
	for i := 0; i+1 < len(args); i++ {
		if (args[i] == "-c:v" || args[i] == "-vcodec" || args[i] == "-codec:v") && args[i+1] == "libx265" {
			encoder = i + 1
			break
		}
	}
	if encoder < 0 {
		return args
	}
	out := append([]string(nil), args...)
	for i := encoder; i+1 < len(out); i++ {
		if out[i] == "-x265-params" {
			out[i+1] = mergeX265ParamString(out[i+1], params)
			return out
		}
	}
	tail := append([]string{"-x265-params", params}, out[encoder+1:]...)
	return append(out[:encoder+1], tail...)
}

// mergeX265ParamString appends each key=value of extra whose key base does
// not already set.
func mergeX265ParamString(base, extra string) string {
	have := map[string]bool{}
	for _, kv := range strings.Split(base, ":") {
		if k, _, _ := strings.Cut(kv, "="); k != "" {
			have[x265ParamKey(k)] = true
		}
	}
	merged := base
	for _, kv := range strings.Split(extra, ":") {
		k, _, _ := strings.Cut(kv, "=")
		if k == "" || have[x265ParamKey(k)] {
			continue
		}
		have[x265ParamKey(k)] = true
		if merged == "" {
			merged = kv
		} else {
			merged += ":" + kv
		}
	}
	return merged
}

// x265ParamKey folds x265's aliases for the keys the worker sets.
func x265ParamKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	switch k {
	case "numa-pools":
		return "pools"
	case "f":
		return "frame-threads"
	}
	return k
}

// x265ThreadParamsOf extracts the worker's scheduling params from an
// invocation, so a derived invocation (the first pass of a two-pass encode)
// runs with the same partition as the one it was derived from.
func x265ThreadParamsOf(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-x265-params" {
			continue
		}
		var keep []string
		for _, kv := range strings.Split(args[i+1], ":") {
			k, _, _ := strings.Cut(kv, "=")
			for _, want := range x265ThreadKeys {
				if x265ParamKey(k) == want {
					keep = append(keep, kv)
				}
			}
		}
		return strings.Join(keep, ":")
	}
	return ""
}
