package mediainspect

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// videoTimelineTail is how far before the container's end the video's last
// packets are looked for, and videoTimelineTimeout bounds the read.
const (
	videoTimelineTail    = 30.0
	videoTimelineTimeout = 20 * time.Second
)

// MeasureVideoSeconds is the length of video stream streamIndex from its first
// picture to the end of its last one. A container can run on past its video,
// with a subtitle or attachment carrying the duration ffprobe reports; the
// video's own duration is often not stated at all. It reads only the file's
// tail: seeking near the end lands on the video's last keyframe before that
// point, so a video that stopped early still shows its last packets.
//
// It returns 0 when the tail holds no packet of the stream (a container that
// cannot seek by time); the caller then has only the container's duration.
func MeasureVideoSeconds(ctx context.Context, ffprobePath, path string, streamIndex int, duration float64) (float64, error) {
	if duration <= 0 {
		return 0, errors.New("container duration unknown")
	}
	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	from := max(0, duration-videoTimelineTail)
	res, err := runFFprobe(ctx, ffprobePath, videoTimelineTimeout,
		"-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-read_intervals", strconv.FormatFloat(from, 'f', 3, 64)+"%",
		"-show_entries", "packet=pts_time,duration_time:stream=start_time", "-of", "compact", path)
	if err != nil {
		return 0, err
	}
	var end, start float64
	packets, hasStart := 0, false
	for _, line := range strings.Split(string(res.stdout), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		values := map[string]float64{}
		for _, f := range fields[1:] {
			if k, v, ok := strings.Cut(f, "="); ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					values[k] = n
				}
			}
		}
		switch fields[0] {
		case "packet":
			pts, ok := values["pts_time"]
			if !ok {
				continue
			}
			if e := pts + values["duration_time"]; packets == 0 || e > end {
				end = e
			}
			packets++
		case "stream":
			start, hasStart = values["start_time"]
		}
	}
	if packets == 0 {
		return 0, nil
	}
	if !hasStart {
		return 0, fmt.Errorf("ffprobe printed no start time for stream %d", streamIndex)
	}
	return end - start, nil
}

// WindowSeconds is the timeline quality windows are placed on: the video's
// measured length when the container runs more than a second longer, else the
// container's duration, so a file whose video fills it keeps the windows it
// always had.
func (r *DetailedReport) WindowSeconds() float64 {
	if r.VideoSeconds > 0 && r.VideoSeconds < r.DurationSec-1 {
		return r.VideoSeconds
	}
	return r.DurationSec
}
