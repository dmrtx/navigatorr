package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/internal/nashelper"
)

// promotionProofHelper marks a promotion whose move aside is proved by the NAS
// helper's file identity instead of the mount's fingerprints; see
// docs/nas-helper.md, "Stage 5 design".
const promotionProofHelper = "helper"

// helperProof is what the NAS helper said about a file right after a full read
// of it: its identity (ctime included) and the digest of its sampled ranges.
// Process stamps a persisted proof like promotionFingerprint.Process: a proof
// from an earlier process never proves a file, the file is read in full again.
type helperProof struct {
	Identity nashelper.Identity `json:"identity"`
	Sample   string             `json:"sample"`
	Process  string             `json:"process,omitempty"`
}

func (p helperProof) persisted() *helperProof {
	p.Process = promotionProcess
	return &p
}

func (p *helperProof) fromThisProcess() (helperProof, bool) {
	if p == nil || p.Process != promotionProcess {
		return helperProof{}, false
	}
	k := *p
	k.Process = ""
	return k, true
}

func (p *promotionState) helperProof() bool { return p.RecoveryProof == promotionProofHelper }

// sameHelperFile is the same inode under a rename: everything but ctime, which a
// rename moves. Both identities must be complete.
func sameHelperFile(a, b nashelper.Identity) bool {
	if !a.Complete || !b.Complete {
		return false
	}
	a.CtimeNs, b.CtimeNs = 0, 0
	return a == b
}

// sameHelperInode is the same file, size included, whatever its times say.
func sameHelperInode(a, b nashelper.Identity) bool {
	if !a.Complete || !b.Complete {
		return false
	}
	a.MtimeNs, b.MtimeNs, a.CtimeNs, b.CtimeNs = 0, 0, 0, 0
	return a == b
}

// promotionKnownHelper is the helper proof this execution's full hash of path to
// sha recorded, or nil when there was none or it was not complete.
func promotionKnownHelper(ctx context.Context, path, sha string) *helperProof {
	v := promotionVerified(ctx)
	if v == nil {
		return nil
	}
	known, ok := v.files[path]
	if !ok || known.sha != sha || known.helper == nil || !known.helper.Identity.Complete {
		return nil
	}
	return known.helper.persisted()
}

// promotionHelperSample asks the helper for the identity and sampled digest of
// path, or says why it cannot (no helper, no mapping, an open breaker, any error).
func (e *Engine) promotionHelperSample(ctx context.Context, path string) (nashelper.SampleResponse, error) {
	client := e.deps.NASHelper
	if client == nil {
		return nashelper.SampleResponse{}, errors.New("no NAS helper")
	}
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return nashelper.SampleResponse{}, err
	}
	if _, mapped := client.Remote(resolved); !mapped {
		return nashelper.SampleResponse{}, nashelper.ErrNotMapped
	}
	if e.helperBreaker.open(time.Now()) {
		return nashelper.SampleResponse{}, errors.New("the NAS helper was recently unreachable")
	}
	yieldReconcileSlot(ctx)
	res, err := client.Sample(ctx, resolved)
	if err != nil && errors.Is(err, nashelper.ErrUnavailable) && !errors.Is(err, nashelper.ErrNotMapped) {
		e.helperBreaker.trip(time.Now())
	}
	return res, err
}

// promotionVerifyByHelper re-proves that path still holds the bytes known
// describes, for the places where that cannot lose data (see
// promotionVerifyByIdentity): the helper's identity and sample now must equal the
// proof's, ctime included unless renamed allows it to have moved. Any doubt, a
// proof from an earlier process, or a helper that cannot answer reads the whole
// file, through the helper when it can. It returns the proof now holding, which
// is nil when the read did not go through the helper.
func (e *Engine) promotionVerifyByHelper(ctx context.Context, path, sha string, known *helperProof, renamed bool) (*helperProof, error) {
	if k, ok := known.fromThisProcess(); ok {
		cur, err := e.promotionHelperSample(ctx, path)
		switch {
		case err == nil && cur.Identity.Complete && sameHelperFile(cur.Identity, k.Identity) && cur.Digest == k.Sample && (renamed || cur.Identity.CtimeNs == k.Identity.CtimeNs):
			proof := helperProof{Identity: cur.Identity, Sample: cur.Digest}.persisted()
			promotionRecordRead(ctx, path, "nas_helper_identity", "", true)
			return proof, nil
		case err == nil && cur.Identity.Complete && sameHelperFile(cur.Identity, k.Identity) && cur.Digest != k.Sample:
			promotionRecordRead(ctx, path, "nas_helper_identity", "sample_changed", true)
		case err == nil:
			// The change time moved without a rename, or the inode is another:
			// something touched the file. Read it to know whether the bytes changed.
			promotionRecordRead(ctx, path, "nas_helper_identity", "identity_moved:"+helperIdentityDiff(k.Identity, cur.Identity), true)
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
	}
	return e.promotionReadInFull(ctx, path, sha)
}

// promotionReadInFull reads path in full and returns the helper proof of that
// read (nil when it did not go through the helper). Unlike
// verifyPromotionHashStable it never trusts this execution's cached fingerprint:
// the helper just said the file moved, and a mount whose attribute cache still
// shows the old fingerprint must not turn that into an unread acceptance.
func (e *Engine) promotionReadInFull(ctx context.Context, path, sha string) (*helperProof, error) {
	if v := promotionVerified(ctx); v != nil {
		delete(v.files, path)
	}
	actual, _, err := e.promotionHashStable(ctx, path)
	if err != nil {
		return nil, err
	}
	if actual != sha {
		return nil, fmt.Errorf("integrity violation: SHA-256 changed for %s", path)
	}
	return promotionKnownHelper(ctx, path, sha), nil
}

// Helper-observed probe of the recovery directory. It replaces the mount probe
// when the original has a complete helper identity: the question is the same, but
// the answer comes from the NAS filesystem and not from what the mount reports.

// promotionHelperProbes caches, per process and recovery directory, the probe's
// verdict and the identity of the directory's filesystem.
var (
	promotionHelperProbesMu sync.Mutex
	promotionHelperProbes   = map[string]helperProbeVerdict{}
)

type helperProbeVerdict struct {
	err error
	fs  nashelper.Identity
}

// promotionProbeHelper checks once per process and recovery directory that a file
// there keeps its inode through a no-replace rename, that a no-replace rename
// onto an existing file is refused, and that the change time moves when the bytes
// are rewritten in place with size and mtime restored; and, per promotion, that
// the original lives on the same filesystem and subvolume as the directory. A
// probe the helper could not run returns a non-definitive failure and is asked
// again; the caller then falls through to the mount probe.
func (e *Engine) promotionProbeHelper(ctx context.Context, dir string, original nashelper.Identity) error {
	if e.deps.NASHelper == nil {
		return &promotionProbeFailure{Step: probeStepCreate, Detail: "no NAS helper"}
	}
	key := filepath.Clean(filepath.Dir(dir))
	promotionHelperProbesMu.Lock()
	verdict, ok := promotionHelperProbes[key]
	promotionHelperProbesMu.Unlock()
	if !ok {
		if _, mapped := e.deps.NASHelper.Remote(dir); !mapped {
			return &promotionProbeFailure{Step: probeStepCreate, Detail: "the NAS helper does not map the recovery directory"}
		}
		if e.helperBreaker.open(time.Now()) {
			return &promotionProbeFailure{Step: probeStepCreate, Detail: "the NAS helper was recently unreachable"}
		}
		fs, err := e.probePromotionHelper(ctx, dir)
		verdict = helperProbeVerdict{err: err, fs: fs}
		var failure *promotionProbeFailure
		if err == nil || (errors.As(err, &failure) && failure.Definitive) {
			promotionHelperProbesMu.Lock()
			promotionHelperProbes[key] = verdict
			promotionHelperProbesMu.Unlock()
		}
	}
	if verdict.err != nil {
		return verdict.err
	}
	if verdict.fs.FSID != original.FSID || verdict.fs.Subvol != original.Subvol {
		return &promotionProbeFailure{Step: probeStepRename, Fs: verdict.fs.FSType, Detail: "the recovery directory is on another filesystem or subvolume than the original", Definitive: true}
	}
	return nil
}

func (e *Engine) helperIdentity(ctx context.Context, path string) (nashelper.Identity, error) {
	client := e.deps.NASHelper
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return nashelper.Identity{}, err
	}
	if _, mapped := client.Remote(resolved); !mapped {
		return nashelper.Identity{}, nashelper.ErrNotMapped
	}
	res, err := client.Identity(ctx, resolved)
	if err != nil {
		return nashelper.Identity{}, err
	}
	return res.Identity, nil
}

// helperProbeQuiet is how long a probe file's identity must stay unchanged before
// it counts as settled; helperProbePoll is how often it is read.
var (
	helperProbeQuiet = 3 * time.Second
	helperProbePoll  = 200 * time.Millisecond
	// helperProbeSettleLimit bounds the wait for a file whose times never settle.
	helperProbeSettleLimit = 15 * time.Second
)

// helperIdentityQuiet is helperIdentity once the file's identity has stopped
// changing for helperProbeQuiet. A file whose times keep moving is not settled
// and says nothing yet about the filesystem.
func (e *Engine) helperIdentityQuiet(ctx context.Context, path string) (nashelper.Identity, error) {
	cur, err := e.helperIdentity(ctx, path)
	if err != nil {
		return nashelper.Identity{}, err
	}
	start := time.Now()
	stableSince := start
	for time.Since(stableSince) < helperProbeQuiet {
		if time.Since(start) > helperProbeSettleLimit {
			return nashelper.Identity{}, errors.New("the probe file's times kept changing after it was written")
		}
		select {
		case <-ctx.Done():
			return nashelper.Identity{}, ctx.Err()
		case <-time.After(helperProbePoll):
		}
		next, err := e.helperIdentity(ctx, path)
		if err != nil {
			return nashelper.Identity{}, err
		}
		if next != cur {
			cur, stableSince = next, time.Now()
		}
	}
	return cur, nil
}

func (e *Engine) probePromotionHelper(ctx context.Context, dir string) (nashelper.Identity, error) {
	fs := promotionFilesystemName(dir)
	fail := func(step, detail string, err error, definitive bool) error {
		return &promotionProbeFailure{Step: step, Fs: fs, Detail: detail, Err: err, Definitive: definitive}
	}
	f, err := os.CreateTemp(dir, ".helper-probe-")
	if err != nil {
		return nashelper.Identity{}, fail(probeStepCreate, "", err, false)
	}
	from, to, other := f.Name(), f.Name()+".moved", f.Name()+".other"
	defer os.Remove(from)
	defer os.Remove(to)
	defer os.Remove(other)
	content := []byte("navigatorr move-aside probe 0")
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nashelper.Identity{}, fail(probeStepCreate, "", err, false)
	}
	// The file was just written over the mount. Samba applies a write's
	// timestamps up to a couple of seconds after the handle closes, which is not
	// a rename changing anything: wait until they stop moving.
	created, err := e.helperIdentityQuiet(ctx, from)
	if err != nil {
		return nashelper.Identity{}, fail(probeStepStat, "", err, false)
	}
	if !created.Complete {
		return nashelper.Identity{}, fail(probeStepStat, "the helper's identity is not complete here", nil, true)
	}
	if err := renamePromotionNoReplace(from, to); err != nil {
		return nashelper.Identity{}, fail(probeStepRename, "", err, promotionCopyUnsupported(err))
	}
	// A rename never touches the modification time, so what is compared is the
	// file itself; the identity is read once its times have settled again, and
	// becomes the baseline of the change-time check below.
	moved, err := e.helperIdentityQuiet(ctx, to)
	if err != nil {
		return nashelper.Identity{}, fail(probeStepStat, "", err, false)
	}
	if !sameHelperInode(created, moved) {
		return nashelper.Identity{}, fail(probeStepRenameInode, "changed: "+helperIdentityDiff(created, moved), nil, true)
	}
	// A no-replace rename onto an existing file must be refused: restoring the
	// original relies on it.
	if err := os.WriteFile(other, []byte("occupied"), 0600); err != nil {
		return nashelper.Identity{}, fail(probeStepRename, "", err, false)
	}
	if err := renamePromotionNoReplace(to, other); err == nil {
		return nashelper.Identity{}, fail(probeStepRename, "a no-replace rename replaced an existing file", nil, true)
	} else if !errors.Is(err, os.ErrExist) {
		return nashelper.Identity{}, fail(probeStepRename, "a no-replace rename onto an existing file failed unexpectedly", err, false)
	}
	mtime := time.Unix(0, moved.MtimeNs)
	const attempts = 150
	var rewritten nashelper.Identity
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nashelper.Identity{}, fail(probeStepRewrite, "", err, false)
		}
		content[len(content)-1] = byte('1' + i%9)
		if err := os.WriteFile(to, content, 0600); err != nil {
			return nashelper.Identity{}, fail(probeStepRewrite, "", err, false)
		}
		if err := os.Chtimes(to, mtime, mtime); err != nil {
			return nashelper.Identity{}, fail(probeStepRewrite, "", err, false)
		}
		if rewritten, err = e.helperIdentity(ctx, to); err != nil {
			return nashelper.Identity{}, fail(probeStepStat, "", err, false)
		}
		if rewritten.Ino != moved.Ino || rewritten.Generation != moved.Generation {
			return nashelper.Identity{}, fail(probeStepRewriteInode, "changed: "+helperIdentityDiff(moved, rewritten), nil, true)
		}
		if rewritten.CtimeNs != moved.CtimeNs {
			return moved, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nashelper.Identity{}, fail(probeStepCtime, fmt.Sprintf("after %d rewrites; mtime restored: %t", attempts, rewritten.MtimeNs == moved.MtimeNs), nil, true)
}

// helperIdentityDiff names the fields that differ, never their values.
func helperIdentityDiff(a, b nashelper.Identity) string {
	var fields []string
	for _, c := range []struct {
		name string
		diff bool
	}{
		{"fsid", a.FSID != b.FSID}, {"subvol", a.Subvol != b.Subvol}, {"generation", a.Generation != b.Generation},
		{"ino", a.Ino != b.Ino}, {"size", a.Size != b.Size}, {"mtime", a.MtimeNs != b.MtimeNs}, {"ctime", a.CtimeNs != b.CtimeNs},
	} {
		if c.diff {
			fields = append(fields, c.name)
		}
	}
	if len(fields) == 0 {
		return "nothing"
	}
	return strings.Join(fields, ",")
}

func logHelperProbe(service string, err error) {
	log.Printf("promotion %s: the NAS helper's probe of the recovery directory failed (%s); not moving the original aside by helper proof", service, promotionProbeDetail(err))
}
