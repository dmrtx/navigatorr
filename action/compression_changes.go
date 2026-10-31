package action

import "github.com/jakenesler/navigatorr/transcode"

// compressionChanges is what changed in how files are compressed, newest
// first. Every policy version a job can freeze has an entry (see the test), so
// a new version cannot ship without saying what it changed.
var compressionChanges = []CompressionChange{
	{Date: "2026-10-31", Text: "Shrink can keep 4K: HDR becomes SDR at 4K. SDR 4K files are recommended Save space, which keeps their resolution."},
	{Date: "2026-10-31", Text: "A file that ends without a result says what happened and offers next steps from what it measured: Audio only, a size limit, x265 or Shrink."},
	{Date: "2026-10-31", Version: transcode.ModePolicyV7, Text: "Audio only: the video is copied unchanged, 4K and HDR included, and only the audio is converted; any smaller file is kept."},
	{Date: "2026-10-28", Text: "Save space converts lossy Atmos (E-AC-3) to AAC on its channel bed, as it already did lossless Atmos."},
	{Date: "2026-10-28", Text: "Save space converts any lossy track at least 1.25 times its AAC target, not only lossless audio."},
	{Date: "2026-10-22", Version: transcode.ModePolicyV6, Text: "Save space and Shrink keep only the audio and subtitles in the languages to keep, the original language and untagged tracks."},
	{Date: "2026-10-22", Version: transcode.ModePolicyV5, Text: "A size limit plans the audio per file: AAC-LC, or HE-AAC when AAC-LC would leave too little for the video."},
	{Date: "2026-10-21", Version: ModePolicyV4, Text: "Every goal encodes the whole file at a short ladder of fixed settings instead of searching on samples; each encode is checked on windows spread across the file. x265 after the media engine misses is now a next step you choose, not automatic."},
	{Date: "2026-10-20", Text: "Save space can aim for a size limit: a percent of the original, a size, or like other episodes."},
	{Date: "2026-10-19", Text: "Save space encodes on the Apple media engine first, with x265 when it cannot pass."},
	{Date: "2026-10-18", Version: transcode.ModePolicyV3, Text: "Save space converts lossless audio to AAC and may encode 8-bit sources as 10-bit HEVC."},
	{Date: "2026-10-18", Text: "Shrink: HDR, Dolby Vision and 4K become SDR 1080p, at least 30% smaller."},
	{Date: "2026-10-11", Version: transcode.ModePolicyV2, Text: "Goals replace hand-tuned profiles: each freezes its policy with the job, so a later change never alters a job already planned."},
}
