package maintenanceui

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/mediainspect"
)

type tracksResponse struct {
	Languages []string    `json:"languages"`
	Audio     []trackView `json:"audio"`
	Subtitles []trackView `json:"subtitles"`
	Error     string      `json:"error"`
	Code      string      `json:"code"`
	Field     string      `json:"field"`
}

// tracksFixture serves one fake video with the given audio and subtitle
// streams from an allow-listed temp root, probed by a fake.
func tracksFixture(t *testing.T, audio, subtitles []mediainspect.DetailedStream) (func(query string) (int, tracksResponse), string, *Server) {
	t.Helper()
	s, h := testUI(t)
	root := t.TempDir()
	video := filepath.Join(root, "Sample Episode.mkv")
	if err := os.WriteFile(video, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	s.probe = func(_ context.Context, _, path string) (mediainspect.DetailedReport, error) {
		return mediainspect.DetailedReport{Path: path, Probed: true, Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}}, Audio: audio, Subtitles: subtitles}, nil
	}
	return func(query string) (int, tracksResponse) {
		w := request(h, "GET", "/api/maintenance/source-tracks?"+query, "", true)
		var out tracksResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return w.Code, out
	}, video, s
}

func sampleTracks() (audio, subtitles []mediainspect.DetailedStream) {
	audio = []mediainspect.DetailedStream{
		{Index: 1, Kind: "audio", Codec: "eac3", Language: "eng", Title: "Surround", Channels: 6, ChannelLayout: "5.1(side)", Disposition: map[string]int{"default": 1}},
		{Index: 2, Kind: "audio", Codec: "ac3", Language: "fre", Channels: 6},
		{Index: 3, Kind: "audio", Codec: "aac", Channels: 2},
		{Index: 4, Kind: "audio", Codec: "dts", Language: "jpn", Channels: 6},
	}
	subtitles = []mediainspect.DetailedStream{
		{Index: 5, Kind: "subtitle", Codec: "subrip", Language: "spa"},
		{Index: 6, Kind: "subtitle", Codec: "subrip", Language: "ger", Disposition: map[string]int{"forced": 1}},
		{Index: 7, Kind: "subtitle", Codec: "subrip"},
	}
	return audio, subtitles
}

func keepOf(tracks []trackView) []bool {
	out := make([]bool, len(tracks))
	for i, t := range tracks {
		out[i] = t.Keep
	}
	return out
}

func TestSourceTracksFollowTheSavedListUnlessTheJobGivesOne(t *testing.T) {
	audio, subs := sampleTracks()
	get, video, s := tracksFixture(t, audio, subs)
	q := "path=" + url.QueryEscape(video)

	// The default list is spa, eng, jpn: French audio goes, untagged audio
	// stays, and so do the untagged and the Spanish subtitles.
	code, out := get(q)
	if code != 200 || !reflect.DeepEqual(out.Languages, []string{"eng", "jpn", "spa"}) {
		t.Fatal(code, out)
	}
	if got, want := keepOf(out.Audio), []bool{true, false, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audio keep %v, want %v", got, want)
	}
	if got, want := keepOf(out.Subtitles), []bool{true, false, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subtitle keep %v, want %v", got, want)
	}
	first := out.Audio[0]
	if first.Index != 1 || first.Language != "eng" || first.Title != "Surround" || first.Codec != "eac3" || first.Channels != 6 || first.ChannelLayout != "5.1(side)" || !first.Default || !first.Original {
		t.Fatalf("track fields: %+v", first)
	}
	if out.Audio[2].Language != "und" || out.Audio[1].Original || out.Subtitles[1].Forced != true {
		t.Fatalf("untagged tracks say und and only the first audio is original: %+v", out)
	}

	// The job's own list replaces the setting; the original track is kept
	// whatever the list says, and ger is deu.
	_, out = get(q + "&keep_languages=deu")
	if !reflect.DeepEqual(out.Languages, []string{"deu"}) {
		t.Fatalf("languages %v", out.Languages)
	}
	if got, want := keepOf(out.Audio), []bool{true, true, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no audio matches keeps every track: %v", got)
	}
	if got, want := keepOf(out.Subtitles), []bool{false, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deu and untagged subtitles stay, spa goes (the original is eng): %v", got)
	}
	_, out = get(q + "&keep_languages=fra,jpn")
	if got, want := keepOf(out.Audio), []bool{true, true, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audio keep %v", got)
	}
	if got, want := keepOf(out.Subtitles), []bool{false, false, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subtitle keep %v", got)
	}

	// Empty means every track, as it does on a job.
	_, out = get(q + "&keep_languages=")
	if len(out.Languages) != 0 || !reflect.DeepEqual(keepOf(out.Subtitles), []bool{true, true, true}) {
		t.Fatalf("empty list: %+v", out)
	}

	// A saved setting is what an unqualified read follows.
	if _, err := s.engine.Deps().Store.SetKeepLanguages([]string{"fra"}); err != nil {
		t.Fatal(err)
	}
	_, out = get(q)
	if !reflect.DeepEqual(out.Languages, []string{"fra"}) || !reflect.DeepEqual(keepOf(out.Audio), []bool{true, true, true, false}) {
		t.Fatalf("saved list: %+v", out)
	}
}

func TestSourceTracksRejectsBadLanguagesAndPaths(t *testing.T) {
	audio, subs := sampleTracks()
	get, video, _ := tracksFixture(t, audio, subs)
	code, out := get("path=" + url.QueryEscape(video) + "&keep_languages=eng,english")
	if code != 400 || out.Code != "invalid_params" || out.Field != "keep_languages" {
		t.Fatal(code, out)
	}
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	os.WriteFile(outside, []byte("fixture"), 0600)
	for _, path := range []string{outside, "/etc/passwd", "../../etc/passwd", "", filepath.Dir(video)} {
		if code, _ := get("path=" + url.QueryEscape(path)); code != 400 {
			t.Fatalf("%q: %d", path, code)
		}
	}
}

func TestSourceTracksRequiresSignInAndNamesNoPath(t *testing.T) {
	audio, subs := sampleTracks()
	_, video, s := tracksFixture(t, audio, subs)
	q := "/api/maintenance/source-tracks?path=" + url.QueryEscape(video)
	if w := request(s.Handler(), "GET", q, "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(s.Handler(), "GET", q, "", true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Sample Episode") || strings.Contains(w.Body.String(), filepath.Dir(video)) {
		t.Fatal("the response names streams only, never the file's location:", w.Body.String())
	}
}

func TestSourceTracksProbeFailureIsAHandledStatus(t *testing.T) {
	audio, subs := sampleTracks()
	_, video, s := tracksFixture(t, audio, subs)
	s.probe = func(_ context.Context, _, path string) (mediainspect.DetailedReport, error) {
		return mediainspect.DetailedReport{Path: path}, context.Canceled
	}
	w := request(s.Handler(), "GET", "/api/maintenance/source-tracks?path="+url.QueryEscape(video), "", true)
	if w.Code != 422 {
		t.Fatal(w.Code, w.Body.String())
	}
}
