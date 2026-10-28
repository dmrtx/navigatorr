package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// goldenDigest* were computed by DigestTranscodeExecutionSpecWithSourceSHA
// before publish_policy existed. Running jobs persist these digests, so an
// on_encode (or omitted) policy must keep reproducing them byte for byte.
const (
	goldenDigestNoSourceSHA = "sha256:ecdf89804f63561348cede2697021002f1efde548dd6e6401d01b728cc4d0d77"
	goldenDigestWithSHA     = "sha256:898d2356054edd9db55e281d22f3be4bd837ae2ba3046fb134920e9ec8962bd5"
)

func TestPublishPolicyDigestRegression(t *testing.T) {
	plan := canonicalTestPlan(t)
	sha := "ab" + strings.Repeat("0123456789abcdef", 4)[:62]
	tests := []struct {
		name      string
		sourceSHA string
		policy    string
		golden    string
	}{
		{"omitted", "", "", goldenDigestNoSourceSHA},
		{"explicit on_encode", "", PublishPolicyOnEncode, goldenDigestNoSourceSHA},
		{"omitted with source sha", sha, "", goldenDigestWithSHA},
		{"explicit on_encode with source sha", sha, PublishPolicyOnEncode, goldenDigestWithSHA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DigestTranscodeExecutionSpecWithPublishPolicy("/media/a/src.mkv", "/media/b/out.mkv", "hevc-vt", tc.sourceSHA, tc.policy, plan)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.golden {
				t.Fatalf("digest = %s, want the pre-policy digest %s", got, tc.golden)
			}
			legacy, err := DigestTranscodeExecutionSpecWithSourceSHA("/media/a/src.mkv", "/media/b/out.mkv", "hevc-vt", tc.sourceSHA, plan)
			if err != nil || legacy != tc.golden {
				t.Fatalf("legacy entry point digest = %s err=%v, want %s", legacy, err, tc.golden)
			}
		})
	}

	t.Run("on_approval differs and is stable", func(t *testing.T) {
		a, err := DigestTranscodeExecutionSpecWithPublishPolicy("/media/a/src.mkv", "/media/b/out.mkv", "hevc-vt", "", PublishPolicyOnApproval, plan)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := DigestTranscodeExecutionSpecWithPublishPolicy("/media/a/src.mkv", "/media/b/out.mkv", "hevc-vt", "", PublishPolicyOnApproval, plan)
		if a != b || a == goldenDigestNoSourceSHA {
			t.Fatalf("on_approval digest %s must be stable and differ from %s", a, goldenDigestNoSourceSHA)
		}
	})
	t.Run("unknown policy fails closed", func(t *testing.T) {
		if _, err := DigestTranscodeExecutionSpecWithPublishPolicy("/a", "/b", "hevc-vt", "", "sometimes", plan); err == nil {
			t.Fatal("an unknown policy must not hash")
		}
	})
}

func TestNormalizePublishPolicy(t *testing.T) {
	tests := []struct {
		raw, want string
		wantErr   bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"on_encode", "", false},
		{" ON_APPROVAL ", PublishPolicyOnApproval, false},
		{"on_approval", PublishPolicyOnApproval, false},
		{"later", "", true},
	}
	for _, tc := range tests {
		got, err := NormalizePublishPolicy(tc.raw)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("NormalizePublishPolicy(%q) = %q, %v; want %q, err=%v", tc.raw, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestRequestOmitsPublishPolicyWhenUnset(t *testing.T) {
	raw, err := json.Marshal(Request{ID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "publish_policy") {
		t.Fatalf("an unset policy must not appear on the wire: %s", raw)
	}
}

func TestDeferredPublicationCapabilityFingerprint(t *testing.T) {
	base := WorkerCapabilities{}
	without, err := ComputeCapabilityFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	// A machine that does not advertise the capability keeps its fingerprint,
	// so shipping the field moves nothing until retention is enabled.
	off := base
	off.DeferredPublication = false
	if again, _ := ComputeCapabilityFingerprint(off); again != without {
		t.Fatalf("fingerprint moved without the capability: %s != %s", again, without)
	}
	on := base
	on.DeferredPublication = true
	with, err := ComputeCapabilityFingerprint(on)
	if err != nil {
		t.Fatal(err)
	}
	if with == without {
		t.Fatal("advertising deferred_publication must change the fingerprint")
	}
	raw, _ := json.Marshal(base)
	if strings.Contains(string(raw), "deferred_publication") {
		t.Fatalf("an unadvertised capability must be omitted: %s", raw)
	}
	raw, _ = json.Marshal(on)
	if !strings.Contains(string(raw), `"deferred_publication":true`) {
		t.Fatalf("the capability must be on the wire when advertised: %s", raw)
	}
	if strings.Contains(string(raw), "start_retention_admission") {
		t.Fatalf("an unadvertised capability must be omitted: %s", raw)
	}
	atStart := on
	atStart.StartRetentionAdmission = true
	if fp, _ := ComputeCapabilityFingerprint(atStart); fp == with {
		t.Fatal("advertising start_retention_admission must change the fingerprint")
	}
	raw, _ = json.Marshal(atStart)
	if !strings.Contains(string(raw), `"start_retention_admission":true`) {
		t.Fatalf("the capability must be on the wire when advertised: %s", raw)
	}
}
