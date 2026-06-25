package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gostream/internal/library"
)

// GoStormClient is the subset of the engines.GoStormClient API that
// the library/add + library/remove handlers need. Declared as an
// interface here so the dashboard package does not import
// internal/syncer/engines (and so tests can substitute a fake).
type GoStormClient interface {
	AddTorrent(ctx context.Context, magnet, title string) (string, error)
	// GetTorrentFiles returns the torrent's file list once metadata is
	// available, bounded by maxWaitSec. Returns a non-nil error on
	// timeout or context cancellation.
	GetTorrentFiles(ctx context.Context, hash string, maxWaitSec int) ([]library.FileStat, error)
	RemoveTorrent(ctx context.Context, hash string) error
	ProbeAudio(ctx context.Context, hash string, fileID int, maxWaitSec int) ([]library.AudioTrack, error)
	BaseURL() string
}

// TorrentInfo (legacy) — kept for backward compatibility; no longer
// used by the interface above.
type TorrentInfo struct {
	Hash      string             `json:"hash"`
	Title     string             `json:"title"`
	Length    int64              `json:"length"`
	FileStats []library.FileStat `json:"file_stats"`
}

// LibraryConfig wires the LibraryHandler to its on-disk layout.
type LibraryConfig struct {
	PhysicalSourcePath string // real movies/tv root (config.PhysicalSourcePath)
	FuseMountPath      string // FUSE virtual mount root (config.FuseMountPath)
	TimeoutSec         int    // server-side metadata wait, default 45
	AuthToken          string // optional X-Gostream-Token shared secret
	ValidationLeaseMin int    // validation lease minutes, default 10
}

// LibraryHandler serves POST /api/library/add and /api/library/remove.
type LibraryHandler struct {
	cfg     LibraryConfig
	gostorm GoStormClient

	leasesMu sync.Mutex
	leases   map[string]*validationLease
}

type validationLease struct {
	SessionID string
	Hash      string
	File      library.FileStat
	Tracks    []library.AudioTrack
	ExpiresAt time.Time
}

// NewLibraryHandler constructs a handler. Both args are required and
// must be non-nil at registration time.
func NewLibraryHandler(cfg LibraryConfig, gs GoStormClient) *LibraryHandler {
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 45
	}
	if cfg.ValidationLeaseMin <= 0 {
		cfg.ValidationLeaseMin = 10
	}
	if cfg.ValidationLeaseMin < 1 {
		cfg.ValidationLeaseMin = 1
	}
	if cfg.ValidationLeaseMin > 60 {
		cfg.ValidationLeaseMin = 60
	}
	return &LibraryHandler{cfg: cfg, gostorm: gs, leases: map[string]*validationLease{}}
}

type addRequest struct {
	Type       string `json:"type"`
	IMDB       string `json:"imdb"`
	TMDB       int    `json:"tmdb"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	Season     int    `json:"season"`
	Episode    int    `json:"episode"`
	SeriesIMDB string `json:"series_imdb"`
	Magnet     string `json:"magnet"`
	MinQuality string `json:"min_quality"` // TODO(follow-up): honor quality floor

	RequiredAudioLanguages []string `json:"required_audio_languages"`
	PreferredAudioLanguage string   `json:"preferred_audio_language"`
	ValidationSessionID    string   `json:"validation_session_id"`
	SelectedFileID         *int     `json:"selected_file_id"`
	SelectedFilePath       string   `json:"selected_file_path"`
}

type addResponse struct {
	StubPath string `json:"stub_path"`
	FusePath string `json:"fuse_path"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
}

type selectedFileResponse struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type validationTimings struct {
	AddTorrent   int64 `json:"add_torrent"`
	MetadataWait int64 `json:"metadata_wait"`
	FileSelect   int64 `json:"file_select"`
	AudioProbe   int64 `json:"audio_probe"`
	Total        int64 `json:"total"`
}

type validateResponse struct {
	Status                   string                `json:"status"`
	Reason                   *string               `json:"reason"`
	Hash                     string                `json:"hash"`
	SelectedFile             *selectedFileResponse `json:"selected_file,omitempty"`
	AudioTracks              []library.AudioTrack  `json:"audio_tracks,omitempty"`
	SelectedAudioIndex       *int                  `json:"selected_audio_index,omitempty"`
	SelectedAudioLanguage    string                `json:"selected_audio_language,omitempty"`
	ValidationSessionID      string                `json:"validation_session_id"`
	ValidationLeaseExpiresAt *time.Time            `json:"validation_lease_expires_at,omitempty"`
	TimingsMS                validationTimings     `json:"timings_ms"`
}

type releaseRequest struct {
	ValidationSessionID string `json:"validation_session_id"`
	Hash                string `json:"hash"`
}

type removeRequest struct {
	StubPath string `json:"stub_path"`
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *LibraryHandler) authorize(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.AuthToken != "" {
		if r.Header.Get("X-Gostream-Token") != h.cfg.AuthToken {
			writeJSONError(w, http.StatusUnauthorized, "missing_or_invalid_token")
			return false
		}
		return true
	}
	if isLoopbackRequest(r) {
		return true
	}
	writeJSONError(w, http.StatusForbidden, "loopback_required_when_token_unset")
	return false
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// reMagnetTitle pulls the dn= value out of a magnet URI, used to infer
// 4K vs 1080p when the caller did not provide year/quality hints.
var reMagnet4K = regexp.MustCompile(`(?i)2160p|4[kK]|uhd`)

// Add handles POST /api/library/add.
//
// Flow (per PLAN M1):
//  1. Validate method/content-type/body.
//  2. AddTorrent → infohash.
//  3. GetTorrentInfo bounded by cfg.TimeoutSec → 504 on timeout.
//  4. Filter video files by size band; movies pick largest, episodes
//     pick largest file whose basename matches requested season/episode.
//  5. Build canonical filename + stub path under PhysicalSourcePath.
//  6. If stub already exists, return 409 with existing data (idempotent).
//  7. Write stub via library.WriteStub.
//  8. Return 200 with {stub_path, fuse_path, hash, size}.
func (h *LibraryHandler) Add(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	h.cleanupExpiredValidationLeases()
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}

	var req addRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json: "+err.Error())
		return
	}

	if err := validateAdd(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Determine output layout BEFORE adding the torrent, so an
	// idempotent re-request for an already-staged item never touches
	// the torrent engine.
	is4K := reMagnet4K.MatchString(req.Magnet)
	for _, existingPath := range h.existingStubPaths(&req, is4K) {
		if existing, err := library.ReadStub(existingPath); err == nil {
			if req.Type == "episode" && !h.existingEpisodeStubMatches(r.Context(), existing, &req) {
				// Existing pre-patch episode stubs may point at the largest file in
				// a season/series pack instead of the requested episode. Leave the
				// old stub in place until the replacement write succeeds below.
				break
			}
			fusePath, err := library.RealToFuse(existingPath, h.cfg.PhysicalSourcePath, h.cfg.FuseMountPath)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "fuse_path_resolve: "+err.Error())
				return
			}
			existingHash := library.HashFromStreamURL(existing.URL)
			if existingHash == "" {
				existingHash = library.HashFromMagnet(existing.Magnet)
			}
			writeJSON(w, http.StatusConflict, addResponse{
				StubPath: existingPath,
				FusePath: fusePath,
				Hash:     existingHash,
				Size:     existing.Size,
			})
			return
		}
	}

	displayTitle := req.Title

	ctx := r.Context()
	hash, err := h.gostorm.AddTorrent(ctx, req.Magnet, displayTitle)
	if err != nil || hash == "" {
		if err == nil {
			err = errors.New("empty hash")
		}
		writeJSONError(w, http.StatusBadGateway, "add_torrent_failed: "+err.Error())
		return
	}

	timeout := h.cfg.TimeoutSec
	files, err := h.gostorm.GetTorrentFiles(ctx, hash, timeout)
	if err != nil {
		h.cleanupTorrentIfUnreferenced(hash)
		writeJSONError(w, http.StatusGatewayTimeout, fmt.Sprintf("metadata_timeout: waited %ds (%s)", timeout, err.Error()))
		return
	}

	timings := validationTimings{}
	result, reason, transient := h.selectAndProbe(ctx, &req, hash, files, &timings)
	if reason != "" {
		if !transient {
			h.cleanupTorrentIfUnreferenced(hash)
		}
		status := http.StatusUnprocessableEntity
		if transient {
			status = http.StatusGatewayTimeout
		}
		writeJSONError(w, status, reason)
		return
	}
	bestFile := result.file
	h.consumeLease(req.ValidationSessionID, hash)

	// Re-derive is4K from the picked file size if magnet title didn't
	// announce it — anything ≥ Movie4KMinBytes is treated as 4K.
	if req.Type == "movie" && !is4K && bestFile.Length >= library.Movie4KMinBytes {
		is4K = true
	}

	var stubPath string
	switch req.Type {
	case "movie":
		releaseDate := fmt.Sprintf("%d-01-01", req.Year)
		filename := library.BuildMovieFilename(req.Title, releaseDate, library.MovieStreamMeta{
			Title: req.Title, Hash: hash, Is4K: is4K,
		})
		stubPath = filepath.Join(h.cfg.PhysicalSourcePath, "movies", filename)
	case "episode":
		filename := library.BuildEpisodeFilename(req.Title, req.Season, req.Episode, hashSuffix(hash))
		stubPath = filepath.Join(
			h.cfg.PhysicalSourcePath, "tv", req.SeriesIMDB,
			fmt.Sprintf("Season.%02d", req.Season), filename,
		)
	}

	fusePath, err := library.RealToFuse(stubPath, h.cfg.PhysicalSourcePath, h.cfg.FuseMountPath)
	if err != nil {
		h.cleanupTorrentIfUnreferenced(hash)
		writeJSONError(w, http.StatusInternalServerError, "fuse_path_resolve: "+err.Error())
		return
	}

	streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play",
		h.gostorm.BaseURL(), hash, bestFile.ID)

	if err := library.WriteStub(stubPath, streamURL, bestFile.Length, req.Magnet, imdbForStub(&req)); err != nil {
		h.cleanupTorrentIfUnreferenced(hash)
		writeJSONError(w, http.StatusInternalServerError, "write_stub: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, addResponse{
		StubPath: stubPath,
		FusePath: fusePath,
		Hash:     hash,
		Size:     bestFile.Length,
	})
}

// Validate handles POST /api/library/validate. It proves metadata, selected
// file, and requested main English audio without writing a stub.
func (h *LibraryHandler) Validate(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	h.cleanupExpiredValidationLeases()
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}

	started := time.Now()
	var timings validationTimings
	var req addRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json: "+err.Error())
		return
	}
	if err := validateAdd(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.RequiredAudioLanguages) == 0 && req.PreferredAudioLanguage == "" {
		req.RequiredAudioLanguages = []string{"eng"}
		req.PreferredAudioLanguage = "eng"
	}

	ctx := r.Context()
	addStarted := time.Now()
	hash, err := h.gostorm.AddTorrent(ctx, req.Magnet, req.Title)
	timings.AddTorrent = time.Since(addStarted).Milliseconds()
	if err != nil || hash == "" {
		if err == nil {
			err = errors.New("empty hash")
		}
		reason := "torrent_engine_busy"
		if errors.Is(ctx.Err(), context.Canceled) {
			reason = "validation_cancelled"
		}
		writeJSON(w, http.StatusOK, validationFailure("transient", reason, hash, req.ValidationSessionID, timings, started))
		return
	}

	metaStarted := time.Now()
	files, err := h.gostorm.GetTorrentFiles(ctx, hash, h.cfg.TimeoutSec)
	timings.MetadataWait = time.Since(metaStarted).Milliseconds()
	if err != nil {
		h.cleanupTorrentIfUnreferenced(hash)
		reason := "metadata_timeout"
		if errors.Is(ctx.Err(), context.Canceled) {
			reason = "validation_cancelled"
		}
		writeJSON(w, http.StatusOK, validationFailure("transient", reason, hash, req.ValidationSessionID, timings, started))
		return
	}

	result, reason, transient := h.selectAndProbe(ctx, &req, hash, files, &timings)
	if reason != "" {
		if !transient {
			h.cleanupTorrentIfUnreferenced(hash)
		}
		status := "invalid"
		if transient {
			status = "transient"
		}
		writeJSON(w, http.StatusOK, validationFailure(status, reason, hash, req.ValidationSessionID, timings, started))
		return
	}

	expires := time.Now().UTC().Add(time.Duration(h.cfg.ValidationLeaseMin) * time.Minute)
	sessionID := strings.TrimSpace(req.ValidationSessionID)
	if sessionID != "" {
		h.storeLease(sessionID, hash, result.file, result.tracks, expires)
	}
	timings.Total = time.Since(started).Milliseconds()
	idx := result.selected.StreamIndex
	selectedLanguage := library.NormalizeAudioLanguage(result.selected.Language)
	if selectedLanguage == "" {
		selectedLanguage = library.NormalizeAudioLanguage(req.PreferredAudioLanguage)
	}
	resp := validateResponse{
		Status:                "valid",
		Hash:                  hash,
		SelectedFile:          &selectedFileResponse{ID: result.file.ID, Path: result.file.Path, Size: result.file.Length},
		AudioTracks:           result.tracks,
		SelectedAudioIndex:    &idx,
		SelectedAudioLanguage: selectedLanguage,
		ValidationSessionID:   sessionID,
		TimingsMS:             timings,
	}
	if sessionID != "" {
		resp.ValidationLeaseExpiresAt = &expires
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *LibraryHandler) ReleaseValidation(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	h.cleanupExpiredValidationLeases()
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}
	var req releaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json: "+err.Error())
		return
	}
	if req.ValidationSessionID == "" || req.Hash == "" {
		writeJSONError(w, http.StatusBadRequest, "validation_session_id and hash required")
		return
	}
	if h.releaseLease(req.ValidationSessionID, req.Hash) && !h.hashHasValidationLease(req.Hash) {
		h.cleanupTorrentIfUnreferenced(req.Hash)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

type selectionProbeResult struct {
	file     library.FileStat
	tracks   []library.AudioTrack
	selected library.AudioTrack
}

func validationFailure(status, reason, hash, sessionID string, timings validationTimings, started time.Time) validateResponse {
	timings.Total = time.Since(started).Milliseconds()
	return validateResponse{Status: status, Reason: &reason, Hash: hash, ValidationSessionID: sessionID, TimingsMS: timings}
}

func (h *LibraryHandler) selectAndProbe(ctx context.Context, req *addRequest, hash string, files []library.FileStat, timings *validationTimings) (selectionProbeResult, string, bool) {
	selectStarted := time.Now()
	bestFile, reason := selectRequestedFile(req, files, reMagnet4K.MatchString(req.Magnet))
	timings.FileSelect = time.Since(selectStarted).Milliseconds()
	if reason != "" {
		return selectionProbeResult{}, reason, false
	}

	if len(req.RequiredAudioLanguages) == 0 && req.PreferredAudioLanguage == "" {
		return selectionProbeResult{file: bestFile}, "", false
	}

	probeStarted := time.Now()
	tracks, err := h.gostorm.ProbeAudio(ctx, hash, bestFile.ID, h.cfg.TimeoutSec)
	timings.AudioProbe = time.Since(probeStarted).Milliseconds()
	if err != nil {
		return selectionProbeResult{}, "audio_probe_failed", true
	}
	selected, foundLanguage, foundMain := library.SelectPreferredMainAudio(tracks, req.RequiredAudioLanguages, req.PreferredAudioLanguage)
	if !foundLanguage {
		return selectionProbeResult{}, "no_english_audio", false
	}
	if !foundMain {
		return selectionProbeResult{}, "no_main_english_audio", false
	}
	return selectionProbeResult{file: bestFile, tracks: tracks, selected: selected}, "", false
}

func selectRequestedFile(req *addRequest, files []library.FileStat, is4K bool) (library.FileStat, string) {
	if req.SelectedFileID != nil || req.SelectedFilePath != "" {
		for _, f := range files {
			if req.SelectedFileID != nil && f.ID != *req.SelectedFileID {
				continue
			}
			if req.SelectedFilePath != "" && f.Path != req.SelectedFilePath {
				continue
			}
			if !library.IsVideoFile(f.Path) {
				return library.FileStat{}, "no_valid_files"
			}
			if req.Type == "movie" {
				minSize, maxSize := library.Movie1080pMinBytes, library.Movie1080pMaxBytes
				if is4K {
					minSize, maxSize = library.Movie4KMinBytes, library.Movie4KMaxBytes
				}
				if f.Length < minSize || f.Length > maxSize {
					return library.FileStat{}, "no_valid_files"
				}
			} else {
				if f.Length < library.EpisodeMinBytes || f.Length > library.EpisodeMaxBytes {
					return library.FileStat{}, "no_valid_files"
				}
				season, episode, ok := library.ParseEpisodeFromFilename(f.Path)
				if !ok || season != req.Season || episode != req.Episode {
					return library.FileStat{}, "target_episode_not_found"
				}
			}
			return f, ""
		}
		return library.FileStat{}, "selected_file_missing"
	}
	if req.Type == "movie" {
		videoFiles := library.FilterVideoFiles(files, is4K)
		if len(videoFiles) == 0 {
			return library.FileStat{}, "no_valid_files"
		}
		sort.Slice(videoFiles, func(i, j int) bool { return videoFiles[i].Length > videoFiles[j].Length })
		return videoFiles[0], ""
	}
	bestFile, ok := library.SelectEpisodeFile(files, req.Season, req.Episode)
	if !ok {
		return library.FileStat{}, "target_episode_not_found"
	}
	return bestFile, ""
}

func (h *LibraryHandler) storeLease(sessionID, hash string, file library.FileStat, tracks []library.AudioTrack, expires time.Time) {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	h.expireLeasesLocked(time.Now())
	h.leases[sessionID] = &validationLease{SessionID: sessionID, Hash: hash, File: file, Tracks: tracks, ExpiresAt: expires}
}

func (h *LibraryHandler) consumeLease(sessionID, hash string) bool {
	if sessionID == "" || hash == "" {
		return false
	}
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	h.expireLeasesLocked(time.Now())
	lease, ok := h.leases[sessionID]
	if !ok || !strings.EqualFold(lease.Hash, hash) {
		return false
	}
	delete(h.leases, sessionID)
	return true
}

func (h *LibraryHandler) releaseLease(sessionID, hash string) bool {
	return h.consumeLease(sessionID, hash)
}

func (h *LibraryHandler) hashHasValidationLease(hash string) bool {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	h.expireLeasesLocked(time.Now())
	for _, lease := range h.leases {
		if strings.EqualFold(lease.Hash, hash) {
			return true
		}
	}
	return false
}

func (h *LibraryHandler) expireLeasesLocked(now time.Time) {
	for key, lease := range h.leases {
		if !lease.ExpiresAt.After(now) {
			delete(h.leases, key)
		}
	}
}

func (h *LibraryHandler) takeExpiredLeases(now time.Time) []*validationLease {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	var expired []*validationLease
	for key, lease := range h.leases {
		if !lease.ExpiresAt.After(now) {
			expired = append(expired, lease)
			delete(h.leases, key)
		}
	}
	return expired
}

// Remove handles POST /api/library/remove. Idempotent: missing torrent
// or missing stub is not an error.
func (h *LibraryHandler) Remove(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	h.cleanupExpiredValidationLeases()
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}

	var req removeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json: "+err.Error())
		return
	}
	if req.StubPath == "" {
		writeJSONError(w, http.StatusBadRequest, "stub_path required")
		return
	}

	if !h.pathUnderPhysicalRoot(req.StubPath) {
		writeJSONError(w, http.StatusBadRequest, "stub_path outside physical source path")
		return
	}

	// Read stub (best effort) to recover the hash.
	var hash string
	if stub, err := library.ReadStub(req.StubPath); err == nil {
		hash = library.HashFromStreamURL(stub.URL)
		if hash == "" {
			hash = library.HashFromMagnet(stub.Magnet)
		}
	}

	if hash != "" && !h.stubTreeReferencesHashExcept(hash, req.StubPath) {
		if err := h.gostorm.RemoveTorrent(r.Context(), hash); err != nil {
			writeJSONError(w, http.StatusBadGateway, "remove_torrent: "+err.Error())
			return
		}
	}

	if err := os.Remove(req.StubPath); err != nil && !os.IsNotExist(err) {
		writeJSONError(w, http.StatusInternalServerError, "remove_stub: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *LibraryHandler) pathUnderPhysicalRoot(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(h.cfg.PhysicalSourcePath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (h *LibraryHandler) cleanupTorrentIfUnreferenced(hash string) {
	if hash == "" || h.stubTreeReferencesHash(hash) || h.hashHasValidationLease(hash) {
		return
	}
	_ = h.gostorm.RemoveTorrent(context.Background(), hash)
}

func (h *LibraryHandler) cleanupExpiredValidationLeases() {
	for _, lease := range h.takeExpiredLeases(time.Now()) {
		h.cleanupTorrentIfUnreferenced(lease.Hash)
	}
}

func (h *LibraryHandler) existingEpisodeStubMatches(ctx context.Context, stub *library.Stub, req *addRequest) bool {
	hash := library.HashFromStreamURL(stub.URL)
	if hash == "" {
		hash = library.HashFromMagnet(stub.Magnet)
	}
	if hash == "" {
		return false
	}

	index, ok := library.StreamIndexFromURL(stub.URL)
	if !ok {
		return false
	}

	files, err := h.gostorm.GetTorrentFiles(ctx, hash, h.cfg.TimeoutSec)
	if err != nil {
		return false
	}

	for _, f := range files {
		if f.ID != index {
			continue
		}
		season, episode, ok := library.ParseEpisodeFromFilename(f.Path)
		if !ok || season != req.Season || episode != req.Episode {
			return false
		}
		return stub.Size == f.Length
	}

	return false
}

func (h *LibraryHandler) stubTreeReferencesHash(hash string) bool {
	return h.stubTreeReferencesHashExcept(hash, "")
}

func (h *LibraryHandler) stubTreeReferencesHashExcept(hash, excludedPath string) bool {
	if strings.TrimSpace(h.cfg.PhysicalSourcePath) == "" {
		return false
	}
	absExcluded := ""
	if excludedPath != "" {
		absExcluded, _ = filepath.Abs(excludedPath)
	}
	found := false
	_ = filepath.WalkDir(h.cfg.PhysicalSourcePath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found || !library.IsVideoFile(path) {
			return nil
		}
		if absExcluded != "" {
			absPath, err := filepath.Abs(path)
			if err == nil && absPath == absExcluded {
				return nil
			}
		}
		stubHash := hashFromStubFile(path)
		if strings.EqualFold(stubHash, hash) {
			found = true
		}
		return nil
	})
	return found
}

func hashFromStubFile(path string) string {
	if stub, err := library.ReadStub(path); err == nil {
		stubHash := library.HashFromStreamURL(stub.URL)
		if stubHash == "" {
			stubHash = library.HashFromMagnet(stub.Magnet)
		}
		return stubHash
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	firstLine := strings.SplitN(string(raw), "\n", 2)[0]
	return library.HashFromStreamURL(firstLine)
}

func (h *LibraryHandler) existingStubPaths(req *addRequest, is4K bool) []string {
	hash := library.HashFromMagnet(req.Magnet)
	switch req.Type {
	case "movie":
		releaseDate := fmt.Sprintf("%d-01-01", req.Year)
		paths := []string{
			filepath.Join(h.cfg.PhysicalSourcePath, "movies", library.BuildMovieFilename(req.Title, releaseDate, library.MovieStreamMeta{
				Title: req.Title, Hash: hash, Is4K: is4K,
			})),
		}
		if !is4K {
			paths = append(paths, filepath.Join(h.cfg.PhysicalSourcePath, "movies", library.BuildMovieFilename(req.Title, releaseDate, library.MovieStreamMeta{
				Title: req.Title, Hash: hash, Is4K: true,
			})))
		}
		return paths
	case "episode":
		filename := library.BuildEpisodeFilename(req.Title, req.Season, req.Episode, hashSuffix(hash))
		return []string{filepath.Join(
			h.cfg.PhysicalSourcePath, "tv", req.SeriesIMDB,
			fmt.Sprintf("Season.%02d", req.Season), filename,
		)}
	default:
		return nil
	}
}

func validateAdd(req *addRequest) error {
	switch req.Type {
	case "movie":
		if req.IMDB == "" && req.TMDB == 0 {
			return errors.New("imdb or tmdb required for movie")
		}
		if req.Title == "" {
			return errors.New("title required")
		}
		if req.Year == 0 {
			return errors.New("year required for movie")
		}
	case "episode":
		if req.Title == "" {
			return errors.New("title required")
		}
		if req.Season <= 0 {
			return errors.New("season required for episode")
		}
		if req.Episode <= 0 {
			return errors.New("episode required for episode")
		}
		if req.SeriesIMDB == "" {
			return errors.New("series_imdb required for episode")
		}
	case "":
		return errors.New("type required (movie|episode)")
	default:
		return fmt.Errorf("unsupported type %q", req.Type)
	}

	if req.Magnet == "" {
		// TODO(follow-up): when omitted, resolve via Prowlarr/Torrentio
		// inside gostream. Currently the caller (e.g. the Phantom
		// Library Jellyfin plugin) is expected to supply a magnet.
		return errors.New("magnet required (indexer resolution not yet implemented)")
	}
	if !strings.HasPrefix(req.Magnet, "magnet:?") {
		return errors.New("magnet must start with magnet:?")
	}
	if library.HashFromMagnet(req.Magnet) == "" {
		return errors.New("magnet missing valid btih hash")
	}
	return nil
}

func hashSuffix(hash string) string {
	if len(hash) >= 8 {
		return hash[len(hash)-8:]
	}
	return hash
}

func imdbForStub(req *addRequest) string {
	if req.Type == "episode" {
		return req.SeriesIMDB
	}
	return req.IMDB
}
