package mediafile

import "testing"

func TestIsDangerousFilename(t *testing.T) {
	for _, n := range []string{
		"Far.Orbit.S02E03.1080p.mkv.exe", "movie.exe", "x.com", "run.bat",
		"Season 2/Episode.mkv.EXE", "payload.com.mkv", "setup.exe.scr",
	} {
		if !IsDangerousFilename(n) {
			t.Errorf("%q should be dangerous", n)
		}
	}
	for _, n := range []string{
		"Movie.2024.1080p.BluRay.mp4", "subs/Show.eng.srt",
		"YTSProxies.com.txt", "YIFYStatus.com.txt", "www.YTS.MX.jpg",
		"Release.NFO", "folder/Some.Site.com.nfo",
	} {
		if IsDangerousFilename(n) {
			t.Errorf("%q should be safe", n)
		}
	}
}
