package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseEpisodeFromFilenamePackLayouts(t *testing.T) {
	cases := []struct {
		path    string
		season  int
		episode int
	}{
		{"Avatar.S02E01.The.Avatar.State.mkv", 2, 1},
		{"Avatar The Last Airbender 2x01 The Avatar State.mkv", 2, 1},
		{"Avatar/Season 02/01 - The Avatar State.mkv", 2, 1},
		{"Avatar/Season.02/01 - The Avatar State.mkv", 2, 1},
		{"Avatar/Book 2/Chapter 1 - The Avatar State.mkv", 2, 1},
		{"Avatar/Book 2/201 - The Avatar State.mkv", 2, 1},
		{"Avatar - 201 - The Avatar State.mkv", 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			season, episode, ok := ParseEpisodeFromFilename(tc.path)
			if !ok || season != tc.season || episode != tc.episode {
				t.Fatalf("got (%d,%d,%v), want (%d,%d,true)", season, episode, ok, tc.season, tc.episode)
			}
		})
	}
}

func TestSelectEpisodeFileFixtureBookChapter(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "avatar_pack_book_chapter.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files []FileStat
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatal(err)
	}
	file, ok := SelectEpisodeFile(files, 2, 1)
	if !ok || file.ID != 201 {
		t.Fatalf("selected %+v ok=%v, want id=201", file, ok)
	}
}

func TestSelectEpisodeFileWrongEpisodeRejected(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	_, ok := SelectEpisodeFile([]FileStat{{ID: 1, Path: "Avatar - 202 - The Cave.mkv", Length: 5 * gb}}, 2, 1)
	if ok {
		t.Fatal("wrong episode selected")
	}
}
