package library

import (
	"strings"
	"unicode"
)

// AudioTrack describes one audio stream reported by ffprobe/gostream.
type AudioTrack struct {
	StreamIndex int    `json:"stream_index"`
	Language    string `json:"language"`
	Title       string `json:"title,omitempty"`
	Codec       string `json:"codec,omitempty"`
	Channels    int    `json:"channels,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

var commentaryAudioTokens = map[string]struct{}{
	"commentary":  {},
	"comment":     {},
	"director":    {},
	"descriptive": {},
	"description": {},
	"sdh":         {},
}

// NormalizeAudioLanguage folds common English spellings/codes to eng.
func NormalizeAudioLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, " ._-[](){}")
	switch s {
	case "eng", "en", "english":
		return "eng"
	default:
		return s
	}
}

// SelectPreferredMainAudio returns the first main audio stream matching
// preferred/required language constraints. Structured language tags may use
// en/eng/english; free-text title matching accepts english/eng tokens only.
func SelectPreferredMainAudio(tracks []AudioTrack, required []string, preferred string) (AudioTrack, bool, bool) {
	if len(tracks) == 0 {
		return AudioTrack{}, false, false
	}

	requiredSet := map[string]struct{}{}
	for _, lang := range required {
		if normalized := NormalizeAudioLanguage(lang); normalized != "" {
			requiredSet[normalized] = struct{}{}
		}
	}
	preferred = NormalizeAudioLanguage(preferred)
	if preferred == "" && len(requiredSet) == 1 {
		for lang := range requiredSet {
			preferred = lang
		}
	}

	var foundLanguage bool
	var firstMain AudioTrack
	var haveMain bool
	for _, track := range tracks {
		if audioMatchesLanguage(track, preferred, requiredSet) {
			foundLanguage = true
			if !isCommentaryOrDescriptive(track) {
				if !haveMain {
					firstMain = track
					haveMain = true
				}
				return track, true, true
			}
		}
	}
	return firstMain, foundLanguage, haveMain
}

func audioMatchesLanguage(track AudioTrack, preferred string, required map[string]struct{}) bool {
	structured := NormalizeAudioLanguage(track.Language)
	if structured != "" {
		if preferred != "" {
			return structured == preferred
		}
		if len(required) > 0 {
			_, ok := required[structured]
			return ok
		}
		return true
	}

	tokens := tokenizeAudioText(track.Title)
	textLang := ""
	if _, ok := tokens["english"]; ok {
		textLang = "eng"
	} else if _, ok := tokens["eng"]; ok {
		textLang = "eng"
	}
	if textLang == "" {
		return false
	}
	if preferred != "" {
		return textLang == preferred
	}
	if len(required) > 0 {
		_, ok := required[textLang]
		return ok
	}
	return true
}

func isCommentaryOrDescriptive(track AudioTrack) bool {
	tokens := tokenizeAudioText(track.Title)
	for token := range commentaryAudioTokens {
		if _, ok := tokens[token]; ok {
			return true
		}
	}
	if _, ok := tokens["audio"]; ok {
		if _, ok := tokens["description"]; ok {
			return true
		}
	}
	return false
}

func tokenizeAudioText(s string) map[string]struct{} {
	out := map[string]struct{}{}
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out[strings.ToLower(b.String())] = struct{}{}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return out
}
