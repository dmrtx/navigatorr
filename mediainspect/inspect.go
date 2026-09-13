// Package mediainspect inspects real media files without trusting the
// *arr mediaInfo cache. It shells out to one known binary (ffprobe) with
// internally built arguments — never a user-supplied command — and falls
// back to extension/size heuristics when ffprobe is unavailable.
package mediainspect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// MasteringDisplayMetadata captures HDR mastering display color volume (SMPTE 2086).
type MasteringDisplayMetadata struct {
	RedX         string `json:"red_x,omitempty"`
	RedY         string `json:"red_y,omitempty"`
	GreenX       string `json:"green_x,omitempty"`
	GreenY       string `json:"green_y,omitempty"`
	BlueX        string `json:"blue_x,omitempty"`
	BlueY        string `json:"blue_y,omitempty"`
	WhitePointX  string `json:"white_point_x,omitempty"`
	WhitePointY  string `json:"white_point_y,omitempty"`
	MinLuminance string `json:"min_luminance,omitempty"`
	MaxLuminance string `json:"max_luminance,omitempty"`
}

// ContentLightLevelMetadata captures HDR content light levels (CTA-861.3).
type ContentLightLevelMetadata struct {
	MaxCLL  int `json:"max_content,omitempty"`
	MaxFALL int `json:"max_average,omitempty"`
}

// SideDataRecord captures arbitrary stream side data records from ffprobe.
type SideDataRecord struct {
	SideDataType string         `json:"side_data_type"`
	Data         map[string]any `json:"data,omitempty"`
}

// HDRReport summarizes high-dynamic-range metadata present on media streams.
type HDRReport struct {
	Present           bool                       `json:"present"`
	ColorPrimaries    string                     `json:"color_primaries,omitempty"`
	ColorTransfer     string                     `json:"color_transfer,omitempty"`
	ColorSpace        string                     `json:"color_space,omitempty"`
	MasteringDisplay  *MasteringDisplayMetadata  `json:"mastering_display,omitempty"`
	ContentLightLevel *ContentLightLevelMetadata `json:"content_light_level,omitempty"`
}

// DetailedStream captures stream metadata needed for high-fidelity transcode verification.
type DetailedStream struct {
	Index             int                        `json:"index"`
	Kind              string                     `json:"kind"` // video, audio, subtitle, attachment
	Codec             string                     `json:"codec"`
	Profile           string                     `json:"profile,omitempty"`
	PixelFormat       string                     `json:"pixel_format,omitempty"`
	Language          string                     `json:"language,omitempty"`
	Title             string                     `json:"title,omitempty"`
	Channels          int                        `json:"channels,omitempty"`
	ChannelLayout     string                     `json:"channel_layout,omitempty"`
	Width             int                        `json:"width,omitempty"`
	Height            int                        `json:"height,omitempty"`
	BitDepth          int                        `json:"bit_depth,omitempty"`
	RFrameRate        string                     `json:"r_frame_rate,omitempty"`
	AvgFrameRate      string                     `json:"avg_frame_rate,omitempty"`
	FrameRate         string                     `json:"frame_rate,omitempty"` // aliases avg_frame_rate or r_frame_rate for backwards compatibility
	FPS               float64                    `json:"fps,omitempty"`
	BitRate           int64                      `json:"bit_rate,omitempty"`
	ColorRange        string                     `json:"color_range,omitempty"`
	ColorSpace        string                     `json:"color_space,omitempty"`
	ColorPrimaries    string                     `json:"color_primaries,omitempty"`
	ColorTransfer     string                     `json:"color_transfer,omitempty"`
	MasteringDisplay  *MasteringDisplayMetadata  `json:"mastering_display,omitempty"`
	ContentLightLevel *ContentLightLevelMetadata `json:"content_light_level,omitempty"`
	SideData          []SideDataRecord           `json:"side_data,omitempty"`
	Tags              map[string]string          `json:"tags,omitempty"`
	Disposition       map[string]int             `json:"disposition,omitempty"`
}

// DetailedReport provides full-fidelity inspection including video, audio, subtitles, attachments, and chapters.
type DetailedReport struct {
	Path        string           `json:"path"`
	Container   string           `json:"container"`
	DurationSec float64          `json:"duration_sec"`
	SizeBytes   int64            `json:"size_bytes"`
	BitRate     int64            `json:"bit_rate,omitempty"` // format/overall container bitrate in bps
	Video       []DetailedStream `json:"video"`
	Audio       []DetailedStream `json:"audio"`
	Subtitles   []DetailedStream `json:"subtitles"`
	Attachments []DetailedStream `json:"attachments"`
	Chapters    int              `json:"chapters"`
	Probed      bool             `json:"probed"`
	HDR         *HDRReport       `json:"hdr,omitempty"`
}

// InspectDetailed runs ffprobe with streams and chapters for rigorous transcode validation.
func InspectDetailed(ctx context.Context, ffprobePath, path string) (DetailedReport, error) {
	rep := DetailedReport{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.IsDir() {
		return rep, fmt.Errorf("%s is a directory", path)
	}
	rep.SizeBytes = fi.Size()
	rep.Container = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")

	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	if ffprobePath == "" {
		return rep, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters", path)
	out, err := cmd.Output()
	if err != nil {
		return rep, nil
	}

	var probe struct {
		Streams []struct {
			Index            int               `json:"index"`
			CodecType        string            `json:"codec_type"`
			CodecName        string            `json:"codec_name"`
			Profile          string            `json:"profile"`
			PixFmt           string            `json:"pix_fmt"`
			RFrameRate       string            `json:"r_frame_rate"`
			AvgFrameRate     string            `json:"avg_frame_rate"`
			Width            int               `json:"width"`
			Height           int               `json:"height"`
			Channels         int               `json:"channels"`
			ChannelLayout    string            `json:"channel_layout"`
			BitsPerRawSample any               `json:"bits_per_raw_sample"`
			BitRate          any               `json:"bit_rate"`
			ColorRange       string            `json:"color_range"`
			ColorSpace       string            `json:"color_space"`
			ColorPrimaries   string            `json:"color_primaries"`
			ColorTransfer    string            `json:"color_transfer"`
			SideDataList     []json.RawMessage `json:"side_data_list"`
			Tags             map[string]string `json:"tags"`
			Disposition      map[string]int    `json:"disposition"`
		} `json:"streams"`
		Format struct {
			FormatName string            `json:"format_name"`
			Duration   string            `json:"duration"`
			Size       string            `json:"size"`
			BitRate    any               `json:"bit_rate"`
			Tags       map[string]string `json:"tags"`
		} `json:"format"`
		Chapters []any `json:"chapters"`
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
	rep.BitRate = ParseBitRate(probe.Format.BitRate, probe.Format.Tags)
	rep.Chapters = len(probe.Chapters)

	for _, st := range probe.Streams {
		lang := ""
		title := ""
		for k, v := range st.Tags {
			if strings.EqualFold(k, "language") {
				lang = maint.NormalizeLang(v)
			}
			if strings.EqualFold(k, "title") {
				title = strings.TrimSpace(v)
			}
		}

		fps, ok := ParseFrameRateRational(st.AvgFrameRate)
		if !ok {
			fps, _ = ParseFrameRateRational(st.RFrameRate)
		}
		frameRate := st.AvgFrameRate
		if frameRate == "" || frameRate == "0/0" {
			frameRate = st.RFrameRate
		}
		bitRate := ParseBitRate(st.BitRate, st.Tags)

		ds := DetailedStream{
			Index:          st.Index,
			Kind:           st.CodecType,
			Codec:          strings.ToLower(st.CodecName),
			Profile:        st.Profile,
			PixelFormat:    st.PixFmt,
			Language:       lang,
			Title:          title,
			Channels:       st.Channels,
			ChannelLayout:  st.ChannelLayout,
			Width:          st.Width,
			Height:         st.Height,
			RFrameRate:     st.RFrameRate,
			AvgFrameRate:   st.AvgFrameRate,
			FrameRate:      frameRate,
			FPS:            fps,
			BitRate:        bitRate,
			ColorRange:     st.ColorRange,
			ColorSpace:     st.ColorSpace,
			ColorPrimaries: st.ColorPrimaries,
			ColorTransfer:  st.ColorTransfer,
			Tags:           st.Tags,
			Disposition:    st.Disposition,
		}

		if len(st.SideDataList) > 0 {
			ds.SideData = sanitizeSideDataList(st.SideDataList)
			for _, rawSD := range st.SideDataList {
				var sdMap map[string]any
				if err := json.Unmarshal(rawSD, &sdMap); err != nil {
					continue
				}
				sdType, _ := sdMap["side_data_type"].(string)
				if strings.EqualFold(sdType, "Mastering display metadata") {
					ds.MasteringDisplay = parseMasteringDisplayMetadata(sdMap)
				}
				if strings.EqualFold(sdType, "Content light level metadata") {
					ds.ContentLightLevel = parseContentLightLevelMetadata(sdMap)
				}
			}
		}

		switch st.CodecType {
		case "video":
			ds.BitDepth = ParseBitDepth(st.BitsPerRawSample, st.PixFmt, st.Profile)
			if isHDRStream(ds) {
				if rep.HDR == nil {
					rep.HDR = &HDRReport{
						Present:           true,
						ColorPrimaries:    ds.ColorPrimaries,
						ColorTransfer:     ds.ColorTransfer,
						ColorSpace:        ds.ColorSpace,
						MasteringDisplay:  ds.MasteringDisplay,
						ContentLightLevel: ds.ContentLightLevel,
					}
				}
			}
			rep.Video = append(rep.Video, ds)
		case "audio":
			rep.Audio = append(rep.Audio, ds)
		case "subtitle":
			rep.Subtitles = append(rep.Subtitles, ds)
		case "attachment":
			rep.Attachments = append(rep.Attachments, ds)
		}
	}

	return rep, nil
}

// ParseFrameRateRational parses a rational frame rate string like "24000/1001" or "24" to float64.
func ParseFrameRateRational(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0/0" {
		return 0, false
	}
	parts := strings.Split(raw, "/")
	if len(parts) == 2 {
		num, err1 := strconv.ParseFloat(parts[0], 64)
		den, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 == nil && err2 == nil && den > 0 && !math.IsNaN(num) && !math.IsNaN(den) && !math.IsInf(num, 0) && !math.IsInf(den, 0) {
			fps := num / den
			if fps > 0 && !math.IsNaN(fps) && !math.IsInf(fps, 0) {
				return fps, true
			}
		}
	} else if len(parts) == 1 {
		val, err := strconv.ParseFloat(parts[0], 64)
		if err == nil && val > 0 && !math.IsNaN(val) && !math.IsInf(val, 0) {
			return val, true
		}
	}
	return 0, false
}

// ParseFrameRate extracts frame rate prioritizing avg_frame_rate first, falling back to r_frame_rate.
// It returns the chosen frame rate string and the calculated float64 FPS.
func ParseFrameRate(rFrameRate, avgFrameRate string) (string, float64) {
	if fps, ok := ParseFrameRateRational(avgFrameRate); ok {
		return avgFrameRate, fps
	}
	if fps, ok := ParseFrameRateRational(rFrameRate); ok {
		return rFrameRate, fps
	}
	return "", 0
}

func parseMasteringDisplayMetadata(sdMap map[string]any) *MasteringDisplayMetadata {
	if sdMap == nil {
		return nil
	}
	getString := func(key string) string {
		v, ok := sdMap[key]
		if !ok || v == nil {
			return ""
		}
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		if s == "" || s == "<nil>" || s == "nil" {
			return ""
		}
		return s
	}

	md := &MasteringDisplayMetadata{
		RedX:         getString("red_x"),
		RedY:         getString("red_y"),
		GreenX:       getString("green_x"),
		GreenY:       getString("green_y"),
		BlueX:        getString("blue_x"),
		BlueY:        getString("blue_y"),
		WhitePointX:  getString("white_point_x"),
		WhitePointY:  getString("white_point_y"),
		MinLuminance: getString("min_luminance"),
		MaxLuminance: getString("max_luminance"),
	}

	if md.RedX == "" && md.RedY == "" && md.GreenX == "" && md.GreenY == "" &&
		md.BlueX == "" && md.BlueY == "" && md.WhitePointX == "" && md.WhitePointY == "" &&
		md.MinLuminance == "" && md.MaxLuminance == "" {
		return nil
	}
	return md
}

func parseContentLightLevelMetadata(sdMap map[string]any) *ContentLightLevelMetadata {
	if sdMap == nil {
		return nil
	}
	getInt := func(keys ...string) int {
		for _, k := range keys {
			v, ok := sdMap[k]
			if !ok || v == nil {
				continue
			}
			switch val := v.(type) {
			case float64:
				if val > 0 && !math.IsNaN(val) && !math.IsInf(val, 0) && val <= float64(math.MaxInt32) {
					return int(val)
				}
			case float32:
				f := float64(val)
				if f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) && f <= float64(math.MaxInt32) {
					return int(f)
				}
			case int:
				if val > 0 {
					return val
				}
			case int64:
				if val > 0 && val <= math.MaxInt32 {
					return int(val)
				}
			case string:
				trimmed := strings.TrimSpace(val)
				if n, err := strconv.Atoi(trimmed); err == nil && n > 0 {
					return n
				}
				if f, err := strconv.ParseFloat(trimmed, 64); err == nil && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) && f <= float64(math.MaxInt32) {
					return int(f)
				}
			}
		}
		return 0
	}

	cll := &ContentLightLevelMetadata{
		MaxCLL:  getInt("max_content", "max_content_light_level"),
		MaxFALL: getInt("max_average", "max_frame_average_light_level"),
	}
	if cll.MaxCLL == 0 && cll.MaxFALL == 0 {
		return nil
	}
	return cll
}

const (
	maxSideDataEntries = 10
	maxSideDataKeys    = 15
	maxSideDataValLen  = 256
)

func sanitizeSideDataList(rawList []json.RawMessage) []SideDataRecord {
	if len(rawList) == 0 {
		return nil
	}
	limit := len(rawList)
	if limit > maxSideDataEntries {
		limit = maxSideDataEntries
	}
	result := make([]SideDataRecord, 0, limit)
	for i := 0; i < limit; i++ {
		var sdMap map[string]any
		if err := json.Unmarshal(rawList[i], &sdMap); err != nil || len(sdMap) == 0 {
			continue
		}
		sdType, _ := sdMap["side_data_type"].(string)
		sanitized := sanitizeSideDataMap(sdMap)
		if len(sanitized) > 0 {
			result = append(result, SideDataRecord{
				SideDataType: sdType,
				Data:         sanitized,
			})
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func sanitizeSideDataMap(entry map[string]any) map[string]any {
	out := make(map[string]any)
	keys := make([]string, 0, len(entry))
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	count := 0
	for _, k := range keys {
		if count >= maxSideDataKeys {
			break
		}
		v := entry[k]
		if v == nil {
			continue
		}
		switch val := v.(type) {
		case string:
			str := strings.TrimSpace(val)
			if str == "" || str == "<nil>" || str == "nil" {
				continue
			}
			if len(str) > maxSideDataValLen {
				str = str[:maxSideDataValLen]
			}
			out[k] = str
			count++
		case float64, float32, int, int64, int32, uint, uint64, uint32, bool:
			out[k] = val
			count++
		case map[string]any:
			nested := make(map[string]any)
			nestedKeys := make([]string, 0, len(val))
			for nk := range val {
				nestedKeys = append(nestedKeys, nk)
			}
			sort.Strings(nestedKeys)

			nestedCount := 0
			for _, nk := range nestedKeys {
				if nestedCount >= 10 {
					break
				}
				nv := val[nk]
				if nv == nil {
					continue
				}
				switch nval := nv.(type) {
				case string:
					str := strings.TrimSpace(nval)
					if str != "" && str != "<nil>" && str != "nil" {
						if len(str) > maxSideDataValLen {
							str = str[:maxSideDataValLen]
						}
						nested[nk] = str
						nestedCount++
					}
				case float64, float32, int, int64, int32, uint, uint64, uint32, bool:
					nested[nk] = nval
					nestedCount++
				}
			}
			if len(nested) > 0 {
				out[k] = nested
				count++
			}
		}
	}
	return out
}

// ParseBitRate extracts bitrate in bits per second from raw ffprobe bit_rate or tags.
func ParseBitRate(raw any, tags map[string]string) int64 {
	if raw != nil {
		switch v := raw.(type) {
		case string:
			trimmed := strings.TrimSpace(v)
			if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil && n > 0 {
				return n
			}
			if f, err := strconv.ParseFloat(trimmed, 64); err == nil && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) && f <= float64(math.MaxInt64) {
				return int64(f)
			}
		case float64:
			if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) && v <= float64(math.MaxInt64) {
				return int64(v)
			}
		case float32:
			f := float64(v)
			if f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) && f <= float64(math.MaxInt64) {
				return int64(f)
			}
		case int64:
			if v > 0 {
				return v
			}
		case int:
			if v > 0 {
				return int64(v)
			}
		}
	}
	for k, v := range tags {
		if strings.HasPrefix(strings.ToUpper(k), "BPS") {
			trimmed := strings.TrimSpace(v)
			if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil && n > 0 {
				return n
			}
			if f, err := strconv.ParseFloat(trimmed, 64); err == nil && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) && f <= float64(math.MaxInt64) {
				return int64(f)
			}
		}
	}
	return 0
}

func isHDRStream(st DetailedStream) bool {
	ct := strings.ToLower(strings.TrimSpace(st.ColorTransfer))
	cp := strings.ToLower(strings.TrimSpace(st.ColorPrimaries))
	cs := strings.ToLower(strings.TrimSpace(st.ColorSpace))
	if ct == "smpte2084" || ct == "arib-std-b67" || strings.Contains(ct, "2084") || strings.Contains(ct, "hlg") {
		return true
	}
	if cp == "bt2020" || strings.Contains(cp, "2020") {
		return true
	}
	if cs == "bt2020nc" || cs == "bt2020c" || strings.Contains(cs, "2020") {
		return true
	}
	if st.MasteringDisplay != nil || st.ContentLightLevel != nil {
		return true
	}
	return false
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
