package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStub_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "movie.mkv")

	if err := WriteStub(path, "http://x/stream?link=abc&index=0&play", 12345, "magnet:?xt=urn:btih:dead", "tt1"); err != nil {
		t.Fatalf("WriteStub: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid json: %v", err)
	}
	if obj["url"] != "http://x/stream?link=abc&index=0&play" {
		t.Errorf("url mismatch: %v", obj["url"])
	}
	if obj["imdb"] != "tt1" {
		t.Errorf("imdb mismatch: %v", obj["imdb"])
	}
	if int64(obj["size"].(float64)) != 12345 {
		t.Errorf("size mismatch: %v", obj["size"])
	}
}

func TestWriteStub_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.mkv")
	if err := WriteStub(path, "a", 1, "magnet:?xt=urn:btih:aa", ""); err != nil {
		t.Fatal(err)
	}
	// Caller is responsible for idempotency check; WriteStub should not
	// fail on second call.
	if err := WriteStub(path, "b", 2, "magnet:?xt=urn:btih:bb", ""); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	s, err := ReadStub(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "b" || s.Size != 2 {
		t.Errorf("expected overwrite, got url=%q size=%d", s.URL, s.Size)
	}
}

func TestFilterVideoFiles_FiltersByExtAndSize(t *testing.T) {
	tooSmall := FileStat{ID: 1, Path: "sample.mkv", Length: 100}
	good1080 := FileStat{ID: 2, Path: "movie.mkv", Length: 8 * 1024 * 1024 * 1024}
	wrongExt := FileStat{ID: 3, Path: "readme.txt", Length: 8 * 1024 * 1024 * 1024}
	good4K := FileStat{ID: 4, Path: "4k.mkv", Length: 30 * 1024 * 1024 * 1024}
	tooBig := FileStat{ID: 5, Path: "huge.mkv", Length: 80 * 1024 * 1024 * 1024}

	in := []FileStat{tooSmall, good1080, wrongExt, good4K, tooBig}

	got := FilterVideoFiles(in, false) // 1080p band
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("1080p filter: got %+v want only id=2", got)
	}

	got = FilterVideoFiles(in, true) // 4K band
	if len(got) != 1 || got[0].ID != 4 {
		t.Errorf("4K filter: got %+v want only id=4", got)
	}
}

func TestFilterEpisodeFiles(t *testing.T) {
	in := []FileStat{
		{Path: "sample.mkv", Length: 1},
		{Path: "ep.mkv", Length: 2 * 1024 * 1024 * 1024},
		{Path: "readme.txt", Length: 5 * 1024 * 1024 * 1024},
		{Path: "huge.mkv", Length: 50 * 1024 * 1024 * 1024},
	}
	got := FilterEpisodeFiles(in)
	if len(got) != 2 || got[0].Path != "sample.mkv" || got[1].Path != "ep.mkv" {
		t.Errorf("episode filter: got %+v", got)
	}
}

func TestParseEpisodeFromFilename(t *testing.T) {
	cases := []struct {
		name    string
		season  int
		episode int
		ok      bool
	}{
		{"Show.S01E02.mkv", 1, 2, true},
		{"Show.1x02.mkv", 1, 2, true},
		{"Show.S01E02.sample.txt", 1, 2, true},
		{"Season 01/01 - Pilot.mkv", 1, 1, true},
		{"Season.02/Show - Ep 03 - Title.mkv", 2, 3, true},
		{"Show.S02.1080p/03 - Return to Omashu.mkv", 2, 3, true},
		{"Show S01-S03/Season 02/03 - Return to Omashu.mkv", 2, 3, true},
		{"[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 2 - Earth (Livre 2 - La terre)/[Avatar Realms] Avatar The Last Airbender 2x03 Return to Omashu [x264 FHD multi sub FR].mkv", 2, 3, true},
		{"[Avatar Realms] Avatar The last airbender 1080p MULTI VF-VO-VOSTFR/Book 2 - Earth (Livre 2 - La terre)/03 - Return to Omashu [x264 FHD multi sub FR].mkv", 0, 0, false},
		{"Show.E02.mkv", 0, 0, false},
	}
	for _, tc := range cases {
		s, e, ok := ParseEpisodeFromFilename(tc.name)
		if s != tc.season || e != tc.episode || ok != tc.ok {
			t.Fatalf("ParseEpisodeFromFilename(%q)=(%d,%d,%v), want (%d,%d,%v)", tc.name, s, e, ok, tc.season, tc.episode, tc.ok)
		}
	}
}

func TestSelectEpisodeFile_TargetMatchOnlyLargestMatch(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	files := []FileStat{
		{ID: 1, Path: "Show.S01E01.mkv", Length: 20 * gb},
		{ID: 2, Path: "Show.S01E02.720p.mkv", Length: 200 * 1024 * 1024},
		{ID: 3, Path: "Show.S01E02.1080p.mkv", Length: 500 * 1024 * 1024},
		{ID: 5, Path: "Season 01/02 - Target From Pack.mkv", Length: 400 * 1024 * 1024},
		{ID: 4, Path: "Show.S01E03.mkv", Length: 25 * gb},
	}
	got, ok := SelectEpisodeFile(files, 1, 2)
	if !ok || got.ID != 3 {
		t.Fatalf("SelectEpisodeFile got %+v ok=%v, want id=3", got, ok)
	}
	if _, ok := SelectEpisodeFile(files, 1, 4); ok {
		t.Fatalf("expected no fallback to unrelated episode")
	}
}

func TestBuildMovieFilename_Deterministic(t *testing.T) {
	stream := MovieStreamMeta{
		Title: "The Matrix 1080p HDR Atmos REMUX",
		Hash:  "0123456789abcdef0123456789abcdef01234567",
		Is4K:  false,
	}
	got := BuildMovieFilename("The Matrix", "1999-03-31", stream)
	want := "The_Matrix_1999_1080p_HDR_Atmos_REMUX_01234567.mkv"
	if got != want {
		t.Errorf("BuildMovieFilename:\n got  %q\n want %q", got, want)
	}

	// Same input → same output (deterministic).
	if again := BuildMovieFilename("The Matrix", "1999-03-31", stream); again != got {
		t.Errorf("non-deterministic: %q != %q", again, got)
	}

	// 4K + DV beats HDR.
	stream4K := MovieStreamMeta{
		Title: "Movie 2160p Dolby.Vision DTS",
		Hash:  "ffffffffffffffffffffffffffffffffffffffff",
		Is4K:  true,
	}
	got4K := BuildMovieFilename("Movie", "2020-01-01", stream4K)
	want4K := "Movie_2020_2160p_DV_5.1_ffffffff.mkv"
	if got4K != want4K {
		t.Errorf("4K:\n got  %q\n want %q", got4K, want4K)
	}
}

func TestBuildEpisodeFilename(t *testing.T) {
	got := BuildEpisodeFilename("Stranger Things", 1, 4, "abc12345")
	want := "Stranger_Things_S01E04_abc12345.mkv"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestGetShowFolderName(t *testing.T) {
	if got := GetShowFolderName("Stranger Things", "2016-07-15"); got != "Stranger_Things (2016)" {
		t.Errorf("got %q", got)
	}
	if got := GetShowFolderName("No Year", ""); got != "No_Year" {
		t.Errorf("got %q", got)
	}
}

func TestRealToFuse(t *testing.T) {
	got, err := RealToFuse("/mnt/real/movies/x.mkv", "/mnt/real", "/mnt/fuse")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/mnt/fuse/movies/x.mkv" {
		t.Errorf("got %q", got)
	}
	if _, err := RealToFuse("/elsewhere/x.mkv", "/mnt/real", "/mnt/fuse"); err == nil {
		t.Errorf("expected error for path outside root")
	}
}

func TestHashFromStreamURL(t *testing.T) {
	url := "http://host/stream?link=" + "abcdef0123456789abcdef0123456789abcdef01" + "&index=0&play"
	if got := HashFromStreamURL(url); got != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("got %q", got)
	}
	if HashFromStreamURL("http://host/none") != "" {
		t.Errorf("expected empty")
	}
}

func TestStreamIndexFromURL(t *testing.T) {
	idx, ok := StreamIndexFromURL("http://host/stream?link=abcdef0123456789abcdef0123456789abcdef01&index=31&play")
	if !ok || idx != 31 {
		t.Fatalf("StreamIndexFromURL got (%d,%v), want (31,true)", idx, ok)
	}
	if _, ok := StreamIndexFromURL("http://host/stream?index=-1"); ok {
		t.Fatalf("negative index should not parse")
	}
	if _, ok := StreamIndexFromURL("http://host/stream?link=abc"); ok {
		t.Fatalf("missing index should not parse")
	}
}

func TestHashFromMagnet(t *testing.T) {
	if got := HashFromMagnet("magnet:?xt=urn:btih:DEADBEEF0123456789ABCDEF0123456789ABCDEF&dn=x"); got != "deadbeef0123456789abcdef0123456789abcdef" {
		t.Errorf("got %q", got)
	}
	if HashFromMagnet("not a magnet") != "" {
		t.Errorf("expected empty")
	}
}
