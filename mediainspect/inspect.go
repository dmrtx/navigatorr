// Package mediainspect inspects real media files without trusting the
// *arr mediaInfo cache. It shells out to one known binary (ffprobe) with
// internally built arguments — never a user-supplied command — and falls
// back to extension/size heuristics when ffprobe is unavailable.
package mediainspect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/maint"
)

// Stream describes one ffprobe stream reduced to what maintenance needs.
type Stream struct {
	Kind     string `json:"kind"` // video, audio, subtitle
	Codec    string `json:"codec"`
	Language string `json:"language,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	BitDepth int    `json:"bit_depth,omitempty"`
}

// Report is the inspection result for one file.
type Report struct {
	Path              string   `json:"path"`
	Container         string   `json:"container"`
	VideoCodec        string   `json:"video_codec"`
	Resolution        string   `json:"resolution"`
	BitDepth          int      `json:"bit_depth,omitempty"`
	AudioLanguages    []string `json:"audio_languages"`
	SubtitleLanguages []string `json:"subtitle_languages"`
	ExternalSubtitles []string `json:"external_subtitles,omitempty"`
	DurationSec       float64  `json:"duration_sec,omitempty"`
	SizeBytes         int64    `json:"size_bytes,omitempty"`
	DangerousFiles    []string `json:"dangerous_files,omitempty"`
	Probed            bool     `json:"probed"`
}

// InspectFile runs ffprobe against path and summarizes the streams.
// ffprobePath may be empty to force heuristic mode (tests, minimal images).
func InspectFile(ctx context.Context, ffprobePath, path string) (Report, error) {
	rep := Report{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.IsDir() {
		return rep, fmt.Errorf("%s is a directory", path)
	}
	rep.SizeBytes = fi.Size()
	rep.Container = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if maint.IsDangerousFilename(path) {
		rep.DangerousFiles = []string{filepath.Base(path)}
	}
	rep.ExternalSubtitles = FindSidecars(path)

	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	if ffprobePath == "" {
		return rep, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Fixed argv: the only variable element is the file path, passed as a
	// single argument with no shell in between.
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := cmd.Output()
	if err != nil {
		return rep, nil // unreadable file: heuristics above still stand
	}
	var probe struct {
		Streams []struct {
			CodecType        string            `json:"codec_type"`
			CodecName        string            `json:"codec_name"`
			Profile          string            `json:"profile"`
			PixFmt           string            `json:"pix_fmt"`
			Width            int               `json:"width"`
			Height           int               `json:"height"`
			BitsPerRawSample any               `json:"bits_per_raw_sample"`
			Tags             map[string]string `json:"tags"`
			Duration         string            `json:"duration"`
		} `json:"streams"`
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
			Size       string `json:"size"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return rep, nil
	}
	rep.Probed = true
	if probe.Format.FormatName != "" {
		rep.Container = strings.Split(probe.Format.FormatName, ",")[0]
	}
	var fmtDur float64
	fmt.Sscanf(probe.Format.Duration, "%f", &fmtDur)
	rep.DurationSec = fmtDur
	for _, st := range probe.Streams {
		lang := ""
		for k, v := range st.Tags {
			if strings.EqualFold(k, "language") {
				lang = maint.NormalizeLang(v)
			}
		}
		switch st.CodecType {
		case "video":
			if rep.VideoCodec == "" {
				rep.VideoCodec = st.CodecName
				rep.Resolution = resolutionName(st.Height)
				rep.BitDepth = ParseBitDepth(st.BitsPerRawSample, st.PixFmt, st.Profile)
			}
		case "audio":
			rep.AudioLanguages = appendNorm(rep.AudioLanguages, lang)
		case "subtitle":
			rep.SubtitleLanguages = appendNorm(rep.SubtitleLanguages, lang)
		}
	}
	return rep, nil
}

// ParseBitDepth extracts bit depth from raw bits (number or string), pixel format, or profile.
// It returns 0 when metadata does not specify the bit depth.
func ParseBitDepth(rawBits any, pixFmt, profile string) int {
	// 1. bits_per_raw_sample from ffprobe (can be string e.g. "8", "10" or number)
	if rawBits != nil {
		switch v := rawBits.(type) {
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		case float64:
			if v > 0 {
				return int(v)
			}
		case int:
			if v > 0 {
				return v
			}
		}
	}

	// 2. Derive from pixel format (e.g. yuv420p10le -> 10, p010le -> 10, yuv420p12le -> 12, yuv420p -> 8)
	pf := strings.ToLower(strings.TrimSpace(pixFmt))
	if pf != "" {
		if strings.Contains(pf, "10le") || strings.Contains(pf, "10be") || strings.Contains(pf, "10msb") || pf == "p010" || strings.HasPrefix(pf, "p010") {
			return 10
		}
		if strings.Contains(pf, "12le") || strings.Contains(pf, "12be") || strings.Contains(pf, "12msb") || pf == "p012" || strings.HasPrefix(pf, "p012") {
			return 12
		}
		if strings.Contains(pf, "16le") || strings.Contains(pf, "16be") || strings.Contains(pf, "16msb") || pf == "p016" || strings.HasPrefix(pf, "p016") {
			return 16
		}
		if strings.Contains(pf, "9le") || strings.Contains(pf, "9be") {
			return 9
		}
		switch pf {
		case "yuv420p", "yuvj420p", "yuv422p", "yuvj422p", "yuv444p", "yuvj444p",
			"nv12", "nv21", "yuyv422", "uyvy422", "rgb24", "bgr24", "rgba", "bgra", "gray":
			return 8
		}
	}

	// 3. Fallback to codec profile metadata
	prof := strings.ToLower(strings.TrimSpace(profile))
	if prof != "" {
		if strings.Contains(prof, "10") { // e.g. "main 10", "high 10", "profile 10"
			return 10
		}
		if strings.Contains(prof, "12") { // e.g. "main 12"
			return 12
		}
		if prof == "main" || prof == "baseline" || prof == "high" || prof == "main progressive" {
			return 8
		}
	}

	return 0
}

func appendNorm(list []string, lang string) []string {
	if lang == "" || lang == "und" {
		return list
	}
	for _, l := range list {
		if l == lang {
			return list
		}
	}
	return append(list, lang)
}

func resolutionName(h int) string {
	switch {
	case h >= 2000:
		return "2160p"
	case h >= 1000:
		return "1080p"
	case h >= 650:
		return "720p"
	case h > 0:
		return "480p"
	default:
		return ""
	}
}

// FindSidecars lists external subtitle files next to a media file.
func FindSidecars(mediaPath string) []string {
	dir := filepath.Dir(mediaPath)
	stem := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
		if !maint.SidecarSubExtensions[ext] {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		// "Movie.eng.srt" or "Movie.srt" both belong to "Movie".
		if base == stem || strings.HasPrefix(base, stem+".") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// ScanDangerous walks root (max depth/files bounded) for dangerous names.
func ScanDangerous(root string, maxFiles int) ([]string, error) {
	if maxFiles <= 0 || maxFiles > 10000 {
		maxFiles = 2000
	}
	var out []string
	count := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, do not abort the scan
		}
		if count >= maxFiles {
			return filepath.SkipAll
		}
		count++
		if !d.IsDir() && maint.IsDangerousFilename(d.Name()) {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}
