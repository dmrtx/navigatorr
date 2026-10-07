package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func (w *Worker) podcastCapabilities(ctx context.Context) *transcode.PodcastCapabilities {
	if w.cfg.AppleSpeechPath == "" {
		return nil
	}
	c := &transcode.PodcastCapabilities{Provider: "apple_speech"}
	fail := func(err error) *transcode.PodcastCapabilities { c.Reason = err.Error(); return c }
	if w.cfg.AppleSpeechProbeAudio == "" {
		return fail(fmt.Errorf("configure apple_speech_probe_audio with a spoken fixture of at most 15 seconds"))
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, w.cfg.AppleSpeechPath, "probe").Output()
	if err != nil {
		return fail(fmt.Errorf("native Speech probe failed"))
	}
	var info struct {
		Available bool   `json:"available"`
		OSVersion string `json:"os_version"`
	}
	if json.Unmarshal(b, &info) != nil || !info.Available {
		return fail(fmt.Errorf("native Speech unavailable"))
	}
	c.ProviderVersion = info.OSVersion
	if !IsPathWithinAllowedRoots(w.cfg.AppleSpeechProbeAudio, w.cfg.AllowedRoots) {
		return fail(fmt.Errorf("ASR fixture outside allowed roots"))
	}
	duration, err := w.podcastDuration(ctx, w.cfg.AppleSpeechProbeAudio)
	if err != nil || duration > 15000 {
		return fail(fmt.Errorf("ASR fixture must be valid audio of at most 15 seconds"))
	}
	binSHA, err := podcastExecutableDigest(ctx, w.cfg.AppleSpeechPath)
	if err != nil {
		return fail(err)
	}
	audioSHA, err := hashLocalFileSHA256(ctx, w.cfg.AppleSpeechProbeAudio)
	if err != nil {
		return fail(err)
	}
	ffmpegSHA, err := podcastExecutableDigest(ctx, w.ffmpegPath)
	if err != nil {
		return fail(err)
	}
	ffprobeSHA, err := podcastExecutableDigest(ctx, w.ffprobePath)
	if err != nil {
		return fail(err)
	}
	language := w.cfg.AppleSpeechProbeLanguage
	if language == "" {
		language = "en_US"
	}
	key := podcast.Digest([]string{binSHA, audioSHA, language, info.OSVersion, ffmpegSHA, ffprobeSHA})
	dir := filepath.Join(w.cfg.StateDir, "_podcast-probe", key[7:])
	checkpoint := filepath.Join(dir, "capability.json")
	var cached transcode.PodcastCapabilities
	if podcast.ReadJSON(checkpoint, &cached) == nil && cached.Available {
		return &cached
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fail(err)
	}
	path := filepath.Join(dir, "transcript.json")
	args := []string{w.cfg.AppleSpeechProbeAudio, path, language}
	if w.cfg.AppleSpeechInstallAssets {
		args = append(args, "--install-assets")
	}
	if err := w.podcastCommand(ctx, dir, w.cfg.AppleSpeechPath, args...); err != nil {
		return fail(fmt.Errorf("executable native timing probe failed; see _podcast-probe"))
	}
	var t podcast.Transcript
	if err := podcast.ReadJSON(path, &t); err != nil {
		return fail(err)
	}
	if err := t.Validate(); err != nil {
		return fail(err)
	}
	if t.SourceHash != "sha256:"+audioSHA {
		return fail(fmt.Errorf("fixture identity changed"))
	}
	output := filepath.Join(dir, "render.mp3")
	cuts := podcast.Cuts{DurationMS: t.DurationMS, Ranges: []podcast.Cut{{StartMS: duration / 3, EndMS: duration / 2}}}
	if err := w.podcastCommand(ctx, dir, w.ffmpegPath, "-nostdin", "-v", "error", "-y", "-i", w.cfg.AppleSpeechProbeAudio, "-filter_complex", podcastFilter(cuts), "-map", "[out]", "-c:a", "libmp3lame", "-q:a", "2", output); err != nil {
		return fail(fmt.Errorf("MP3 filter/encoder execution probe failed"))
	}
	if err := w.podcastCommand(ctx, dir, w.ffmpegPath, "-nostdin", "-v", "error", "-xerror", "-i", output, "-f", "null", "-"); err != nil {
		return fail(fmt.Errorf("MP3 decode probe failed"))
	}
	c.Available = true
	c.NativeTimingVerified = true
	c.MP3RenderVerified = true
	c.VerifiedLanguage = language
	if err := podcast.WriteJSON(checkpoint, c); err != nil {
		return fail(err)
	}
	return c
}

func podcastExecutableDigest(ctx context.Context, path string) (string, error) {
	// Homebrew's stable bin paths are symlinks to versioned Cellar binaries.
	// Hash the resolved executable without relaxing regular-file checks for
	// media and checkpoints, and invalidate proof when its bytes change.
	executable, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	return hashLocalFileSHA256(ctx, executable)
}
