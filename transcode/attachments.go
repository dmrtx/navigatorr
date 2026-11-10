package transcode

import (
	"fmt"
	"strings"
)

// AttachedFile is an attachment (a font, mostly) as a probe shows it: its
// file name, MIME type and size in bytes.
type AttachedFile struct {
	Name, MimeType string
	Size           int64
}

// AttachmentChange holds an output's attachments to its source's and
// describes the first difference, or returns "": as many files, and every
// source file among them by name, type and size, however many of each. The
// worker's check and the coordinator's both use it.
//
// ffmpeg copies an attachment's name and type as they are, an empty type
// included, so both are compared exactly (the type whatever its case). A
// source record made before sizes were kept has none (every Size 0); its
// files are compared by name and type.
//
// Cover art is not an attachment here. Matroska reads a re-attached JPEG,
// PNG, GIF or TIFF cover back as a picture, which the cover art check
// counts; a BMP or WebP one comes back as an attachment, and that check has
// already failed it for a missing cover.
func AttachmentChange(source, output []AttachedFile) string {
	if len(output) != len(source) {
		return fmt.Sprintf("Attachment count changed: the source has %d, the output %d", len(source), len(output))
	}
	sized := false
	for _, f := range source {
		if f.Size > 0 {
			sized = true
		}
	}
	key := func(f AttachedFile) AttachedFile {
		f.MimeType = strings.ToLower(f.MimeType)
		if !sized {
			f.Size = 0
		}
		return f
	}
	count := map[AttachedFile]int{}
	for _, f := range output {
		count[key(f)]++
	}
	// The first source file missing is reported, in file order.
	for _, f := range source {
		k := key(f)
		if count[k] == 0 {
			return fmt.Sprintf("Attachment %q (%s, %d bytes) is missing from the output", f.Name, f.MimeType, f.Size)
		}
		count[k]--
	}
	return ""
}
