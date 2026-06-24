package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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
}

// LibraryHandler serves POST /api/library/add and /api/library/remove.
type LibraryHandler struct {
	cfg     LibraryConfig
	gostorm GoStormClient
}

// NewLibraryHandler constructs a handler. Both args are required and
// must be non-nil at registration time.
func NewLibraryHandler(cfg LibraryConfig, gs GoStormClient) *LibraryHandler {
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 45
	}
	return &LibraryHandler{cfg: cfg, gostorm: gs}
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
}

type addResponse struct {
	StubPath string `json:"stub_path"`
	FusePath string `json:"fuse_path"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
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

	var bestFile library.FileStat
	if req.Type == "movie" {
		videoFiles := library.FilterVideoFiles(files, is4K)
		if len(videoFiles) == 0 {
			h.cleanupTorrentIfUnreferenced(hash)
			writeJSONError(w, http.StatusUnprocessableEntity, "no_valid_files")
			return
		}
		sort.Slice(videoFiles, func(i, j int) bool {
			return videoFiles[i].Length > videoFiles[j].Length
		})
		bestFile = videoFiles[0]
	} else {
		var ok bool
		bestFile, ok = library.SelectEpisodeFile(files, req.Season, req.Episode)
		if !ok {
			h.cleanupTorrentIfUnreferenced(hash)
			writeJSONError(w, http.StatusUnprocessableEntity, "target_episode_not_found")
			return
		}
	}

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

// Remove handles POST /api/library/remove. Idempotent: missing torrent
// or missing stub is not an error.
func (h *LibraryHandler) Remove(w http.ResponseWriter, r *http.Request) {
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
	if hash == "" || h.stubTreeReferencesHash(hash) {
		return
	}
	_ = h.gostorm.RemoveTorrent(context.Background(), hash)
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
