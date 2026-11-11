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
// counts; a BMP or WebP one comes back as an attachment, which
// TakeReattachedCovers sets aside before this comparison.
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

// CoverMimeTypes maps an attached picture's codec to the MIME type its cover
// is re-attached with. Other codecs are refused before the encode.
var CoverMimeTypes = map[string]string{"mjpeg": "image/jpeg", "png": "image/png", "bmp": "image/bmp", "gif": "image/gif", "webp": "image/webp", "tiff": "image/tiff"}

// CoverReadsBackAsPicture reports whether a cover re-attached to Matroska
// with this MIME type is probed back as a picture (attached_pic). ffmpeg's
// Matroska reader knows JPEG, PNG, GIF and TIFF; a BMP or WebP attachment
// is read back as a plain attachment.
func CoverReadsBackAsPicture(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/png", "image/gif", "image/tiff":
		return true
	}
	return false
}

// TakeReattachedCovers finds, among an output's attachments, the covers that
// were re-attached as attachments (those CoverReadsBackAsPicture is false
// for): one file per cover, of its MIME type and, when the cover has a name,
// that name. Files that exactly match a source attachment are left to the
// attachment check. It returns the output's other attachments, or a message
// naming the first cover that is missing. The worker's check and the
// coordinator's both use it.
func TakeReattachedCovers(covers, source, output []AttachedFile) (rest []AttachedFile, missing string) {
	taken := make([]bool, len(output))
	for _, s := range source {
		for i, o := range output {
			if !taken[i] && o.Name == s.Name && strings.EqualFold(o.MimeType, s.MimeType) && (s.Size == 0 || o.Size == s.Size) {
				taken[i] = true
				break
			}
		}
	}
	covered := make([]bool, len(output))
	for _, c := range covers {
		found := false
		for i, o := range output {
			if taken[i] || covered[i] || !strings.EqualFold(o.MimeType, c.MimeType) || (c.Name != "" && o.Name != c.Name) {
				continue
			}
			covered[i], found = true, true
			break
		}
		if !found {
			return nil, fmt.Sprintf("Cover art %q (%s) is missing from the output", c.Name, c.MimeType)
		}
	}
	for i, o := range output {
		if !covered[i] {
			rest = append(rest, o)
		}
	}
	return rest, ""
}
