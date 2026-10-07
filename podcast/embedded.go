package podcast

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const embeddedDescription = "Navigatorr original-audio transcript"
const maxEmbeddedTag = 64 << 20

type EmbeddedTranscript struct {
	Version          int        `json:"version"`
	Timeline         string     `json:"timeline"`
	ASRJobID         string     `json:"asr_job_id"`
	TranscriptDigest string     `json:"transcript_digest"`
	Transcript       Transcript `json:"transcript"`
	Cuts             Cuts       `json:"cuts"`
}

func syncSafe(n int) []byte {
	return []byte{byte(n>>21) & 127, byte(n>>14) & 127, byte(n>>7) & 127, byte(n) & 127}
}
func tagSize(b []byte) (int, error) {
	n := 0
	for _, v := range b {
		if v > 127 {
			return 0, fmt.Errorf("invalid ID3 size")
		}
		n = (n << 7) | int(v)
	}
	return n, nil
}

// EmbedTranscript preserves the MPEG audio bytes and existing unflagged ID3v2.3/4
// frames. Unsupported tag structures fail before the original file is touched.
func EmbedTranscript(file, asrJobID string, t Transcript, cuts Cuts) error {
	if err := VerifyCuts(t, cuts); err != nil {
		return err
	}
	bundle := EmbeddedTranscript{1, "original_audio", asrJobID, Digest(t), t, cuts}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	object := append([]byte("\x00application/json\x00navigatorr-transcript.json\x00"+embeddedDescription+"\x00"), payload...)
	in, err := os.Open(file)
	if err != nil {
		return err
	}
	defer in.Close()
	header := make([]byte, 10)
	if _, err = io.ReadFull(in, header); err != nil {
		return err
	}
	version := byte(3)
	frames := []byte{}
	audioOffset := int64(0)
	if string(header[:3]) == "ID3" {
		version = header[3]
		if (version != 3 && version != 4) || header[4] != 0 || header[5] != 0 {
			return fmt.Errorf("unsupported ID3 structure for transcript")
		}
		size, e := tagSize(header[6:])
		if e != nil {
			return e
		}
		if size > maxEmbeddedTag {
			return fmt.Errorf("ID3 tag too large")
		}
		old := make([]byte, size)
		if _, err = io.ReadFull(in, old); err != nil {
			return err
		}
		audioOffset = int64(size + 10)
		for at := 0; at < len(old); {
			if old[at] == 0 {
				break
			}
			if at+10 > len(old) {
				return fmt.Errorf("truncated ID3 frame")
			}
			h := old[at : at+10]
			n := int(binary.BigEndian.Uint32(h[4:8]))
			if version == 4 {
				n, err = tagSize(h[4:8])
				if err != nil {
					return err
				}
			}
			if n <= 0 || n > len(old)-at-10 {
				return fmt.Errorf("invalid ID3 frame length")
			}
			frame := old[at : at+10+n]
			// Replace our own attachment when rendering a previously tagged input.
			own := string(h[:4]) == "GEOB" && h[8] == 0 && h[9] == 0 && bytes.HasPrefix(frame[10:], []byte("\x00application/json\x00navigatorr-transcript.json\x00"+embeddedDescription+"\x00"))
			if !own {
				frames = append(frames, frame...)
			}
			at += 10 + n
		}
	}
	fh := make([]byte, 10)
	copy(fh, "GEOB")
	if version == 4 {
		copy(fh[4:8], syncSafe(len(object)))
	} else {
		binary.BigEndian.PutUint32(fh[4:8], uint32(len(object)))
	}
	frames = append(frames, fh...)
	frames = append(frames, object...)
	if len(frames) > maxEmbeddedTag {
		return fmt.Errorf("transcript tag too large")
	}
	header = append([]byte{'I', 'D', '3', version, 0, 0}, syncSafe(len(frames))...)
	stage, err := os.CreateTemp(filepath.Dir(file), ".transcript-*.mp3")
	if err != nil {
		return err
	}
	name := stage.Name()
	defer os.Remove(name)
	fail := func(e error) error { stage.Close(); return e }
	if _, err = stage.Write(header); err != nil {
		return fail(err)
	}
	if _, err = stage.Write(frames); err != nil {
		return fail(err)
	}
	if _, err = in.Seek(audioOffset, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err = io.Copy(stage, in); err != nil {
		return fail(err)
	}
	if err = stage.Sync(); err != nil {
		return fail(err)
	}
	if err = stage.Close(); err != nil {
		return err
	}
	return os.Rename(name, file)
}
