package transcode

import (
	"fmt"
	"strings"
)

// Publish policy: when a worker copies a finished, validated candidate to the
// media share.
const (
	// PublishPolicyOnEncode publishes the candidate as soon as it validates. It
	// is the behaviour of every job that carries no policy, and the policy is
	// never sent for it.
	PublishPolicyOnEncode = "on_encode"
	// PublishPolicyOnApproval keeps the validated candidate on the worker's
	// local disk until the coordinator asks for it to be published (see
	// docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md).
	PublishPolicyOnApproval = "on_approval"

	// PublishFallbackRetentionFull is the publish fallback of an on_approval
	// job that publishes on encode because the worker had no room left in its
	// retention budget.
	PublishFallbackRetentionFull = "retention_full"
)

// NormalizePublishPolicy returns the canonical policy a job carries: empty for
// on_encode (the default, omitted from the request, the job record and the
// execution-spec digest), PublishPolicyOnApproval for deferred publication. An
// unknown value is an error, never a silent default.
func NormalizePublishPolicy(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", PublishPolicyOnEncode:
		return "", nil
	case PublishPolicyOnApproval:
		return PublishPolicyOnApproval, nil
	default:
		return "", fmt.Errorf("unknown publish_policy %q (want %s|%s)", raw, PublishPolicyOnEncode, PublishPolicyOnApproval)
	}
}
