package action

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// InvalidParamsError reports a request the caller can fix by changing one
// input. Every path that creates an action returns it before anything is
// written, so a bad submission neither leaves an action row behind nor binds
// its idempotency key to inputs that can never run.
//
// Error() is the reason alone: the step failures that preflight records keep
// their historical text, and the surfaces that return the error (MCP, HTTP)
// name Field themselves.
type InvalidParamsError struct {
	// Field is the input the reason is about, "" when no single input is.
	Field string
	// Reason says what is wrong and, where it helps, how to fix it.
	Reason string
}

func (e *InvalidParamsError) Error() string { return e.Reason }

// AsInvalidParams reports whether err is, or wraps, an InvalidParamsError.
func AsInvalidParams(err error) (*InvalidParamsError, bool) {
	var invalid *InvalidParamsError
	if errors.As(err, &invalid) {
		return invalid, true
	}
	return nil, false
}

// inputErr builds an InvalidParamsError for field.
func inputErr(field, format string, args ...any) error {
	return &InvalidParamsError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// attributeInput names field as the subject of err unless err already names
// one. A nil err stays nil.
func attributeInput(field string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := AsInvalidParams(err); ok {
		return err
	}
	return &InvalidParamsError{Field: field, Reason: err.Error()}
}

// firstPresent returns the first key that inputs sets to a non-nil value, or
// the first key when none is set.
func firstPresent(inputs map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := inputs[k]; ok && v != nil {
			return k
		}
	}
	return keys[0]
}

// field names the size budget input that selected the budget.
func (b *SizeBudgetPolicy) field() string {
	switch {
	case b == nil:
		return "size_budget_percent"
	case b.Siblings != nil:
		return "size_budget_source"
	case b.Bytes > 0:
		return "size_budget_bytes"
	}
	return "size_budget_percent"
}

// validateActionInputs runs the template's Validate hook, if it has one. Every
// path that creates an action calls it before it looks up or reserves an
// idempotency key, checks worker admission or writes anything. It returns the
// normalized inputs to persist: the hook's result, or inputs unchanged when
// the template has no hook or the hook returns nil.
func (e *Engine) validateActionInputs(ctx context.Context, tmpl ActionTemplate, inputs map[string]any) (map[string]any, error) {
	if tmpl.Validate == nil {
		return inputs, nil
	}
	normalized, err := tmpl.Validate(ctx, e, inputs)
	if err != nil {
		return nil, err
	}
	if normalized == nil {
		normalized = inputs
	}
	return normalized, nil
}

// ValidateInputs judges a request exactly as admission would, without
// admitting anything. A caller that has to do something before admission, such
// as check that the worker is ready, calls it first so a request the caller
// got wrong is reported as that and never as a worker problem. Names that are
// not templates are left for admission to refuse. A key that already holds an
// action with identical inputs is a replay and is not judged again, exactly as
// in admission.
func (e *Engine) ValidateInputs(ctx context.Context, name string, inputs map[string]any, key string) error {
	tmpl, ok := e.GetTemplate(name)
	if !ok {
		return nil
	}
	if key = strings.TrimSpace(key); key != "" && e.deps.Store != nil {
		if old, err := e.deps.Store.FindActionByIdempotencyKey(name, key); err == nil && old != nil {
			if matches, err := immutableActionInputsMatch(old.InputsJSON, inputs); err == nil && matches {
				return nil
			}
		}
	}
	_, err := e.validateActionInputs(ctx, tmpl, inputs)
	return err
}

// unsupportedInput rejects any input the template does not declare, so the
// catalog's required and optional lists are also the accepted set.
func unsupportedInput(tmpl ActionTemplate, inputs map[string]any, extra ...string) error {
	allowed := map[string]bool{"idempotency_key": true}
	for _, name := range tmpl.RequiredInputs {
		allowed[name] = true
	}
	for _, name := range tmpl.OptionalInputs {
		allowed[name] = true
	}
	for _, name := range extra {
		allowed[name] = true
	}
	// Report the alphabetically first unknown key, so the answer is stable.
	var unknown string
	for name := range inputs {
		if !allowed[name] && (unknown == "" || name < unknown) {
			unknown = name
		}
	}
	if unknown != "" {
		return inputErr(unknown, "unsupported input %q for %s; experimental encoder settings must be supplied inside strict profile_config", unknown, tmpl.Name)
	}
	return nil
}

// requiredInputs checks that every input the template lists as required is a
// non-blank string. All required inputs of the validated actions are strings.
func requiredInputs(tmpl ActionTemplate, inputs map[string]any) error {
	for _, name := range tmpl.RequiredInputs {
		raw, present := inputs[name]
		if !present || raw == nil {
			return inputErr(name, "%s is required", name)
		}
		s, ok := raw.(string)
		if !ok {
			return inputErr(name, "%s must be a string", name)
		}
		if strings.TrimSpace(s) == "" {
			return inputErr(name, "%s is required", name)
		}
	}
	return nil
}

// validBenchmarkMetric normalizes a caller-supplied metric.
func validBenchmarkMetric(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw != "auto" && raw != "vmaf" && raw != "ssim" && raw != "both" && raw != "vmaf+ssim" {
		return "", inputErr("metric", "invalid metric %q: must be 'auto', 'vmaf', 'ssim', or 'both' (fail closed)", raw)
	}
	if raw == "vmaf+ssim" {
		raw = "both"
	}
	return raw, nil
}

// transcodeRequest is what validating a transcode request teaches preflight,
// so it can act on the request without deciding anything about it again.
type transcodeRequest struct {
	// ProfileConfig is the decoded, normalized profile_config; set only when
	// HasProfileConfig.
	ProfileConfig    recipe.Profile
	ProfileDigest    string
	HasProfileConfig bool
	// PreserveBitDepth is preserve_source_bit_depth (default true).
	PreserveBitDepth bool
	// PinnedBitDepth is the output bit depth the request insists on: 8 or 10
	// when profile_config pins one and preserve_source_bit_depth is true,
	// otherwise 0. The source is then judged by one comparison against it.
	PinnedBitDepth int
}

// validateTranscodeRequest checks everything a transcode_media or
// benchmark_transcode request decides without reading the source. Preflight
// calls it again as its first step, so a request stored by an older version is
// judged by the same rules before any file is touched.
func (e *Engine) validateTranscodeRequest(name string, inputs map[string]any) (*transcodeRequest, error) {
	tmpl, ok := e.GetTemplate(name)
	if !ok {
		return nil, fmt.Errorf("unknown action template %q", name)
	}
	if err := unsupportedInput(tmpl, inputs); err != nil {
		return nil, err
	}
	if err := requiredInputs(tmpl, inputs); err != nil {
		return nil, err
	}
	if err := validateModeInputs(inputs); err != nil {
		return nil, err
	}
	req, err := validateTranscodeCommon(inputs)
	if err != nil {
		return nil, err
	}
	if e.deps.Fs != nil {
		raw := strings.TrimSpace(getString(inputs, "path"))
		if _, err := e.deps.Fs.ResolveRead(raw); err != nil {
			return nil, inputErr("path", "path %q is outside allowed read roots: %v", raw, err)
		}
	}
	return req, nil
}

// validateTranscodeCommon holds the rules every transcode request shares,
// single-file or batch: no destructive replacement, a decodable profile_config
// that does not mix with profile, a boolean preserve_source_bit_depth and a
// known metric.
func validateTranscodeCommon(inputs map[string]any) (*transcodeRequest, error) {
	if getBool(inputs, "replace_original") {
		return nil, inputErr("replace_original", "destructive replacement (replace_original: true) is not supported; transcoding is candidate-only and never modifies the original")
	}
	req := &transcodeRequest{}
	var err error
	req.ProfileConfig, req.ProfileDigest, req.HasProfileConfig, err = decodeEphemeralProfileInput(inputs)
	if err != nil {
		return nil, err
	}
	req.PreserveBitDepth, err = resolvePreserveSourceBitDepth(inputs)
	if err != nil {
		return nil, err
	}
	if req.HasProfileConfig && req.PreserveBitDepth {
		req.PinnedBitDepth = recipe.ExpectedBitDepth(req.ProfileConfig)
	}
	if metric := strings.TrimSpace(getString(inputs, "metric")); metric != "" {
		if _, err := validBenchmarkMetric(metric); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func validateTranscodeMedia(_ context.Context, e *Engine, inputs map[string]any) (map[string]any, error) {
	_, err := e.validateTranscodeRequest("transcode_media", inputs)
	return inputs, err
}

func validateBenchmarkTranscode(_ context.Context, e *Engine, inputs map[string]any) (map[string]any, error) {
	_, err := e.validateTranscodeRequest("benchmark_transcode", inputs)
	return inputs, err
}

// validateTranscodeBatch checks what a batch passes through to its children
// and what its first step would otherwise reject after admission. The set of
// files a Sonarr series resolves to is only known later, so only the inputs
// themselves are judged here.
func validateTranscodeBatch(_ context.Context, e *Engine, inputs map[string]any) (map[string]any, error) {
	tmpl, ok := e.GetTemplate("transcode_batch")
	if !ok {
		return nil, fmt.Errorf("unknown action template %q", "transcode_batch")
	}
	if err := unsupportedInput(tmpl, inputs); err != nil {
		return nil, err
	}
	if err := validateModeInputs(inputs); err != nil {
		return nil, err
	}
	if err := e.validateBatchPromotionInputs(inputs); err != nil {
		return nil, err
	}
	if _, err := validateTranscodeCommon(inputs); err != nil {
		return nil, err
	}
	if paths, present, err := batchPathsInput(inputs); err != nil {
		return nil, err
	} else if present && e.deps.Fs != nil {
		for _, p := range paths {
			if _, err := e.deps.Fs.ResolveRead(p); err != nil {
				return nil, inputErr("paths", "batch source path is outside allowed read roots: %v", err)
			}
		}
	}
	return inputs, nil
}

// podcastFrozenInputs are written by the podcast hook itself, so an admitted
// request replayed from its stored inputs must still be accepted.
var podcastFrozenInputs = []string{"podcast_policy_digest", "podcast_pipeline_version"}

// resolvePodcastPaths resolves and checks a podcast request's source and
// output paths. It does not touch the files, so admission and preflight share
// it.
func (e *Engine) resolvePodcastPaths(inputs map[string]any) (source, output string, err error) {
	source, err = e.deps.Fs.ResolveRead(getString(inputs, "path"))
	if err != nil {
		return "", "", inputErr("path", "%v", err)
	}
	output, err = e.deps.Fs.ResolveWrite(getString(inputs, "output_path"))
	if err != nil {
		return "", "", inputErr("output_path", "%v", err)
	}
	if _, err := e.deps.Fs.ResolveRead(output); err != nil {
		return "", "", inputErr("output_path", "output must also be in allowed_read_roots: %v", err)
	}
	if source == output || strings.ToLower(filepath.Ext(output)) != ".mp3" {
		return "", "", inputErr("output_path", "output must be a separate MP3 path")
	}
	return source, output, nil
}

// checkPodcastFields checks the required inputs of a podcast request and the
// format of source_sha256. Preflight repeats it on the stored inputs.
func checkPodcastFields(tmpl ActionTemplate, inputs map[string]any) error {
	if err := requiredInputs(tmpl, inputs); err != nil {
		return err
	}
	if raw := getString(inputs, "source_sha256"); raw != "" {
		if _, err := transcode.NormalizeSourceSHA256(raw); err != nil {
			return attributeInput("source_sha256", err)
		}
	}
	return nil
}

// validatePodcast checks a clean_podcast_ads request and freezes the podcast
// policy identity into the inputs that are stored.
func validatePodcast(_ context.Context, e *Engine, inputs map[string]any) (map[string]any, error) {
	tmpl, ok := e.GetTemplate("clean_podcast_ads")
	if !ok {
		return nil, fmt.Errorf("unknown action template %q", "clean_podcast_ads")
	}
	if err := unsupportedInput(tmpl, inputs, podcastFrozenInputs...); err != nil {
		return nil, err
	}
	if err := checkPodcastFields(tmpl, inputs); err != nil {
		return nil, err
	}
	if e.deps.Fs != nil {
		if _, _, err := e.resolvePodcastPaths(inputs); err != nil {
			return nil, err
		}
	}
	frozen := make(map[string]any, len(inputs)+2)
	for k, v := range inputs {
		frozen[k] = v
	}
	if err := e.freezePodcastInputs(frozen); err != nil {
		return nil, err
	}
	return frozen, nil
}
