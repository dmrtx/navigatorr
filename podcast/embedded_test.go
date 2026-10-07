package podcast

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedTranscriptPreservesAudioAndMetadata(t *testing.T) {
	tr, p := fixture()
	cuts, err := Plan(tr, p, decisions(tr, p))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []byte{0, 3, 4} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			audio := append([]byte{0xff, 0xfb}, bytes.Repeat([]byte("mpeg audio"), 100)...)
			old := []byte{}
			title := []byte("\x00Episode title")
			h := make([]byte, 10)
			copy(h, "TIT2")
			if version == 4 {
				copy(h[4:8], syncSafe(len(title)))
			} else {
				binary.BigEndian.PutUint32(h[4:8], uint32(len(title)))
			}
			if version != 0 {
				old = append(h, title...)
			}
			input := audio
			if version != 0 {
				input = append(append(append([]byte{'I', 'D', '3', version, 0, 0}, syncSafe(len(old))...), old...), audio...)
			}
			file := filepath.Join(t.TempDir(), "episode.mp3")
			os.WriteFile(file, input, 0600)
			for pass := 0; pass < 2; pass++ {
				if err := EmbedTranscript(file, "native-asr", tr, cuts); err != nil {
					t.Fatal(err)
				}
			}
			out, _ := os.ReadFile(file)
			size, _ := tagSize(out[6:10])
			if !bytes.Equal(out[size+10:], audio) {
				t.Fatal("audio bytes changed")
			}
			if !bytes.Equal(out[10:10+len(old)], old) {
				t.Fatal("existing metadata changed")
			}
			frame := out[10+len(old) : 10+size]
			if string(frame[:4]) != "GEOB" {
				t.Fatal("missing attachment")
			}
			prefix := []byte("\x00application/json\x00navigatorr-transcript.json\x00" + embeddedDescription + "\x00")
			var bundle EmbeddedTranscript
			if err := json.Unmarshal(bytes.TrimPrefix(frame[10:], prefix), &bundle); err != nil {
				t.Fatal(err)
			}
			if bundle.TranscriptDigest != Digest(tr) || bundle.Timeline != "original_audio" || bundle.ASRJobID != "native-asr" || VerifyCuts(bundle.Transcript, bundle.Cuts) != nil {
				t.Fatal("attachment identity lost")
			}
		})
	}
}
func TestEmbeddingFailsWithoutChangingUnsupportedTag(t *testing.T) {
	tr, p := fixture()
	cuts, _ := Plan(tr, p, decisions(tr, p))
	data := []byte{'I', 'D', '3', 3, 0, 0x80, 0, 0, 0, 0}
	f := filepath.Join(t.TempDir(), "bad.mp3")
	os.WriteFile(f, data, 0600)
	if EmbedTranscript(f, "asr", tr, cuts) == nil {
		t.Fatal("unsupported tag accepted")
	}
	got, _ := os.ReadFile(f)
	if !bytes.Equal(got, data) {
		t.Fatal("original modified")
	}
}
