// Package nashelper is a read-only service for the machine that holds the media
// library. It reports what the library's own filesystem says about a file and
// reads files from local disk, so the coordinator need not pull a file across a
// network mount just to learn its hash or whether it changed.
//
// It never writes, renames or deletes, and it is never given a value to compare
// with: it measures, and the caller decides.
package nashelper

import "errors"

// Version identifies the API and the build in every health answer.
const Version = "0.1.0"

// Identity is what the library's filesystem reports for an open file. Numbers
// are strings in JSON because durable state loses precision on large integers.
// Subvol and Generation are empty where the filesystem has no such notion.
// Complete says the helper could read everything this filesystem offers: it is
// false when the generation (or, on btrfs, the subvolume) is unavailable, and a
// caller that needs them to tell a file from a later one reusing its inode must
// check it.
type Identity struct {
	FSID       string `json:"fsid"`
	FSType     string `json:"fs_type"`
	Subvol     string `json:"subvol,omitempty"`
	Generation string `json:"generation,omitempty"`
	Ino        uint64 `json:"ino,string"`
	Size       int64  `json:"size,string"`
	MtimeNs    int64  `json:"mtime_ns,string"`
	CtimeNs    int64  `json:"ctime_ns,string"`
	Complete   bool   `json:"complete"`
}

// Health is the answer to GET /v1/health.
type Health struct {
	Version    string   `json:"version"`
	InstanceID string   `json:"instance_id"`
	Roots      []string `json:"roots"`
	// Resolver is how paths are resolved under a root: "openat2" or "walk".
	Resolver string `json:"resolver"`
}

// PathRequest names one file; it is the whole request body of identity, hash
// and sample. Unknown fields are rejected, so a caller cannot hand the helper
// an expected value and have it echoed back as a verdict.
type PathRequest struct {
	Path string `json:"path"`
}

// IdentityResponse is the answer to POST /v1/identity.
type IdentityResponse struct {
	InstanceID string   `json:"instance_id"`
	Identity   Identity `json:"identity"`
}

// SampleResponse is the answer to POST /v1/sample.
type SampleResponse struct {
	InstanceID string   `json:"instance_id"`
	Digest     string   `json:"digest"`
	Identity   Identity `json:"identity"`
}

// Job states.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// HashResult is a finished hash: the digest of exactly Bytes bytes, taken while
// Identity held steady.
type HashResult struct {
	SHA256   string   `json:"sha256"`
	Bytes    int64    `json:"bytes,string"`
	Identity Identity `json:"identity"`
}

// Job is the answer to POST /v1/hash (202) and GET /v1/jobs/{id}.
type Job struct {
	ID         string      `json:"id"`
	InstanceID string      `json:"instance_id"`
	State      string      `json:"state"`
	BytesRead  int64       `json:"bytes_read,string"`
	TotalBytes int64       `json:"total_bytes,string"`
	Result     *HashResult `json:"result,omitempty"`
	// Usage is the result of a usage job; BytesRead counts bytes so far.
	Usage *UsageResult `json:"usage,omitempty"`
	Error *Error       `json:"error,omitempty"`
}

// Volume is what the filesystem under a served root reports about its space.
// It is the whole volume, whatever the root holds, so it is the number to set
// the folders against.
type Volume struct {
	Root       string `json:"root"`
	FSID       string `json:"fsid"`
	FSType     string `json:"fs_type"`
	TotalBytes int64  `json:"total_bytes,string"`
	// FreeBytes includes space reserved for privileged use; AvailBytes is what
	// an ordinary writer can still use.
	FreeBytes  int64 `json:"free_bytes,string"`
	AvailBytes int64 `json:"avail_bytes,string"`
}

// VolumeResponse is the answer to GET /v1/volumes.
type VolumeResponse struct {
	InstanceID string   `json:"instance_id"`
	Volumes    []Volume `json:"volumes"`
}

// UsageResult is the size of a directory tree: the apparent size of its regular
// files, as ls and the coordinator's own folder scan count it. Links are not
// followed, a directory on another filesystem is not entered, and Skipped counts
// what could not be read or was left out for either reason.
//
// Stamps holds the modification time, in whole seconds, of every directory in the
// tree (the root is "."), so a caller can tell later whether anything was added,
// removed or renamed without walking it again. A tree with more than
// MaxUsageStamps directories carries none.
type UsageResult struct {
	Bytes   int64            `json:"bytes,string"`
	Files   int64            `json:"files,string"`
	Dirs    int64            `json:"dirs,string"`
	Skipped int64            `json:"skipped,string"`
	Stamps  map[string]int64 `json:"stamps,omitempty"`
	// Children holds the size of each folder down to the depth asked for, so one
	// walk answers for the folders inside as well. It is absent when none was
	// asked for, and stops short when the answer would grow too large: the folders
	// left out are measured on their own.
	Children []UsageChild `json:"children,omitempty"`
}

// MaxUsageStamps bounds how many directories a usage result stamps.
const MaxUsageStamps = 1024

// MaxListEntries bounds one listing, which the client reads whole. A folder with
// more entries is refused, and the caller lists it through the mount.
const MaxListEntries = 5000

// Entry types in a ListEntry. Only what the entry itself is: a link is reported
// as a link and never followed.
const (
	EntryFile    = "file"
	EntryDir     = "dir"
	EntrySymlink = "symlink"
	EntryOther   = "other"
)

// ListEntry is one name in a directory as the library's own filesystem
// reports it: type, apparent size and modification time. The size is the
// entry's own (a directory's is not the size of what is inside it).
type ListEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Size    int64  `json:"size,string"`
	MtimeNs int64  `json:"mtime_ns,string"`
}

// ListResponse is the answer to POST /v1/list: every entry of one directory in
// one reply, taken with local reads instead of one round trip per entry over a
// network mount. DirMtimeNs is the directory's own modification time taken
// before its entries were read, which moves when an entry is added, removed or
// renamed (not when a file's contents change).
type ListResponse struct {
	InstanceID string      `json:"instance_id"`
	DirMtimeNs int64       `json:"dir_mtime_ns,string"`
	Entries    []ListEntry `json:"entries"`
}

// Error codes.
const (
	CodeNotFound  = "not_found"
	CodeOutside   = "outside_roots"
	CodeNotFile   = "not_regular_file"
	CodeNotDir    = "not_directory"
	CodeChanged   = "changed_while_reading"
	CodeDenied    = "permission_denied"
	CodeBusy      = "busy"
	CodeCancelled = "cancelled"
	CodeBadInput  = "bad_request"
	CodeAuth      = "unauthorized"
	CodeInternal  = "internal"
)

// Error is the body of every failure and the error of a failed job.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

var (
	errNotFound = &Error{Code: CodeNotFound, Message: "no such file"}
	errOutside  = &Error{Code: CodeOutside, Message: "path is outside the served roots or crosses a symbolic link"}
	errNotFile  = &Error{Code: CodeNotFile, Message: "path is not a regular file"}
	errNotDir   = &Error{Code: CodeNotDir, Message: "path is not a directory"}
	errDenied   = &Error{Code: CodeDenied, Message: "permission denied"}
	errTooMany  = &Error{Code: CodeBadInput, Message: "the directory has more entries than one listing carries"}
	errBadName  = &Error{Code: CodeBadInput, Message: "an entry name is not valid UTF-8, which a JSON listing cannot carry"}
	errChanged  = &Error{Code: CodeChanged, Message: "the file changed while it was read"}
)

func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}
