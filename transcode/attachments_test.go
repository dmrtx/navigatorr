package transcode

import (
	"strings"
	"testing"
)

// The worker and the coordinator hold an output's attachments to the
// source's by this one rule.
func TestAttachmentChange(t *testing.T) {
	f := func(name, mime string, size int64) AttachedFile { return AttachedFile{name, mime, size} }
	src := []AttachedFile{f("a.ttf", "font/ttf", 10), f("b.ttf", "", 20)}
	for _, tc := range []struct {
		name        string
		source, out []AttachedFile
		want        string
	}{
		{"the same files in another order", src, []AttachedFile{f("b.ttf", "", 20), f("a.ttf", "font/ttf", 10)}, ""},
		{"a type whatever its case", src, []AttachedFile{f("a.ttf", "FONT/TTF", 10), f("b.ttf", "", 20)}, ""},
		{"none", nil, nil, ""},
		// ffmpeg copies an empty type as it is.
		{"an empty type written as another", src, []AttachedFile{f("a.ttf", "font/ttf", 10), f("b.ttf", "application/octet-stream", 20)}, `Attachment "b.ttf" (, 20 bytes) is missing`},
		{"a file missing", src, src[:1], "the source has 2, the output 1"},
		{"an extra file", nil, src[:1], "the source has 0, the output 1"},
		{"a file cut short", src, []AttachedFile{f("a.ttf", "font/ttf", 9), f("b.ttf", "", 20)}, `Attachment "a.ttf" (font/ttf, 10 bytes) is missing`},
		{"a duplicate in place of a file", src, []AttachedFile{f("a.ttf", "font/ttf", 10), f("a.ttf", "font/ttf", 10)}, `Attachment "b.ttf"`},
		// A record made before sizes were kept: name and type only.
		{"a source record without sizes", []AttachedFile{f("a.ttf", "font/ttf", 0)}, []AttachedFile{f("a.ttf", "font/ttf", 999)}, ""},
		{"a source record without sizes, renamed", []AttachedFile{f("a.ttf", "font/ttf", 0)}, []AttachedFile{f("c.ttf", "font/ttf", 999)}, `Attachment "a.ttf" (font/ttf, 0 bytes) is missing`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AttachmentChange(tc.source, tc.out)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
