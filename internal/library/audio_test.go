package library

import "testing"

func TestSelectPreferredMainAudioEnglishHeuristics(t *testing.T) {
	cases := []struct {
		name       string
		tracks     []AudioTrack
		wantOK     bool
		wantIndex  int
		wantReason string
	}{
		{
			name:   "untagged english token",
			tracks: []AudioTrack{{StreamIndex: 3, Title: "Main English", Codec: "aac"}},
			wantOK: true, wantIndex: 3,
		},
		{
			name:   "bare en free text forbidden",
			tracks: []AudioTrack{{StreamIndex: 3, Title: "Audio en Espanol", Codec: "aac"}},
			wantOK: false,
		},
		{
			name:   "substring poleng forbidden",
			tracks: []AudioTrack{{StreamIndex: 3, Title: "poleng", Codec: "aac"}},
			wantOK: false,
		},
		{
			name:   "french substring forbidden",
			tracks: []AudioTrack{{StreamIndex: 3, Title: "French", Codec: "aac"}},
			wantOK: false,
		},
		{
			name: "commentary excluded when main exists",
			tracks: []AudioTrack{
				{StreamIndex: 1, Language: "eng", Title: "English Director Commentary"},
				{StreamIndex: 2, Language: "eng", Title: "English Main"},
			},
			wantOK: true, wantIndex: 2,
		},
		{
			name:   "only descriptive invalid main",
			tracks: []AudioTrack{{StreamIndex: 1, Language: "eng", Title: "English Audio Description"}},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			track, _, ok := SelectPreferredMainAudio(tc.tracks, []string{"eng", "en", "english"}, "eng")
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v track=%+v", ok, tc.wantOK, track)
			}
			if ok && track.StreamIndex != tc.wantIndex {
				t.Fatalf("stream index=%d want %d", track.StreamIndex, tc.wantIndex)
			}
		})
	}
}
