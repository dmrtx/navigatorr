package action

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/jakenesler/navigatorr/arrservice"
)

func (p *promotionState) mediaEndpoint() string {
	if p.MovieID > 0 {
		return "/api/v3/movie/" + strconv.Itoa(p.MovieID)
	}
	return "/api/v3/series/" + strconv.Itoa(p.SeriesID)
}
func (p *promotionState) fileEndpoint(id int) string {
	if p.MovieID > 0 {
		return "/api/v3/moviefile/" + strconv.Itoa(id)
	}
	return "/api/v3/episodefile/" + strconv.Itoa(id)
}

// Map Radarr's one movie/file association to the shared promotion invariants.
// Series-specific IDs remain internal here; UI projections expose movie IDs.
func (e *Engine) promotionMovieSnapshot(ctx context.Context, svc *arrservice.Service, p *promotionState) (*promotionLibrary, error) {
	var movie struct {
		ID          int    `json:"id"`
		Path        string `json:"path"`
		MovieFileID int    `json:"movieFileId"`
	}
	b, err := svc.Get(ctx, p.mediaEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(b, &movie) != nil || movie.ID != p.MovieID || filepath.Clean(movie.Path) != p.SeriesPath {
		return nil, fmt.Errorf("Radarr movie identity or library path changed")
	}
	b, err = svc.Get(ctx, "/api/v3/moviefile", map[string]string{"movieId": strconv.Itoa(p.MovieID)})
	if err != nil {
		return nil, err
	}
	var files []promotionFile
	if err = json.Unmarshal(b, &files); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for i := range files {
		f := &files[i]
		if f.ID <= 0 || f.MovieID != p.MovieID || seen[f.ID] {
			return nil, fmt.Errorf("invalid or duplicate Radarr movieFile")
		}
		seen[f.ID] = true
		f.SeriesID = p.MovieID
		if f.Path == "" && f.RelativePath != "" {
			f.Path = filepath.Join(p.SeriesPath, f.RelativePath)
		}
		if !withinPromotionPath(p.SeriesPath, f.Path) {
			return nil, fmt.Errorf("Radarr returned a file outside the approved movie root")
		}
	}
	return &promotionLibrary{Files: files, Episodes: []promotionEpisode{{ID: p.MovieID, SeriesID: p.MovieID, EpisodeFileID: movie.MovieFileID}}}, nil
}
