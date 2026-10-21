package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/jakenesler/navigatorr/transcode/selector"
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
	if name == removedBenchmarkAction {
		return inputErr("", removedBenchmarkMessage)
	}
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
	if err := unsupportedInput(tmpl, inputs, searchOnlyInputs...); err != nil {
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
// that does not mix with profile and a boolean preserve_source_bit_depth.
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
	return req, nil
}

func validateTranscodeMedia(_ context.Context, e *Engine, inputs map[string]any) (map[string]any, error) {
	if _, err := e.validateTranscodeRequest("transcode_media", inputs); err != nil {
		return inputs, err
	}
	return inputs, e.rejectSearchInputs("transcode_media", inputs)
}

// searchOnlyInputs only ever tuned the CRF sample search.
var searchOnlyInputs = []string{"metric", "priority", "shared_calibration", "calibration_items", "batch_fixed_quality"}

// SearchOnlyInputs are the inputs new requests may no longer set because
// they only tuned the removed sample search.
func SearchOnlyInputs() []string { return slices.Clone(searchOnlyInputs) }

// rejectSearchInputs refuses, for a new request, what only drove the CRF
// sample search: every goal now encodes the whole file at a fixed recipe and
// lets the final check decide (see transcode_recipe.go). It is an admission
// rule only: an action already admitted that holds them is ended by the engine
// (see removed_search.go).
func (e *Engine) rejectSearchInputs(name string, inputs map[string]any) error {
	if name == removedBenchmarkAction {
		return inputErr("", removedBenchmarkMessage)
	}
	for _, key := range searchOnlyInputs {
		if inputSet(inputs[key]) {
			return inputErr(key, "%s only tuned the sample search, which was removed; choose a goal (mode) instead", key)
		}
	}
	if raw, present := inputs["profile_config"]; present && raw != nil {
		var profile recipe.Profile
		if b, err := json.Marshal(raw); err == nil && json.Unmarshal(b, &profile) == nil && profile.Optimization != nil && profile.Optimization.Enabled {
			return inputErr("profile_config", "profile_config with optimization enabled ran the sample search, which was removed; turn optimization off for a fixed profile, or choose a goal (mode) instead")
		}
	}
	return e.rejectSearchProfile(name, inputs)
}

// rejectSearchProfile refuses a request without a goal whose catalog profile
// would run the sample search: the profile it names, or the one it falls back
// to when it names none. transcode_media falls back to the configured
// default_profile (then hevc-vt, as resolving the plan does), a batch to auto;
// auto stands for every profile the selector can pick. A profile that does not
// resolve is left to the step that resolves it.
func (e *Engine) rejectSearchProfile(action string, inputs map[string]any) error {
	if _, goal := inputs["mode"]; goal || inputs["profile_config"] != nil || e.deps.Config == nil {
		return nil
	}
	name := strings.TrimSpace(getString(inputs, "profile"))
	configured := false
	if name == "" {
		if action == "transcode_batch" {
			name = "auto"
		} else if name = strings.TrimSpace(e.deps.Config.Transcode.DefaultProfile); name != "" {
			configured = true
		} else {
			name = "hevc-vt"
		}
	}
	candidates := []string{name}
	if strings.EqualFold(name, "auto") {
		candidates = selector.Profiles()
	}
	for _, candidate := range candidates {
		profile, err := e.deps.Config.Transcode.ResolveProfile(candidate)
		if err != nil || profile.Optimization == nil || !profile.Optimization.Enabled {
			continue
		}
		what := fmt.Sprintf("profile %q runs", name)
		if candidate != name {
			what = fmt.Sprintf("profile %s can pick %q, which runs", name, candidate)
		}
		if configured {
			return inputErr("profile", "no goal or profile was given, and the configured default_profile is used: %s the sample search, which was removed; choose a goal (mode) or a profile without optimization, or set transcode.default_profile to one", what)
		}
		return inputErr("profile", "%s the sample search, which was removed; choose a goal (mode) or a profile without optimization instead", what)
	}
	return nil
}

// RefusedSearchInputs is why a new request with these inputs would be refused
// for driving the removed sample search, nil when it would not be: what a
// rerun of an older action checks before offering to start it again.
func (e *Engine) RefusedSearchInputs(action string, inputs map[string]any) error {
	return e.rejectSearchInputs(action, inputs)
}

// inputSet reports an input given a value: not absent, null, empty or false.
func inputSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case bool:
		return x
	}
	return true
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
	if err := unsupportedInput(tmpl, inputs, searchOnlyInputs...); err != nil {
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
	if err := e.rejectSearchInputs("transcode_batch", inputs); err != nil {
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
