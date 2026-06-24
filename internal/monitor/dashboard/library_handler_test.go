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
	getCalls    int
}

func (f *fakeGoStorm) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	f.addCalls++
	f.addedMagnet = magnet
	f.addedTitle = title
	return library.HashFromMagnet(magnet), nil
}

func (f *fakeGoStorm) GetTorrentFiles(ctx context.Context, hash string, maxWaitSec int) ([]library.FileStat, error) {
	f.getCalls++
	return f.files, nil
}

func (f *fakeGoStorm) RemoveTorrent(ctx context.Context, hash string) error {
	f.removedHash = hash
	return nil
}

func (f *fakeGoStorm) BaseURL() string { return "http://gostorm.local" }

func postRemove(t *testing.T, h *LibraryHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/library/remove", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Remove(rr, req)
	return rr
}

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

func TestLibraryAddEpisode_ExactAvatarSeriesPackSelectsS02E03NotLargestFile(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	real := t.TempDir()
	fuse := t.TempDir()
	gs := &fakeGoStorm{files: []library.FileStat{
		{ID: 23, Path: "[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 2 - Earth (Livre 2 - La terre)/[Avatar Realms] Avatar The Last Airbender 2x03 Return to Omashu [x264 FHD multi sub FR].mkv", Length: 1142880878},
		{ID: 31, Path: "[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 2 - Earth (Livre 2 - La terre)/[Avatar Realms] Avatar The Last Airbender 2x11 The Desert  [x264 FHD multi sub FR].mkv", Length: 1175677822},
		{ID: 57, Path: "[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 3 - Fire (Livre 3 - Le feu)/[Avatar Realms] Avatar The Last Airbender 3x17 The Ember Island Players [x264 FHD multi sub FR].mkv", Length: 2 * gb},
	}}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)

	rr := postAdd(t, h, map[string]any{
		"type":        "episode",
		"title":       "Avatar The Last Airbender",
		"season":      2,
		"episode":     3,
		"series_imdb": "tt0417299",
		"magnet":      "magnet:?xt=urn:btih:" + hash + "&dn=Avatar%3A%20The%20Last%20Airbender%20-%20S01%20to%20S03%20-%201080p%20-%20Bluray%20AAC5.1%20-%20X264-Rapta",
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
	if !strings.Contains(stub.URL, "index=23") {
		t.Fatalf("stub URL %q, want target episode index=23", stub.URL)
	}
	if strings.Contains(stub.URL, "index=31") || stub.Size == 1175677822 {
		t.Fatalf("stub selected largest wrong episode: %+v", stub)
	}
}

func TestLibraryAddEpisode_ReplacesExistingZeroSizePackStub(t *testing.T) {
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	real := t.TempDir()
	fuse := t.TempDir()
	magnet := "magnet:?xt=urn:btih:" + hash
	gs := &fakeGoStorm{files: []library.FileStat{{ID: 23, Path: "Avatar The Last Airbender 2x03 Return to Omashu.mkv", Length: 1142880878}}}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)
	body := map[string]any{
		"type":        "episode",
		"title":       "Avatar The Last Airbender",
		"season":      2,
		"episode":     3,
		"series_imdb": "tt0417299",
		"magnet":      magnet,
	}
	stubPath := filepath.Join(real, "tv", "tt0417299", "Season.02", "Avatar_The_Last_Airbender_S02E03_278799e9.mkv")
	if err := library.WriteStub(stubPath, "http://gostorm.local/stream?link="+hash+"&index=23&play", 0, magnet, "tt0417299"); err != nil {
		t.Fatal(err)
	}

	rr := postAdd(t, h, body)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stub, err := library.ReadStub(stubPath)
	if err != nil {
		t.Fatal(err)
	}
	if stub.Size != 1142880878 {
		t.Fatalf("zero-size stub was not repaired: %+v", stub)
	}
}

func TestLibraryAddEpisode_ReplacesExistingWrongPackIndex(t *testing.T) {
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	real := t.TempDir()
	fuse := t.TempDir()
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=Avatar%3A%20The%20Last%20Airbender%20-%20S01%20to%20S03"
	gs := &fakeGoStorm{files: []library.FileStat{
		{ID: 23, Path: "[Avatar Realms] Avatar The Last Airbender 2x03 Return to Omashu.mkv", Length: 1142880878},
		{ID: 31, Path: "[Avatar Realms] Avatar The Last Airbender 2x11 The Desert.mkv", Length: 1175677822},
	}}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)
	body := map[string]any{
		"type":        "episode",
		"title":       "Avatar The Last Airbender",
		"season":      2,
		"episode":     3,
		"series_imdb": "tt0417299",
		"magnet":      magnet,
	}
	stubPath := filepath.Join(real, "tv", "tt0417299", "Season.02", "Avatar_The_Last_Airbender_S02E03_278799e9.mkv")
	if err := library.WriteStub(stubPath, "http://gostorm.local/stream?link="+hash+"&index=31&play", 1175677822, magnet, "tt0417299"); err != nil {
		t.Fatal(err)
	}

	rr := postAdd(t, h, body)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stub, err := library.ReadStub(stubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.URL, "index=23") || stub.Size != 1142880878 {
		t.Fatalf("stub not repaired: %+v", stub)
	}
	if gs.addCalls != 1 {
		t.Fatalf("expected replacement add call, got %d", gs.addCalls)
	}
}

func TestLibraryAddEpisode_ExistingValidStubReturns409WithoutAddingTorrent(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	real := t.TempDir()
	fuse := t.TempDir()
	gs := &fakeGoStorm{files: []library.FileStat{{ID: 7, Path: "Show.S01E02.mkv", Length: 5 * gb}}}
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
	if err := library.WriteStub(stubPath, "http://gostorm.local/stream?link=abcdef0123456789abcdef0123456789abcdef01&index=7&play", 5*gb, body["magnet"].(string), "tt123"); err != nil {
		t.Fatal(err)
	}

	rr := postAdd(t, h, body)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gs.addCalls != 0 {
		t.Fatalf("expected no AddTorrent call, got %d", gs.addCalls)
	}
	if gs.getCalls != 1 {
		t.Fatalf("expected metadata validation call, got %d", gs.getCalls)
	}
}

func TestLibraryRemoveRejectsPathOutsidePhysicalSource(t *testing.T) {
	real := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(outside, []byte("not a library stub"), 0644); err != nil {
		t.Fatal(err)
	}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: t.TempDir(), TimeoutSec: 1}, &fakeGoStorm{})

	rr := postRemove(t, h, map[string]any{"stub_path": outside})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file should remain: %v", err)
	}
}

func TestLibraryRemoveEpisodePackStubKeepsTorrentWhenLegacySiblingReferencesHash(t *testing.T) {
	real := t.TempDir()
	fuse := t.TempDir()
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	magnet := "magnet:?xt=urn:btih:" + hash
	gs := &fakeGoStorm{}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)
	removePath := filepath.Join(real, "tv", "tt0417299", "Season.02", "Avatar_The_Last_Airbender_S02E03_278799e9.mkv")
	siblingPath := filepath.Join(real, "tv", "tt0417299", "Season.02", "Avatar_The_Last_Airbender_S02E04_278799e9.mkv")
	if err := library.WriteStub(removePath, "http://gostorm.local/stream?link="+hash+"&index=23&play", 1142880878, magnet, "tt0417299"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(siblingPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingPath, []byte("http://gostorm.local/stream?link="+hash+"&index=24&play\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rr := postRemove(t, h, map[string]any{"stub_path": removePath})

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gs.removedHash != "" {
		t.Fatalf("shared pack torrent should remain while legacy sibling stub exists; removed %q", gs.removedHash)
	}
}

func TestLibraryRemoveEpisodePackStubKeepsTorrentWhenSiblingReferencesHash(t *testing.T) {
	real := t.TempDir()
	fuse := t.TempDir()
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	magnet := "magnet:?xt=urn:btih:" + hash
	gs := &fakeGoStorm{}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)
	removePath := filepath.Join(real, "tv", "tt0417299", "Season.02", "Avatar_The_Last_Airbender_S02E03_278799e9.mkv")
	siblingPath := filepath.Join(real, "tv", "tt0417299", "Season.02", "Avatar_The_Last_Airbender_S02E04_278799e9.mkv")
	if err := library.WriteStub(removePath, "http://gostorm.local/stream?link="+hash+"&index=23&play", 1142880878, magnet, "tt0417299"); err != nil {
		t.Fatal(err)
	}
	if err := library.WriteStub(siblingPath, "http://gostorm.local/stream?link="+hash+"&index=24&play", 1143272279, magnet, "tt0417299"); err != nil {
		t.Fatal(err)
	}

	rr := postRemove(t, h, map[string]any{"stub_path": removePath})

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gs.removedHash != "" {
		t.Fatalf("shared pack torrent should remain while sibling stub exists; removed %q", gs.removedHash)
	}
	if _, err := os.Stat(removePath); !os.IsNotExist(err) {
		t.Fatalf("removed stub still exists or stat failed differently: %v", err)
	}
	if _, err := os.Stat(siblingPath); err != nil {
		t.Fatalf("sibling stub missing: %v", err)
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
