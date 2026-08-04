package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gostream/internal/library"
)

type fakeGoStorm struct {
	files       []library.FileStat
	audioTracks []library.AudioTrack
	removedHash string
	addedMagnet string
	addedTitle  string
	addCalls    int
	getCalls    int
	addErr      error
	filesErr    error
	probeErr    error
}

func (f *fakeGoStorm) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	f.addCalls++
	f.addedMagnet = magnet
	f.addedTitle = title
	if f.addErr != nil {
		return "", f.addErr
	}
	return library.HashFromMagnet(magnet), nil
}

func (f *fakeGoStorm) GetTorrentFiles(ctx context.Context, hash string, maxWaitSec int) ([]library.FileStat, error) {
	f.getCalls++
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.files, nil
}

func (f *fakeGoStorm) RemoveTorrent(ctx context.Context, hash string) error {
	f.removedHash = hash
	return nil
}

func (f *fakeGoStorm) ProbeAudio(ctx context.Context, hash string, fileID int, maxWaitSec int) ([]library.AudioTrack, error) {
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	return f.audioTracks, nil
}

func (f *fakeGoStorm) BaseURL() string { return "http://gostorm.local" }

func postRemove(t *testing.T, h *LibraryHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/library/remove", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:1234"
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
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Add(rr, req)
	return rr
}

func postValidate(t *testing.T, h *LibraryHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/library/validate", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Validate(rr, req)
	return rr
}

func baseEpisodeRequest(magnet string) map[string]any {
	return map[string]any{
		"type":                     "episode",
		"title":                    "Avatar The Last Airbender",
		"season":                   2,
		"episode":                  1,
		"series_imdb":              "tt0417299",
		"magnet":                   magnet,
		"preferred_audio_language": "eng",
		"required_audio_languages": []string{"eng", "en", "english"},
		"validation_session_id":    "session-1",
	}
}

func TestLibraryValidate_PolishDefaultEnglishPresentSelectsEnglish(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	real := t.TempDir()
	fuse := t.TempDir()
	magnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"
	gs := &fakeGoStorm{
		files: []library.FileStat{{ID: 12, Path: "Avatar.S02E01.mkv", Length: 5 * gb}},
		audioTracks: []library.AudioTrack{
			{StreamIndex: 1, Language: "pol", Title: "Polish", Codec: "aac", Channels: 2, Default: true},
			{StreamIndex: 2, Language: "eng", Title: "English", Codec: "aac", Channels: 2},
		},
	}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: real, FuseMountPath: fuse, TimeoutSec: 1}, gs)

	rr := postValidate(t, h, baseEpisodeRequest(magnet))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp validateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "valid" || resp.SelectedAudioIndex == nil || *resp.SelectedAudioIndex != 2 {
		t.Fatalf("unexpected validate response: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(real, "tv")); !os.IsNotExist(err) {
		t.Fatalf("validate must not write stub tree; stat err=%v", err)
	}
}

func TestLibraryValidate_NoEnglishAudioInvalid(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	gs := &fakeGoStorm{
		files:       []library.FileStat{{ID: 12, Path: "Avatar.S02E01.mkv", Length: 5 * gb}},
		audioTracks: []library.AudioTrack{{StreamIndex: 1, Language: "pol", Title: "Polish", Codec: "aac", Channels: 2}},
	}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1}, gs)

	rr := postValidate(t, h, baseEpisodeRequest("magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"))

	var resp validateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "invalid" || resp.Reason == nil || *resp.Reason != "no_english_audio" {
		t.Fatalf("unexpected validate response: %+v", resp)
	}
}

func TestLibraryAdd_RevalidatesSelectedFileAudioHints(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	magnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"
	gs := &fakeGoStorm{
		files:       []library.FileStat{{ID: 12, Path: "Avatar.S02E01.mkv", Length: 5 * gb}},
		audioTracks: []library.AudioTrack{{StreamIndex: 2, Language: "eng", Title: "English", Codec: "aac", Channels: 2}},
	}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1}, gs)
	body := baseEpisodeRequest(magnet)
	body["selected_file_id"] = 12
	body["selected_file_path"] = "Avatar.S02E01.mkv"

	rr := postAdd(t, h, body)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLibraryValidate_AudioProbeFailureIsTransient(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	magnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"
	gs := &fakeGoStorm{files: []library.FileStat{{ID: 12, Path: "Show.S02E01.mkv", Length: 5 * gb}}, probeErr: errors.New("probe failed")}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1}, gs)
	body := baseEpisodeRequest(magnet)
	body["title"] = "Show"
	body["series_imdb"] = "tt123"

	rr := postValidate(t, h, body)
	var resp validateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "transient" || resp.Reason == nil || *resp.Reason != "audio_probe_failed" {
		t.Fatalf("response=%+v body=%s", resp, rr.Body.String())
	}
}

func TestLibraryValidate_MetadataCancelIsTransientCancelled(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"
	gs := &fakeGoStorm{filesErr: context.Canceled}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1}, gs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, err := json.Marshal(baseEpisodeRequest(magnet))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/library/validate", bytes.NewReader(raw)).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Validate(rr, req)
	var resp validateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || resp.Status != "transient" || resp.Reason == nil || *resp.Reason != "validation_cancelled" {
		t.Fatalf("status=%d response=%+v body=%s", rr.Code, resp, rr.Body.String())
	}
}

func TestLibraryValidate_ExpiredLeaseCleanupRemovesOnlyUnreferenced(t *testing.T) {
	hash := "abcdef0123456789abcdef0123456789abcdef01"
	gs := &fakeGoStorm{}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir()}, gs)
	h.storeLease("expired", hash, library.FileStat{ID: 1}, nil, time.Now().Add(-time.Minute))
	h.cleanupExpiredValidationLeases()
	if gs.removedHash != hash {
		t.Fatalf("expected expired unreferenced lease cleanup, got %q", gs.removedHash)
	}

	gs = &fakeGoStorm{}
	h = NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir()}, gs)
	h.storeLease("expired", hash, library.FileStat{ID: 1}, nil, time.Now().Add(-time.Minute))
	h.storeLease("active", hash, library.FileStat{ID: 1}, nil, time.Now().Add(time.Minute))
	h.cleanupExpiredValidationLeases()
	if gs.removedHash != "" {
		t.Fatalf("must preserve hash with active validation lease, removed %q", gs.removedHash)
	}
}

func TestLibraryAuth_AllProtectedEndpointsRejectNonLoopbackWithoutToken(t *testing.T) {
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir()}, &fakeGoStorm{})
	endpoints := []struct {
		name string
		body string
		call func(http.ResponseWriter, *http.Request)
	}{
		{name: "add", body: `{"type":"movie","title":"Movie","year":2024,"tmdb":1,"magnet":"magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"}`, call: h.Add},
		{name: "remove", body: `{"stub_path":"/tmp/x.mkv"}`, call: h.Remove},
		{name: "validate", body: `{"type":"movie","title":"Movie","year":2024,"tmdb":1,"magnet":"magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01","validation_session_id":"s"}`, call: h.Validate},
		{name: "release", body: `{"validation_session_id":"s","hash":"abcdef0123456789abcdef0123456789abcdef01"}`, call: h.ReleaseValidation},
	}
	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/library/"+ep.name, bytes.NewReader([]byte(ep.body)))
			req.RemoteAddr = "203.0.113.10:1234"
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			ep.call(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestLibraryAuthTokenAndLoopbackRules(t *testing.T) {
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1, AuthToken: "secret"}, &fakeGoStorm{})
	raw := []byte(`{"stub_path":"/tmp/nope"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/remove", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Remove(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/library/remove", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gostream-Token", "secret")
	rr = httptest.NewRecorder()
	h.Remove(rr, req)
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("correct token rejected: %d %s", rr.Code, rr.Body.String())
	}

	loopbackOnly := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1}, &fakeGoStorm{})
	req = httptest.NewRequest(http.MethodPost, "/api/library/validate/release", bytes.NewReader([]byte(`{"validation_session_id":"s","hash":"h"}`)))
	req.RemoteAddr = "203.0.113.9:4321"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	loopbackOnly.ReleaseValidation(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-loopback anonymous status=%d", rr.Code)
	}
}

func TestLibraryReleaseValidationPreservesSharedHashLease(t *testing.T) {
	gs := &fakeGoStorm{}
	h := NewLibraryHandler(LibraryConfig{PhysicalSourcePath: t.TempDir(), FuseMountPath: t.TempDir(), TimeoutSec: 1}, gs)
	expires := time.Now().Add(10 * time.Minute)
	h.storeLease("session-1", "abcdef0123456789abcdef0123456789abcdef01", library.FileStat{ID: 1}, nil, expires)
	h.storeLease("session-2", "abcdef0123456789abcdef0123456789abcdef01", library.FileStat{ID: 1}, nil, expires)

	raw := []byte(`{"validation_session_id":"session-1","hash":"abcdef0123456789abcdef0123456789abcdef01"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/validate/release", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ReleaseValidation(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gs.removedHash != "" {
		t.Fatalf("shared hash was removed while second validation lease exists: %q", gs.removedHash)
	}
	if !h.hashHasValidationLease("abcdef0123456789abcdef0123456789abcdef01") {
		t.Fatalf("second validation lease missing")
	}
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
