package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gostream/internal/library"
)

type fakeGoStorm struct {
	files       []library.FileStat
	removedHash string
	addedMagnet string
	addedTitle  string
	addCalls    int
}

func (f *fakeGoStorm) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	f.addCalls++
	f.addedMagnet = magnet
	f.addedTitle = title
	return "abcdef0123456789abcdef0123456789abcdef01", nil
}

func (f *fakeGoStorm) GetTorrentFiles(ctx context.Context, hash string, maxWaitSec int) ([]library.FileStat, error) {
	return f.files, nil
}

func (f *fakeGoStorm) RemoveTorrent(ctx context.Context, hash string) error {
	f.removedHash = hash
	return nil
}

func (f *fakeGoStorm) BaseURL() string { return "http://gostorm.local" }

func postAdd(t *testing.T, h *LibraryHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/library/add", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Add(rr, req)
	return rr
}

func TestLibraryAddEpisode_SelectsRequestedEpisodeFromPack(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	real := t.TempDir()
	fuse := t.TempDir()
	gs := &fakeGoStorm{files: []library.FileStat{
		{ID: 10, Path: "Show.S01E01.mkv", Length: 20 * gb},
		{ID: 11, Path: "Show.S01E02.720p.mkv", Length: 2 * gb},
		{ID: 12, Path: "Show.S01E02.1080p.mkv", Length: 5 * gb},
		{ID: 13, Path: "Show.S01E03.mkv", Length: 25 * gb},
	}}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)

	rr := postAdd(t, h, map[string]any{
		"type":        "episode",
		"title":       "Show",
		"season":      1,
		"episode":     2,
		"series_imdb": "tt123",
		"magnet":      "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp addResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	stub, err := library.ReadStub(resp.StubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.URL, "index=12") {
		t.Fatalf("stub URL %q, want selected target episode index=12", stub.URL)
	}
	if _, err := os.Stat(filepath.Join(real, "tv", "tt123", "Season.01")); err != nil {
		t.Fatalf("expected season directory: %v", err)
	}
}

func TestLibraryAddEpisode_ExistingStubReturns409WithoutTouchingTorrent(t *testing.T) {
	real := t.TempDir()
	fuse := t.TempDir()
	gs := &fakeGoStorm{}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)
	body := map[string]any{
		"type":        "episode",
		"title":       "Show",
		"season":      1,
		"episode":     2,
		"series_imdb": "tt123",
		"magnet":      "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01",
	}
	stubPath := filepath.Join(real, "tv", "tt123", "Season.01", "Show_S01E02_abcdef01.mkv")
	if err := library.WriteStub(stubPath, "http://gostorm.local/stream?link=abcdef0123456789abcdef0123456789abcdef01&index=7&play", 123, body["magnet"].(string), "tt123"); err != nil {
		t.Fatal(err)
	}

	rr := postAdd(t, h, body)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gs.addCalls != 0 {
		t.Fatalf("expected no AddTorrent call, got %d", gs.addCalls)
	}
}

func TestLibraryAddEpisode_NoTargetMatchKeepsReferencedTorrent(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	real := t.TempDir()
	fuse := t.TempDir()
	gs := &fakeGoStorm{files: []library.FileStat{
		{ID: 10, Path: "Show.S01E01.mkv", Length: 5 * gb},
	}}
	sharedStub := filepath.Join(real, "tv", "tt123", "Season.01", "Show_S01E01_abcdef01.mkv")
	magnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"
	if err := library.WriteStub(sharedStub, "http://gostorm.local/stream?link=abcdef0123456789abcdef0123456789abcdef01&index=10&play", 123, magnet, "tt123"); err != nil {
		t.Fatal(err)
	}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)

	rr := postAdd(t, h, map[string]any{
		"type":        "episode",
		"title":       "Show",
		"season":      1,
		"episode":     2,
		"series_imdb": "tt123",
		"magnet":      magnet,
	})

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gs.removedHash != "" {
		t.Fatalf("did not expect cleanup for referenced hash, got %q", gs.removedHash)
	}
}

func TestLibraryAddEpisode_NoTargetMatchReturns422AndRemovesUnreferencedTorrent(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	real := t.TempDir()
	fuse := t.TempDir()
	gs := &fakeGoStorm{files: []library.FileStat{
		{ID: 10, Path: "Show.S01E01.mkv", Length: 5 * gb},
	}}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)

	rr := postAdd(t, h, map[string]any{
		"type":        "episode",
		"title":       "Show",
		"season":      1,
		"episode":     2,
		"series_imdb": "tt123",
		"magnet":      "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01",
	})

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "target_episode_not_found") {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if gs.removedHash == "" {
		t.Fatalf("expected unreferenced torrent cleanup")
	}
}
