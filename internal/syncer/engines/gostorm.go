package engines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gostream/internal/library"
)

// GoStormClient handles HTTP operations with the GoStorm engine.
type GoStormClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewGoStormClient creates a client for GoStorm API operations.
func NewGoStormClient(baseURL string) *GoStormClient {
	return &GoStormClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

// BaseURL returns the configured GoStorm base URL (no trailing slash).
func (c *GoStormClient) BaseURL() string {
	return c.baseURL
}

// GetTorrentFiles is a thin wrapper around GetTorrentInfo that returns
// just the file list. Provided so that the dashboard package (which
// must not import engines directly) can talk to the gostorm client
// through a small interface.
func (c *GoStormClient) GetTorrentFiles(ctx context.Context, hash string, maxWaitSec int) ([]FileStat, error) {
	info, err := c.GetTorrentInfo(ctx, hash, maxWaitSec)
	if err != nil {
		return nil, err
	}
	return info.FileStats, nil
}

// TorrentStats holds torrent information from GoStorm.
type TorrentStats struct {
	Hash        string     `json:"hash"`
	Title       string     `json:"title"`
	Length      int64      `json:"length"`
	ActivePeers int        `json:"active_peers"`
	FileStats   []FileStat `json:"file_stats"`
}

// FileStat holds file information from GoStorm. Aliased to the
// internal/library type so that both the engines and the HTTP
// /api/library/add handler operate on the same value type without
// needing per-call conversion.
type FileStat = library.FileStat

// AddTorrent adds a magnet URL to GoStorm via POST /torrents {"action":"add"}.
// Returns the 40-char info hash or empty string on failure.
func (c *GoStormClient) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	m := regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{32,40})`)
	match := m.FindStringSubmatch(magnet)
	if len(match) < 2 {
		return "", fmt.Errorf("cannot extract hash from magnet")
	}
	hash := strings.ToLower(match[1])

	body := map[string]interface{}{
		"action": "add",
		"link":   magnet,
		"title":  title,
		"save":   true,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrents", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gostorm error %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 120)]))
	}

	// Response contains the torrent object with hash
	var result struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Hash != "" {
		return strings.ToLower(result.Hash), nil
	}

	return hash, nil
}

// GetTorrentInfo polls GoStorm until file_stats appear.
func (c *GoStormClient) GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error) {
	sleepSeq := []int{1, 2, 3, 3, 3, 5}
	deadline := time.Now().Add(time.Duration(maxWait) * time.Second)
	attempt := 0

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		info, err := c.getTorrent(ctx, hash)
		if err == nil && len(info.FileStats) > 0 {
			return info, nil
		}

		sleep := 3
		if attempt < len(sleepSeq) {
			sleep = sleepSeq[attempt]
		}
		time.Sleep(time.Duration(sleep) * time.Second)
		attempt++
	}

	return nil, fmt.Errorf("metadata timeout for %s… (waited %ds)", hash[:8], maxWait)
}

// RemoveTorrent removes a torrent from GoStorm.
func (c *GoStormClient) RemoveTorrent(ctx context.Context, hash string) error {
	body := map[string]string{"action": "rem", "hash": hash}
	return c.postTorrents(ctx, body)
}

// ProbeAudio uses ffprobe against the gostream file stream and returns all
// audio streams with stable container stream indexes.
func (c *GoStormClient) ProbeAudio(ctx context.Context, hash string, fileID int, maxWaitSec int) ([]library.AudioTrack, error) {
	if maxWaitSec <= 0 {
		maxWaitSec = 45
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(maxWaitSec)*time.Second)
	defer cancel()
	streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", c.baseURL, url.QueryEscape(hash), fileID)
	cmd := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=index,codec_name,channels:stream_tags=language,title:stream_disposition=default", "-of", "json", streamURL)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var raw struct {
		Streams []struct {
			Index       int               `json:"index"`
			CodecName   string            `json:"codec_name"`
			Channels    int               `json:"channels"`
			Tags        map[string]string `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	tracks := make([]library.AudioTrack, 0, len(raw.Streams))
	for _, stream := range raw.Streams {
		tracks = append(tracks, library.AudioTrack{
			StreamIndex: stream.Index,
			Language:    library.NormalizeAudioLanguage(stream.Tags["language"]),
			Title:       stream.Tags["title"],
			Codec:       stream.CodecName,
			Channels:    stream.Channels,
			Default:     stream.Disposition.Default == 1,
		})
	}
	return tracks, nil
}

// ListTorrents returns all active torrents.
func (c *GoStormClient) ListTorrents(ctx context.Context) ([]TorrentStats, error) {
	body := map[string]string{"action": "list"}
	data, err := c.doTorrents(ctx, body)
	if err != nil {
		return nil, err
	}

	var torrents []TorrentStats
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, err
	}
	return torrents, nil
}

func (c *GoStormClient) getTorrent(ctx context.Context, hash string) (*TorrentStats, error) {
	body := map[string]string{"action": "get", "hash": hash}
	data, err := c.doTorrents(ctx, body)
	if err != nil {
		return nil, err
	}

	var info TorrentStats
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *GoStormClient) doTorrents(ctx context.Context, body map[string]string) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrents", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gostorm /torrents: status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (c *GoStormClient) postTorrents(ctx context.Context, body map[string]string) error {
	_, err := c.doTorrents(ctx, body)
	return err
}

// TitleFromFilename extracts a clean display title from an MKV filename.
// e.g. "Your_Friends_Neighbors_S01E09_17361ba1.mkv" → "Your Friends Neighbors S01E09"
func TitleFromFilename(filename string) string {
	s := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Remove trailing _hash8 (8 hex chars)
	if re := regexp.MustCompile(`_[a-f0-9]{8}$`); re.MatchString(s) {
		s = s[:len(s)-9]
	}
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")
	return strings.TrimSpace(s)
}

// BuildMagnet creates a magnet URL from an info hash and optional trackers.
func BuildMagnet(infoHash, name string, trackers []string) string {
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
	if name != "" {
		magnet += fmt.Sprintf("&dn=%s", url.QueryEscape(name))
	}
	for _, tr := range trackers {
		magnet += fmt.Sprintf("&tr=%s", url.QueryEscape(tr))
	}
	return magnet
}

// DefaultTrackers returns the fallback tracker list.
func DefaultTrackers() []string {
	return []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.stealth.si:80/announce",
		"udp://tracker.torrent.eu.org:451/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
