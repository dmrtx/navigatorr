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
	_, err := c.do(ctx, "POST", "/api/v2/torrents/pause", form)
	return err
}

// ResumeTorrents resumes torrents by hash.
func (c *Client) ResumeTorrents(ctx context.Context, hashes []string) error {
	form := url.Values{
		"hashes": {strings.Join(hashes, "|")},
	}
	_, err := c.do(ctx, "POST", "/api/v2/torrents/resume", form)
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
