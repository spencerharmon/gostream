// Package persist provides helpers for the Vault Mode prestage flow.
// Keeps the vault_handler decoupled from main.go's path helpers and
// independent of the api-add branch's internal/library package so the
// two PRs can land in either order.
package persist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Stub is the on-disk JSON layout of a virtual .mkv stub.
// Mirrors vfs.MkvJSON intentionally; redefined here to avoid an
// import cycle / coupling to the FUSE package from a pure helper.
type Stub struct {
	URL             string `json:"url"`
	Size            int64  `json:"size"`
	Magnet          string `json:"magnet"`
	Imdb            string `json:"imdb"`
	Persist         bool   `json:"persist,omitempty"`
	PersistPriority int    `json:"persist_priority,omitempty"`
	// Extra preserves unknown fields across rewrites so callers don't
	// silently strip future stub additions.
	Extra map[string]json.RawMessage `json:"-"`
}

// ReadStub parses a stub file at path. Returns ErrNotExist if the file
// is missing; returns a parse error if present-but-malformed.
func ReadStub(path string) (*Stub, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Decode into a generic map first so we can preserve unknown fields.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse stub: %w", err)
	}
	var s Stub
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse stub fields: %w", err)
	}
	known := map[string]struct{}{
		"url": {}, "size": {}, "magnet": {}, "imdb": {},
		"persist": {}, "persist_priority": {},
	}
	s.Extra = map[string]json.RawMessage{}
	for k, v := range raw {
		if _, ok := known[k]; !ok {
			s.Extra[k] = v
		}
	}
	return &s, nil
}

// WriteStubAtomic writes the stub to path via a tmp + rename. Unknown
// fields captured in Extra are preserved.
func WriteStubAtomic(path string, s *Stub) error {
	merged := map[string]json.RawMessage{}
	for k, v := range s.Extra {
		merged[k] = v
	}
	set := func(k string, v interface{}) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		merged[k] = b
		return nil
	}
	if err := set("url", s.URL); err != nil {
		return err
	}
	if err := set("size", s.Size); err != nil {
		return err
	}
	if err := set("magnet", s.Magnet); err != nil {
		return err
	}
	if err := set("imdb", s.Imdb); err != nil {
		return err
	}
	if s.Persist {
		if err := set("persist", true); err != nil {
			return err
		}
		if s.PersistPriority > 0 {
			if err := set("persist_priority", s.PersistPriority); err != nil {
				return err
			}
		} else {
			delete(merged, "persist_priority")
		}
	} else {
		delete(merged, "persist")
		delete(merged, "persist_priority")
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ExtractHashFromMagnet pulls the BTIH info-hash out of a magnet URI.
// Returns "" if not present.
func ExtractHashFromMagnet(magnet string) string {
	const key = "xt=urn:btih:"
	i := strings.Index(magnet, key)
	if i < 0 {
		return ""
	}
	rest := magnet[i+len(key):]
	if j := strings.IndexAny(rest, "&/"); j >= 0 {
		rest = rest[:j]
	}
	return strings.ToLower(strings.TrimSpace(rest))
}

// RealToFuse maps a physical stub path under realRoot to its FUSE-mount
// counterpart under fuseRoot. Returns an error if realPath is not under
// realRoot.
func RealToFuse(realPath, realRoot, fuseRoot string) (string, error) {
	if realRoot == "" || fuseRoot == "" {
		return "", errors.New("realRoot/fuseRoot must be set")
	}
	abs, err := filepath.Abs(realPath)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(realRoot)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %s not under %s", realPath, realRoot)
	}
	return filepath.Join(fuseRoot, rel), nil
}
