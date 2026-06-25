// Package library is the single source of truth for stub-file format,
// filename conventions, and video-file filtering used both by the sync
// engines and by the external POST /api/library/add HTTP endpoint.
//
// Engines previously inlined private createMKV / filterVideoFiles /
// buildMovieFilename helpers; those now delegate here so that any
// external client and the cron-driven syncer produce byte-identical
// artifacts.
package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// FileStat mirrors the file-list entries returned by the GoStorm
// /torrents get response. The engines package re-exports this as a
// type alias so existing call sites keep compiling unchanged.
type FileStat struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

// Size thresholds (bytes). Kept here so both the syncer's accept/reject
// logic and the external library/add handler share the same band.
const (
	Movie4KMinBytes    int64 = 10 * 1024 * 1024 * 1024 // 10 GB
	Movie4KMaxBytes    int64 = 60 * 1024 * 1024 * 1024 // 60 GB
	Movie1080pMinBytes int64 = 4 * 1024 * 1024 * 1024  // 4 GB
	Movie1080pMaxBytes int64 = 20 * 1024 * 1024 * 1024 // 20 GB
	EpisodeMinBytes    int64 = 1                         // effectively no minimum
	EpisodeMaxBytes    int64 = 30 * 1024 * 1024 * 1024 // 30 GB
)

var videoExts = map[string]struct{}{
	".mkv": {}, ".mp4": {}, ".avi": {}, ".mov": {}, ".m4v": {},
}

// IsVideoFile reports whether p has a recognized video extension.
func IsVideoFile(p string) bool {
	_, ok := videoExts[strings.ToLower(filepath.Ext(p))]
	return ok
}

// FilterVideoFiles keeps only video files whose size falls in the movie
// 4K (is4K=true) or 1080p (is4K=false) band. Order is preserved.
func FilterVideoFiles(files []FileStat, is4K bool) []FileStat {
	minSize := Movie1080pMinBytes
	maxSize := Movie1080pMaxBytes
	if is4K {
		minSize = Movie4KMinBytes
		maxSize = Movie4KMaxBytes
	}
	var out []FileStat
	for _, f := range files {
		if !IsVideoFile(f.Path) {
			continue
		}
		if f.Length >= minSize && f.Length <= maxSize {
			out = append(out, f)
		}
	}
	return out
}

// FilterEpisodeFiles keeps only video files inside the episode size
// band (1–30 GB) — matches the TV engine's per-episode acceptance.
func FilterEpisodeFiles(files []FileStat) []FileStat {
	var out []FileStat
	for _, f := range files {
		if !IsVideoFile(f.Path) {
			continue
		}
		if f.Length >= EpisodeMinBytes && f.Length <= EpisodeMaxBytes {
			out = append(out, f)
		}
	}
	return out
}

var (
	reTVEpNum          = regexp.MustCompile(`(?i)[Ss](\d+)[Ee](\d+)`)
	reTV1xEp           = regexp.MustCompile(`(?i)(\d+)x(\d+)`)
	reTVCombinedEp     = regexp.MustCompile(`(?i)(?:^|[\s._-])([1-9])(\d{2})(?:[\s._-]|$)`)
	reTVSeasonDir      = regexp.MustCompile(`(?i)(?:^|[ ._-])(?:(?:season|book)[ ._-]*|s)(\d{1,3})(?:$|[ ._-])`)
	reTVEpisodeOnly    = regexp.MustCompile(`(?i)(?:^|[\s._-])(?:(?:e(?:p(?:isode)?)?|chapter)[\s._-]*)?(\d{1,3})(?:[\s._-]|$)`)
	reTVChapterEpisode = regexp.MustCompile(`(?i)(?:^|[\s._-])chapter[\s._-]*(\d{1,3})(?:[\s._-]|$)`)
)

// ParseEpisodeFromFilename extracts season/episode tokens from torrent
// paths. It recognizes S01E02 and 1x02 forms, plus episode-only names
// under Season folders (for example Season 01/01 - Pilot.mkv).
func ParseEpisodeFromFilename(path string) (season int, episode int, ok bool) {
	name := filepath.Base(path)
	if m := reTVEpNum.FindStringSubmatch(name); len(m) == 3 {
		_, _ = fmt.Sscanf(m[1], "%d", &season)
		_, _ = fmt.Sscanf(m[2], "%d", &episode)
		return season, episode, season > 0 && episode > 0
	}
	if m := reTV1xEp.FindStringSubmatch(name); len(m) == 3 {
		_, _ = fmt.Sscanf(m[1], "%d", &season)
		_, _ = fmt.Sscanf(m[2], "%d", &episode)
		return season, episode, season > 0 && episode > 0
	}
	if m := reTVCombinedEp.FindStringSubmatch(name); len(m) == 3 {
		_, _ = fmt.Sscanf(m[1], "%d", &season)
		_, _ = fmt.Sscanf(m[2], "%d", &episode)
		if season > 0 && episode > 0 && episode <= 99 {
			return season, episode, true
		}
	}

	season = seasonFromPath(path)
	if season <= 0 {
		return 0, 0, false
	}

	if isBookSeasonPath(path) {
		for _, m := range reTVChapterEpisode.FindAllStringSubmatch(name, -1) {
			var ep int
			_, _ = fmt.Sscanf(m[1], "%d", &ep)
			if ep > 0 && ep <= 200 {
				return season, ep, true
			}
		}
		return 0, 0, false
	}

	for _, re := range []*regexp.Regexp{reTVChapterEpisode, reTVEpisodeOnly} {
		for _, m := range re.FindAllStringSubmatch(name, -1) {
			var ep int
			_, _ = fmt.Sscanf(m[1], "%d", &ep)
			if ep >= 100 && ep/100 == season && ep%100 > 0 {
				return season, ep % 100, true
			}
			if ep > 0 && ep <= 200 && ep != 480 && ep != 720 && ep != 1080 {
				return season, ep, true
			}
		}
	}

	return 0, 0, false
}

func seasonFromPath(path string) int {
	path = filepath.ToSlash(path)
	parts := strings.Split(path, "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if m := reTVSeasonDir.FindStringSubmatch(parts[i]); len(m) == 2 {
			var season int
			_, _ = fmt.Sscanf(m[1], "%d", &season)
			return season
		}
	}
	return 0
}

func isBookSeasonPath(path string) bool {
	path = filepath.ToSlash(path)
	parts := strings.Split(path, "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if strings.Contains(strings.ToLower(parts[i]), "book") {
			return true
		}
	}
	return false
}

// SelectEpisodeFile returns the largest valid video file whose basename
// matches the requested season/episode. It intentionally does not fall
// back to the largest unrelated video file; season/series packs must not
// create a valid-looking stub pointing at the wrong episode.
func SelectEpisodeFile(files []FileStat, season, episode int) (FileStat, bool) {
	var best FileStat
	var found bool
	for _, f := range FilterEpisodeFiles(files) {
		fs, fe, ok := ParseEpisodeFromFilename(f.Path)
		if !ok || fs != season || fe != episode {
			continue
		}
		if !found || f.Length > best.Length {
			best = f
			found = true
		}
	}
	return best, found
}

// MovieStreamMeta carries the minimum information BuildMovieFilename
// needs from the source stream classification.
type MovieStreamMeta struct {
	Title string // raw stream title, used for HDR/DV/Atmos/REMUX tag detection
	Hash  string // 40-char info hash; last 8 chars become the filename suffix
	Is4K  bool
}

var (
	reMovHDR       = regexp.MustCompile(`(?i)\bhdr\b|hdr10\+?`)
	reMovDV        = regexp.MustCompile(`(?i)\bdv\b|dovi|dolby.?vision`)
	reMovAtmos     = regexp.MustCompile(`(?i)atmos`)
	reMov51        = regexp.MustCompile(`(?i)5\.1|dts|ddp5|ddp|dd\+|eac3|ac3`)
	reMovRemux     = regexp.MustCompile(`(?i)\bremux\b`)
	reMovTitleYear = regexp.MustCompile(`(.+?)[._\s]\(?((?:19|20)\d{2})\)?`)
	reMovAlnum     = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
	reMovUnder     = regexp.MustCompile(`_+`)
)

// SanitizeMovieFilename collapses anything that isn't alnum/._- into '_'.
func SanitizeMovieFilename(s string) string {
	s = reMovAlnum.ReplaceAllString(s, "_")
	s = reMovUnder.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// BuildMovieFilename produces the canonical movie stub filename, e.g.
//
//	The.Matrix_1999_2160p_DV_Atmos_REMUX_abc12345.mkv
//
// releaseDate is a "YYYY-MM-DD" or just "YYYY" prefix; if neither is
// available we try to recover the year from the title.
func BuildMovieFilename(title, releaseDate string, stream MovieStreamMeta) string {
	year := ""
	if len(releaseDate) >= 4 {
		year = releaseDate[:4]
	} else if m := reMovTitleYear.FindStringSubmatch(title); len(m) > 2 {
		year = m[2]
	}

	base := SanitizeMovieFilename(title)
	if year != "" {
		base = fmt.Sprintf("%s_%s", base, year)
	}

	if stream.Is4K {
		base += "_2160p"
	} else {
		base += "_1080p"
	}

	if reMovDV.MatchString(stream.Title) {
		base += "_DV"
	} else if reMovHDR.MatchString(stream.Title) {
		base += "_HDR"
	}

	if reMovAtmos.MatchString(stream.Title) {
		base += "_Atmos"
	} else if reMov51.MatchString(stream.Title) {
		base += "_5.1"
	}

	if reMovRemux.MatchString(stream.Title) {
		base += "_REMUX"
	}

	hash := stream.Hash
	suffix := hash
	if len(hash) >= 8 {
		suffix = hash[len(hash)-8:]
	}
	return fmt.Sprintf("%s_%s.mkv", base, suffix)
}

var (
	reTVSanitize = regexp.MustCompile(`[<>:"/\\|?*'"&]`)
	reTVSpaces   = regexp.MustCompile(`\s+`)
	reTVUnders   = regexp.MustCompile(`_+`)
)

// SanitizeName cleans a show name for use as a directory or filename
// component. Matches the legacy TV engine sanitizer byte-for-byte.
func SanitizeName(name string) string {
	clean := reTVSanitize.ReplaceAllString(name, "")
	clean = reTVSpaces.ReplaceAllString(clean, "_")
	clean = reTVUnders.ReplaceAllString(clean, "_")
	return strings.Trim(clean, "_")
}

// GetShowFolderName produces "<Sanitized Show> (YYYY)" when firstAirDate
// is present, else just the sanitized show name.
func GetShowFolderName(showName, firstAirDate string) string {
	clean := SanitizeName(showName)
	year := ""
	if len(firstAirDate) >= 4 {
		year = firstAirDate[:4]
	}
	if year != "" {
		return fmt.Sprintf("%s (%s)", clean, year)
	}
	return clean
}

// BuildEpisodeFilename produces the canonical TV episode stub filename,
// e.g. Stranger_Things_S01E04_abc12345.mkv.
func BuildEpisodeFilename(show string, season, episode int, hash8 string) string {
	return fmt.Sprintf("%s_S%02dE%02d_%s.mkv", SanitizeName(show), season, episode, hash8)
}

// WriteStub writes the JSON stub used by gostream's FUSE layer. It is
// the single canonical writer for stub files — sync engines and the
// /api/library/add HTTP handler both call this so that the on-disk
// format is identical regardless of source.
//
// Returns nil on success, error otherwise. Existence of the target file
// is NOT checked here; callers that need idempotent behaviour must stat
// first.
func WriteStub(stubPath, streamURL string, fileSize int64, magnet, imdbID string) error {
	data := map[string]interface{}{
		"url":    streamURL,
		"size":   fileSize,
		"magnet": magnet,
		"imdb":   imdbID,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal stub: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(stubPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(stubPath), err)
	}
	if err := os.WriteFile(stubPath, jsonData, 0644); err != nil {
		return fmt.Errorf("write %s: %w", stubPath, err)
	}
	return nil
}

// ReadStub loads a stub JSON file. Returns the parsed fields used by
// /api/library/remove (url, magnet, size).
type Stub struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Magnet string `json:"magnet"`
	IMDB   string `json:"imdb"`
}

// ReadStub parses a stub file. Only the JSON format is supported here
// — the legacy text format is read inside the syncer's own
// rehydrate/cleanup paths and is not exposed to API clients.
func ReadStub(stubPath string) (*Stub, error) {
	raw, err := os.ReadFile(stubPath)
	if err != nil {
		return nil, err
	}
	var s Stub
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse stub %s: %w", stubPath, err)
	}
	return &s, nil
}

// RealToFuse converts a stub's real filesystem path (under PhysicalSourcePath)
// into the equivalent path under the FUSE virtual mount.
//
// Returns an error if realPath is not inside realRoot.
func RealToFuse(realPath, realRoot, fuseRoot string) (string, error) {
	absReal, err := filepath.Abs(realPath)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(realRoot)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absReal)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("path %s is not under real root %s", realPath, realRoot)
	}
	return filepath.Join(fuseRoot, rel), nil
}

// HashFromStreamURL parses the "link=<hash>" query parameter out of a
// stub's stream URL. Returns "" if no 40-char hex hash is present.
var reStubHashURL = regexp.MustCompile(`link=([a-fA-F0-9]{40})`)

func HashFromStreamURL(streamURL string) string {
	m := reStubHashURL.FindStringSubmatch(streamURL)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// StreamIndexFromURL parses the "index=<n>" query parameter out of a
// stub's stream URL. Returns false if no non-negative index is present.
var reStubIndexURL = regexp.MustCompile(`(?:[?&])index=([0-9]+)(?:&|$)`)

func StreamIndexFromURL(streamURL string) (int, bool) {
	m := reStubIndexURL.FindStringSubmatch(streamURL)
	if len(m) < 2 {
		return 0, false
	}
	idx, err := strconv.Atoi(m[1])
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

// HashFromMagnet extracts the 32-40 hex info hash out of a magnet URI.
var reStubMagnetHash = regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{32,40})`)

func HashFromMagnet(magnet string) string {
	m := reStubMagnetHash.FindStringSubmatch(magnet)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}
