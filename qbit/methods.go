package qbit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// ListTorrents returns all torrents.
func (c *Client) ListTorrents(ctx context.Context) ([]TorrentInfo, error) {
	data, err := c.do(ctx, "GET", "/api/v2/torrents/info", nil)
	if err != nil {
		return nil, err
	}

	var torrents []TorrentInfo
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, fmt.Errorf("decoding torrents: %w", err)
	}
	return torrents, nil
}

// GetTorrent retrieves a specific torrent by hash.
func (c *Client) GetTorrent(ctx context.Context, hash string) (*TorrentInfo, error) {
	if hash == "" {
		return nil, fmt.Errorf("hash is required")
	}
	data, err := c.do(ctx, "GET", "/api/v2/torrents/info?hashes="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, err
	}

	var torrents []TorrentInfo
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, fmt.Errorf("decoding torrent: %w", err)
	}
	if len(torrents) == 0 {
		return nil, fmt.Errorf("torrent %q not found", hash)
	}
	return &torrents[0], nil
}

// AddTorrent adds a torrent by magnet link or URL.
func (c *Client) AddTorrent(ctx context.Context, urls string, savePath string) error {
	form := url.Values{
		"urls": {urls},
	}
	if savePath != "" {
		form.Set("savepath", savePath)
	}

	_, err := c.do(ctx, "POST", "/api/v2/torrents/add", form)
	return err
}

// PauseTorrents pauses torrents by hash.
func (c *Client) PauseTorrents(ctx context.Context, hashes []string) error {
	form := url.Values{
		"hashes": {strings.Join(hashes, "|")},
	}
	return c.doRenamed(ctx, form, "stop", "pause")
}

// ResumeTorrents resumes torrents by hash.
func (c *Client) ResumeTorrents(ctx context.Context, hashes []string) error {
	form := url.Values{
		"hashes": {strings.Join(hashes, "|")},
	}
	return c.doRenamed(ctx, form, "start", "resume")
}

// doRenamed calls a torrents command under the name qBittorrent 5 gave it
// and, when the server does not know it (404), under the older name.
func (c *Client) doRenamed(ctx context.Context, form url.Values, current, legacy string) error {
	_, err := c.do(ctx, "POST", "/api/v2/torrents/"+current, form)
	if err != nil && strings.HasPrefix(err.Error(), "HTTP 404") {
		_, err = c.do(ctx, "POST", "/api/v2/torrents/"+legacy, form)
	}
	return err
}

// DeleteTorrents deletes torrents by hash, optionally deleting files.
func (c *Client) DeleteTorrents(ctx context.Context, hashes []string, deleteFiles bool) error {
	form := url.Values{
		"hashes":      {strings.Join(hashes, "|")},
		"deleteFiles": {fmt.Sprintf("%t", deleteFiles)},
	}
	_, err := c.do(ctx, "POST", "/api/v2/torrents/delete", form)
	return err
}

// ListFiles returns the files contained in a torrent by hash. Inspecting
// this list before trusting a download is the torrent-content safety gate.
func (c *Client) ListFiles(ctx context.Context, hash string) ([]TorrentFile, error) {
	if hash == "" {
		return nil, fmt.Errorf("hash is required")
	}
	data, err := c.do(ctx, "GET", "/api/v2/torrents/files?hash="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, err
	}

	var files []TorrentFile
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("decoding torrent files: %w", err)
	}
	return files, nil
}

// GetTransferInfo returns global transfer statistics.
func (c *Client) GetTransferInfo(ctx context.Context) (*TransferInfo, error) {
	data, err := c.do(ctx, "GET", "/api/v2/transfer/info", nil)
	if err != nil {
		return nil, err
	}

	var info TransferInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("decoding transfer info: %w", err)
	}
	return &info, nil
}

// CategorySavePaths returns the save path of every category that sets one.
func (c *Client) CategorySavePaths(ctx context.Context) ([]string, error) {
	data, err := c.do(ctx, "GET", "/api/v2/torrents/categories", nil)
	if err != nil {
		return nil, err
	}
	var cats map[string]struct {
		SavePath string `json:"savePath"`
	}
	if err := json.Unmarshal(data, &cats); err != nil {
		return nil, fmt.Errorf("decoding categories: %w", err)
	}
	var out []string
	for _, cat := range cats {
		if cat.SavePath != "" {
			out = append(out, cat.SavePath)
		}
	}
	return out, nil
}

// DefaultPaths returns the default save path and, when enabled, the folder for
// incomplete downloads.
func (c *Client) DefaultPaths(ctx context.Context) ([]string, error) {
	data, err := c.do(ctx, "GET", "/api/v2/app/preferences", nil)
	if err != nil {
		return nil, err
	}
	var p struct {
		SavePath        string `json:"save_path"`
		TempPath        string `json:"temp_path"`
		TempPathEnabled bool   `json:"temp_path_enabled"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("decoding preferences: %w", err)
	}
	var out []string
	if p.SavePath != "" {
		out = append(out, p.SavePath)
	}
	if p.TempPathEnabled && p.TempPath != "" {
		out = append(out, p.TempPath)
	}
	return out, nil
}
