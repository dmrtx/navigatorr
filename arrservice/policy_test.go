package arrservice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/config"
)

func TestPlexPolicyReadOnlyByDefault(t *testing.T) {
	p := newPlexPolicy("plex", nil)
	allowed := [][2]string{
		{"GET", "/identity"},
		{"GET", "/library/sections"},
		{"GET", "/library/sections/3"},
		{"GET", "/library/sections/3/all"},
		{"GET", "/library/sections/3/all?type=4"},
		{"GET", "/Library/Sections/3/All"},
		{"GET", "/library/sections/3/firstCharacter"},
		{"GET", "/library/metadata/12345"},
		{"GET", "/library/metadata/12,13/children"},
		{"GET", "/library/metadata/12345/allLeaves"},
		{"GET", "/library/metadata/12345/matches"},
		{"HEAD", "/library/sections"},
		{"GET", "library/sections/"},
		{"GET", "/hubs/search"},
		{"GET", "/status/sessions"},
		{"GET", "/system/agents"},
	}
	for _, c := range allowed {
		if err := p.Check(c[0], c[1], nil); err != nil {
			t.Errorf("%s %s should be allowed: %v", c[0], c[1], err)
		}
	}
	denied := [][2]string{
		// GET routes that change something, or echo secrets
		{"GET", "/library/sections/3/refresh"},
		{"GET", "/library/sections/3/refresh?force=1"},
		{"GET", "/library/sections/3/emptyTrash"},
		{"GET", "/library/sections/3/analyze"},
		{"GET", "/library/optimize"},
		{"GET", "/library/metadata/12345/refresh"},
		{"GET", "/library/metadata/12345/match"},
		{"GET", "/:/scrobble"},
		{"GET", "/:/prefs"},
		{"GET", "/myplex/account"},
		{"GET", "/security/token"},
		{"GET", "/accounts"},
		{"GET", "/butler"},
		// writes and deletes, switched off
		{"POST", "/library/sections"},
		{"PUT", "/library/sections/3"},
		{"PUT", "/library/sections/3/refresh"},
		{"PUT", "/library/metadata/12345/match"},
		{"PUT", "/library/metadata/12345"},
		{"PUT", "/library/metadata/12345/refresh"},
		{"DELETE", "/library/sections/3"},
		{"DELETE", "/library/metadata/12345"},
		{"PUT", "/library/sections/3/emptyTrash"},
		// path tricks
		{"GET", "/library/sections/3/all/../refresh"},
		{"GET", "/library/sections/%33/refresh"},
		{"GET", "/library//sections"},
		{"GET", "/library/sections/3/all\\..\\refresh"},
		{"GET", "/library/sections#/3/refresh"},
		{"GET", "/library/sections/3/all\n"},
		{"GET", "/library/sections/3;x/all"},
		{"GET", "/identity/../myplex/account"},
	}
	for _, c := range denied {
		err := p.Check(c[0], c[1], nil)
		var pe *PolicyError
		if !errors.As(err, &pe) {
			t.Errorf("%s %q should be blocked, got %v", c[0], c[1], err)
		}
	}
}

func TestPlexPolicyRejectsTokenAndMethodOverrideQuery(t *testing.T) {
	p := newPlexPolicy("plex", []string{config.PlexWriteRefresh})
	if err := p.Check("GET", "/library/sections", map[string]string{"X-Plex-Token": "x"}); err == nil {
		t.Error("token in query must be blocked")
	}
	if err := p.Check("GET", "/library/sections?_method=DELETE", nil); err == nil {
		t.Error("_method in path query must be blocked")
	}
	if err := p.Check("GET", "/library/sections", map[string]string{"_method": "DELETE"}); err == nil {
		t.Error("_method in query must be blocked")
	}
}

func TestPlexPolicyWriteGroups(t *testing.T) {
	cases := []struct {
		group, method, path string
	}{
		{config.PlexWriteRefresh, "GET", "/library/sections/3/refresh"},
		{config.PlexWriteRefresh, "PUT", "/library/metadata/9/refresh"},
		{config.PlexWriteLibraryNew, "POST", "/library/sections"},
		{config.PlexWriteLibraryEdit, "PUT", "/library/sections/3"},
		{config.PlexWriteLibraryEdit, "PUT", "/library/sections/3/prefs"},
		{config.PlexWriteMatch, "PUT", "/library/metadata/9/match"},
		{config.PlexWriteMatch, "PUT", "/library/metadata/9/unmatch"},
		{config.PlexWriteMetaEdit, "PUT", "/library/metadata/9"},
	}
	for _, c := range cases {
		if err := newPlexPolicy("plex", nil).Check(c.method, c.path, nil); err == nil {
			t.Errorf("%s %s must be off without %s", c.method, c.path, c.group)
		}
		on := newPlexPolicy("plex", []string{c.group})
		if err := on.Check(c.method, c.path, nil); err != nil {
			t.Errorf("%s %s should pass with %s: %v", c.method, c.path, c.group, err)
		}
		// a different group never opens it
		other := config.PlexWriteGroups[0]
		if other == c.group {
			other = config.PlexWriteGroups[1]
		}
		if err := newPlexPolicy("plex", []string{other}).Check(c.method, c.path, nil); err == nil {
			t.Errorf("%s %s must stay blocked with only %s", c.method, c.path, other)
		}
	}
	// Even with every group on, deletes and trash stay out.
	all := newPlexPolicy("plex", config.PlexWriteGroups)
	for _, c := range [][2]string{
		{"DELETE", "/library/sections/3"},
		{"DELETE", "/library/metadata/9"},
		{"PUT", "/library/sections/3/emptyTrash"},
		{"GET", "/library/sections/3/emptyTrash"},
		{"POST", "/library/sections/3"},
		{"GET", "/myplex/account"},
	} {
		if err := all.Check(c[0], c[1], nil); err == nil {
			t.Errorf("%s %s must never be allowed", c[0], c[1])
		}
	}
}

// The policy sits in the client, so a request it blocks never reaches Plex.
func TestPlexServiceBlocksBeforeTheNetwork(t *testing.T) {
	var hits atomic.Int32
	var gotToken, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotToken = r.Header.Get("X-Plex-Token")
		gotAccept = r.Header.Get("Accept")
		w.Write([]byte(`{"MediaContainer":{}}`))
	}))
	defer srv.Close()

	svc := NewService("plex", config.ServiceConfig{
		URL:        srv.URL,
		APIKey:     "tok-12345678",
		AuthMethod: "header",
		AuthHeader: config.DefaultAuthHeaders["plex"],
		Policy:     config.PolicyPlex,
	})
	ctx := context.Background()

	if _, _, err := svc.DoRequest(ctx, "GET", "/library/sections/3/refresh", nil, nil); err == nil {
		t.Fatal("refresh must be blocked")
	}
	if _, _, err := svc.DoRequest(ctx, "DELETE", "/library/sections/3", nil, nil); err == nil {
		t.Fatal("delete must be blocked")
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked requests reached the server %d times", hits.Load())
	}
	if _, code, err := svc.DoRequest(ctx, "GET", "/library/sections", nil, nil); err != nil || code != 200 {
		t.Fatalf("read failed: %v %d", err, code)
	}
	if gotToken != "tok-12345678" || gotAccept != "application/json" {
		t.Errorf("token %q accept %q", gotToken, gotAccept)
	}
}
