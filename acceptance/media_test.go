package acceptance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// srcSpec describes a synthetic source. The picture is a moving test pattern
// with temporal noise: textured and animated enough that the quality gates
// measure something real, yet cheap to generate.
type srcSpec struct {
	name  string
	w, h  int
	secs  int
	noise int // temporal noise strength, 0 for none
	// audio is the codec of the stereo track: "flac" (default) or "ac3".
	audio string
	// untagged leaves out the Matroska statistics tags that muxers such as
	// mkvmerge write, as many remuxed files do.
	untagged bool
	// vfr adds a burst of frames closer together than one frame period.
	vfr bool
	// hdr makes a 10-bit HEVC HDR10 source (BT.2020, PQ, mastering display and
	// content light metadata) instead of SDR H.264.
	hdr bool
	// hevc makes an SDR 8-bit HEVC source.
	hevc bool
}

// noiseAudio is the sources' stereo track: two independent channels of quiet
// pink noise, which no codec can shrink. Measured with the local ffmpeg
// (ffprobe packet sizes), FLAC encodes it at about 1170 kbps. The track's first
// version, a 330 Hz sine, came to 133 kbps: under 1.25 times the size goal's
// AAC-LC target of 192, which the audio levels copy rather than convert.
func noiseAudio(secs int) string {
	return fmt.Sprintf("anoisesrc=color=pink:amplitude=0.001:sample_rate=48000:duration=%d:seed=1[l];"+
		"anoisesrc=color=pink:amplitude=0.001:sample_rate=48000:duration=%d:seed=2[r];[l][r]amerge=inputs=2", secs, secs)
}

func (s *suite) run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, tail(out.String(), 4000))
	}
	return out.String()
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// source returns the synthetic file for spec, making it on first use. The
// cases that only read a source share one copy; a case that changes its source
// (promotion) makes its own with a distinct name.
func (s *suite) source(t *testing.T, spec srcSpec) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.sources[spec.name]; ok {
		return p
	}
	start := time.Now()
	p := s.makeSource(t, spec)
	t.Logf("made source %s (%s) in %s", spec.name, megabytes(fileSize(t, p)), time.Since(start).Round(time.Second))
	s.sources[spec.name] = p
	return p
}

// makeSource writes the source into the media root and returns its path.
func (s *suite) makeSource(t *testing.T, spec srcSpec) string {
	t.Helper()
	if spec.audio == "" {
		spec.audio = "flac"
	}
	path := filepath.Join(s.root, spec.name+".mkv")
	raw := filepath.Join(s.root, ".raw-"+spec.name+".mkv")
	defer os.Remove(raw)

	pic := fmt.Sprintf("testsrc2=size=%dx%d:rate=24:duration=%d", spec.w, spec.h, spec.secs)
	if spec.noise > 0 {
		pic += fmt.Sprintf(",noise=alls=%d:allf=t", spec.noise)
	}
	if spec.vfr {
		// 12 s at 24 fps, 8 s at 48 fps (20.8 ms apart, closer than one 24 fps
		// tick), then 24 fps again; frame N keeps its picture throughout. The
		// millisecond time base is Matroska's own, so no two frames collide.
		pic += `,settb=1/1000,setpts='if(lt(N,288),N/24,if(lt(N,672),12+(N-288)/48,20+(N-672)/24))/TB'`
	}
	args := []string{"-hide_banner", "-v", "error", "-y", "-f", "lavfi", "-i", pic,
		"-f", "lavfi", "-i", noiseAudio(spec.secs)}
	switch {
	case spec.hdr:
		args = append(args, "-vf", "format=yuv420p10le", "-c:v", "libx265", "-preset", "veryfast", "-crf", "16",
			"-x265-params", "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:"+
				"master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400:"+
				"hdr10=1:hdr10-opt=1:repeat-headers=1",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv")
	case spec.hevc:
		args = append(args, "-vf", "format=yuv420p", "-c:v", "libx265", "-preset", "veryfast", "-crf", "16",
			"-x265-params", "log-level=error", "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	default:
		args = append(args, "-vf", "format=yuv420p", "-c:v", "libx264", "-preset", "veryfast", "-crf", "14",
			"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	}
	if spec.vfr {
		// Keep the source timestamps exactly; a vfr encode would re-quantise them.
		args = append(args, "-fps_mode:v:0", "passthrough", "-enc_time_base:v:0", "demux")
	}
	switch spec.audio {
	case "ac3":
		args = append(args, "-c:a", "ac3", "-b:a", "384k", "-ac", "2")
	default:
		args = append(args, "-c:a", "flac", "-ac", "2")
	}
	args = append(args, "-map_metadata", "-1", "-map", "0:v:0", "-map", "1:a:0", raw)
	s.run(t, s.ffmpeg, args...)

	if spec.untagged {
		s.run(t, s.ffmpeg, "-hide_banner", "-v", "error", "-y", "-i", raw, "-map", "0", "-c", "copy",
			"-map_metadata", "-1", "-fflags", "+bitexact", "-flags:v", "+bitexact", "-flags:a", "+bitexact", path)
		return path
	}
	s.tagStatistics(t, raw, path)
	return path
}

// tagStatistics remuxes src to dst with the per-stream Matroska statistics
// tags (BPS, NUMBER_OF_BYTES, NUMBER_OF_FRAMES, DURATION) that mkvmerge writes
// and Navigatorr reads for stream sizes.
func (s *suite) tagStatistics(t *testing.T, src, dst string) {
	t.Helper()
	out := s.run(t, s.ffprobe, "-v", "error", "-show_entries", "packet=stream_index,size", "-of", "csv=p=0", src)
	bytesBy, framesBy := map[int]int64{}, map[int]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 2 {
			continue
		}
		idx, _ := strconv.Atoi(f[0])
		n, _ := strconv.ParseInt(f[1], 10, 64)
		bytesBy[idx] += n
		framesBy[idx]++
	}
	info := s.probe(t, src)
	args := []string{"-hide_banner", "-v", "error", "-y", "-i", src, "-map", "0", "-c", "copy", "-map_metadata", "-1"}
	for i, st := range info.Streams {
		bps := int64(float64(bytesBy[i]) * 8 / info.Format.Duration)
		sel := fmt.Sprintf("-metadata:s:%d", i)
		args = append(args,
			sel, fmt.Sprintf("BPS=%d", bps),
			sel, fmt.Sprintf("NUMBER_OF_BYTES=%d", bytesBy[i]),
			sel, fmt.Sprintf("NUMBER_OF_FRAMES=%d", framesBy[i]),
			sel, "DURATION="+st.Tags["DURATION"])
	}
	s.run(t, s.ffmpeg, append(args, dst)...)
}

// probeInfo is the part of ffprobe's JSON the assertions read.
type probeInfo struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Duration float64 `json:"-"`
		RawDur   string  `json:"duration"`
		Size     int64   `json:"-"`
		RawSize  string  `json:"size"`
	} `json:"format"`
}

type probeStream struct {
	Index          int               `json:"index"`
	CodecType      string            `json:"codec_type"`
	CodecName      string            `json:"codec_name"`
	Profile        string            `json:"profile"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	PixFmt         string            `json:"pix_fmt"`
	ColorPrimaries string            `json:"color_primaries"`
	ColorTransfer  string            `json:"color_transfer"`
	ColorSpace     string            `json:"color_space"`
	Channels       int               `json:"channels"`
	Tags           map[string]string `json:"tags"`
}

func (s *suite) probe(t *testing.T, path string) probeInfo {
	t.Helper()
	out := s.run(t, s.ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path)
	var info probeInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("parsing ffprobe output for %s: %v", filepath.Base(path), err)
	}
	info.Format.Duration, _ = strconv.ParseFloat(info.Format.RawDur, 64)
	info.Format.Size, _ = strconv.ParseInt(info.Format.RawSize, 10, 64)
	return info
}

func (p probeInfo) first(kind string) (probeStream, bool) {
	for _, st := range p.Streams {
		if st.CodecType == kind {
			return st, true
		}
	}
	return probeStream{}, false
}

// decodes proves the file is playable end to end: every frame decodes without
// an error.
func (s *suite) decodes(t *testing.T, path string) {
	t.Helper()
	s.run(t, s.ffmpeg, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// videoFrames counts the packets of the first video stream: one per frame, so
// an encode that dropped or merged frames shows as a different count.
func (s *suite) videoFrames(t *testing.T, path string) int {
	t.Helper()
	out := s.run(t, s.ffprobe, "-v", "error", "-count_packets", "-select_streams", "v:0", "-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path)
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("counting frames of %s: %q", filepath.Base(path), out)
	}
	return n
}
