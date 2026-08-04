package engines

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gostream/internal/library"
	"gostream/internal/prowlarr"
)

func TestTVClassifySeriesPackS01ToS03AndSeasonWindowOverlap(t *testing.T) {
	e := testTVEngine(t)
	stream := prowlarr.Stream{
		Title:    "Avatar: The Last Airbender - S01 to S03 - 1080p - Bluray AAC5.1 - X264-Rapta · Torrentio · 562 seeders",
		Name:     "Avatar: The Last Airbender - S01 to S03 - 1080p - Bluray AAC5.1 - X264-Rapta",
		InfoHash: "61dec87b710152d6121b59d0e2005a7b278799e9",
	}

	classified := e.classifyStream(stream)
	if classified == nil {
		t.Fatal("classifyStream returned nil")
	}
	if !classified.IsFullpack {
		t.Fatalf("expected S01 to S03 candidate to classify as fullpack: %+v", classified)
	}
	span := e.extractSeasonSpan(classified.Title)
	if span == nil || span[0] != 1 || span[1] != 3 {
		t.Fatalf("span=%v, want 1..3", span)
	}
	if !e.streamOverlapsSeasonWindow(classified, 2, 3) {
		t.Fatalf("series pack 1..3 should overlap target window 2..3")
	}
	if e.streamOverlapsSeasonWindow(classified, 4, 5) {
		t.Fatalf("series pack 1..3 should not overlap target window 4..5")
	}
	if classified.Seeders != 562 {
		t.Fatalf("seeders=%d, want 562", classified.Seeders)
	}
}

func TestTVClassifySeasonWordFullpackUsesSeasonNumber(t *testing.T) {
	e := testTVEngine(t)
	stream := prowlarr.Stream{
		Title:    "Example Show - Season 2 Complete 1080p Bluray · 50 seeders",
		Name:     "Example Show - Season 2 Complete 1080p Bluray",
		InfoHash: "abcdef0123456789abcdef0123456789abcdef01",
	}

	classified := e.classifyStream(stream)
	if classified == nil {
		t.Fatal("classifyStream returned nil")
	}
	if !classified.IsFullpack || classified.Season != 2 {
		t.Fatalf("classified=%+v, want fullpack season 2", classified)
	}
	if !e.streamOverlapsSeasonWindow(classified, 2, 2) {
		t.Fatalf("Season 2 fullpack should overlap target season 2")
	}
}

func TestTVProcessSingleSelectsRequestedEpisodeFromPack(t *testing.T) {
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	files := []library.FileStat{
		{ID: 23, Path: "[Avatar Realms] Avatar The Last Airbender 2x03 Return to Omashu.mkv", Length: 1142880878},
		{ID: 31, Path: "[Avatar Realms] Avatar The Last Airbender 2x11 The Desert.mkv", Length: 1175677822},
	}
	e, shutdown := testTVEngineWithGoStorm(t, hash, files)
	defer shutdown()

	created := e.processSingle(context.Background(), "Avatar The Last Airbender", TVStream{
		Title:        "Avatar The Last Airbender S02E03 1080p",
		Hash:         hash,
		Season:       2,
		QualityScore: 200,
	}, "2005-02-21")
	if created != 1 {
		t.Fatalf("created=%d, want 1", created)
	}

	stubPath := filepath.Join(e.tvDir, library.GetShowFolderName("Avatar The Last Airbender", "2005-02-21"), "Season.02", library.BuildEpisodeFilename("Avatar The Last Airbender", 2, 3, "61dec87b"))
	stub, err := library.ReadStub(stubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.URL, "index=23") {
		t.Fatalf("stub URL %q, want requested episode index=23", stub.URL)
	}
}

func TestTVProcessFullpackWritesAvatarS02E03WithCorrectIndex(t *testing.T) {
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	files := []library.FileStat{
		{ID: 1, Path: "[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 1 - Water/[Avatar Realms] Avatar The Last Airbender 1x01 The boy in the iceberg [x264 FHD multi sub FR].mkv", Length: 1115940681},
		{ID: 23, Path: "[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 2 - Earth (Livre 2 - La terre)/[Avatar Realms] Avatar The Last Airbender 2x03 Return to Omashu [x264 FHD multi sub FR].mkv", Length: 1142880878},
		{ID: 31, Path: "[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 2 - Earth (Livre 2 - La terre)/[Avatar Realms] Avatar The Last Airbender 2x11 The Desert  [x264 FHD multi sub FR].mkv", Length: 1175677822},
		{ID: 99, Path: "[Avatar Realms] Avatar The Last Airbender 2x99 Extras.mkv", Length: 1142880878},
	}
	e, shutdown := testTVEngineWithGoStorm(t, hash, files)
	defer shutdown()

	counts := e.processFullpack(context.Background(), "Avatar The Last Airbender", TVStream{
		Title:        "Avatar: The Last Airbender - S01 to S03 - 1080p - Bluray AAC5.1 - X264-Rapta",
		Hash:         hash,
		IsFullpack:   true,
		QualityScore: 200,
	}, "2005-02-21", 2, 3, map[int]bool{}, map[int]bool{}, map[int]int{2: 20})
	if counts[2] != 2 {
		t.Fatalf("created S02=%d, want 2 (counts=%v)", counts[2], counts)
	}
	if counts[1] != 0 {
		t.Fatalf("created outside target window: counts=%v", counts)
	}

	stubPath := filepath.Join(e.tvDir, library.GetShowFolderName("Avatar The Last Airbender", "2005-02-21"), "Season.02", library.BuildEpisodeFilename("Avatar The Last Airbender", 2, 3, "61dec87b"))
	stub, err := library.ReadStub(stubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.URL, "index=23") {
		t.Fatalf("stub URL %q, want S02E03 file index=23", stub.URL)
	}
	if stub.Size == 1175677822 {
		t.Fatalf("S02E03 stub used S02E11 size: %+v", stub)
	}
}

func TestTVProcessFullpackSelectsLargestDuplicateForSameEpisode(t *testing.T) {
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	files := []library.FileStat{
		{ID: 23, Path: "Avatar The Last Airbender 2x03 Return to Omashu 720p.mkv", Length: 1142880878},
		{ID: 123, Path: "Avatar The Last Airbender 2x03 Return to Omashu 1080p.mkv", Length: 2142880878},
	}
	e, shutdown := testTVEngineWithGoStorm(t, hash, files)
	defer shutdown()

	counts := e.processFullpack(context.Background(), "Avatar The Last Airbender", TVStream{
		Title:        "Avatar: The Last Airbender - S01 to S03 - 1080p - Bluray AAC5.1 - X264-Rapta",
		Hash:         hash,
		IsFullpack:   true,
		QualityScore: 200,
	}, "2005-02-21", 2, 2, map[int]bool{}, map[int]bool{}, map[int]int{2: 20})

	if counts[2] != 1 {
		t.Fatalf("created S02=%d, want 1 (counts=%v)", counts[2], counts)
	}
	stubPath := filepath.Join(e.tvDir, library.GetShowFolderName("Avatar The Last Airbender", "2005-02-21"), "Season.02", library.BuildEpisodeFilename("Avatar The Last Airbender", 2, 3, "61dec87b"))
	stub, err := library.ReadStub(stubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.URL, "index=123") {
		t.Fatalf("stub URL %q, want largest duplicate index=123", stub.URL)
	}
}

func TestTVProcessFullpackDoesNotRemoveTorrentWhenExistingEpisodeSkipped(t *testing.T) {
	hash := "61dec87b710152d6121b59d0e2005a7b278799e9"
	files := []library.FileStat{{ID: 23, Path: "Avatar The Last Airbender 2x03 Return to Omashu.mkv", Length: 1142880878}}
	removed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req["action"] {
		case "add":
			_ = json.NewEncoder(w).Encode(map[string]string{"hash": hash})
		case "get":
			_ = json.NewEncoder(w).Encode(TorrentStats{Hash: hash, FileStats: files})
		case "rem":
			removed = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	e := testTVEngine(t)
	e.gostorm = NewGoStormClient(server.URL)
	e.registry[e.episodeKey("Avatar The Last Airbender", 2, 3)] = TVEpisodeEntry{QualityScore: 1000, Hash: hash, FilePath: "existing.mkv"}
	counts := e.processFullpack(context.Background(), "Avatar The Last Airbender", TVStream{
		Title:        "Avatar: The Last Airbender - S01 to S03 - 1080p - Bluray AAC5.1 - X264-Rapta",
		Hash:         hash,
		IsFullpack:   true,
		QualityScore: 200,
	}, "2005-02-21", 2, 2, map[int]bool{}, map[int]bool{}, map[int]int{2: 20})

	if len(counts) != 0 {
		t.Fatalf("expected no created episodes, got %v", counts)
	}
	if removed {
		t.Fatalf("skipped existing pack episode should not remove torrent still referenced by registry")
	}
}

func testTVEngine(t *testing.T) *TVGoEngine {
	t.Helper()
	// Mirrors config.LoadConfig's default LanguageConfig so classify/quality
	// scoring behaves the same as production instead of never-matching nil
	// regexes (e.reITA/e.reExclLang are per-instance since the upstream
	// rebase replaced the old package-level reTVITA constant).
	preferredTerms := []string{"ita", "multi", "dual"}
	preferredFlags := []string{"IT"}
	excludedFlags := []string{
		"ES", "FR", "DE", "RU", "CN", "JP", "KR", "TH", "PT", "BR",
		"UA", "PL", "NL", "TR", "SA", "IN", "CZ", "HU", "RO",
	}
	return &TVGoEngine{
		tvDir:            t.TempDir(),
		stateDir:         t.TempDir(),
		logger:           log.New(io.Discard, "", 0),
		registry:         map[string]TVEpisodeEntry{},
		processedThisRun: map[string]bool{},
		blacklist:        BlacklistData{Hashes: map[string]string{}},
		reITA:            CompileLanguageRegex(preferredTerms, preferredFlags),
		reExclLang:       CompileLanguageRegex(ExcludedTitleTerms(excludedFlags), excludedFlags),
	}
}

func testTVEngineWithGoStorm(t *testing.T, hash string, files []library.FileStat) (*TVGoEngine, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrents" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req["action"] {
		case "add":
			_ = json.NewEncoder(w).Encode(map[string]string{"hash": hash})
		case "get":
			_ = json.NewEncoder(w).Encode(TorrentStats{Hash: hash, FileStats: files})
		case "rem":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "bad action", http.StatusBadRequest)
		}
	}))
	e := testTVEngine(t)
	e.gostorm = NewGoStormClient(server.URL)
	e.registryFile = filepath.Join(t.TempDir(), "registry.json")
	e.blacklistFile = filepath.Join(t.TempDir(), "blacklist.json")
	return e, func() {
		server.Close()
		_ = os.RemoveAll(e.tvDir)
	}
}
