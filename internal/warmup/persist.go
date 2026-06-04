package warmup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// persistEntry tracks a hash that has been explicitly marked persistent
// so that warmup writes for it ignore FileSize cap, and eviction prefers
// non-persistent entries.
type persistEntry struct {
	Priority int   `json:"priority"`
	MarkedAt int64 `json:"markedAt"`
}

// persistFile is the on-disk format for persistMap. Lives next to
// warmup files at <warmup_dir>/persist.json.
type persistFile struct {
	Version int                     `json:"version"`
	Entries map[string]persistEntry `json:"entries"`
}

const persistFileName = "persist.json"
const persistVersion = 1

// MarkPersistent marks a torrent hash as persistent. Subsequent
// WriteChunk calls for this hash ignore the FileSize cap, and
// eviction will prefer non-persistent entries.
func (d *DiskWarmupCache) MarkPersistent(hash string, priority int) {
	if hash == "" {
		return
	}
	if priority < 0 {
		priority = 0
	}
	if priority > 100 {
		priority = 100
	}
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	d.persistMap[hash] = persistEntry{Priority: priority, MarkedAt: time.Now().Unix()}
	d.savePersistLocked()
}

// UnmarkPersistent removes the persistent mark on a hash. Cached
// bytes remain on disk but become eligible for normal LRU eviction.
func (d *DiskWarmupCache) UnmarkPersistent(hash string) {
	if hash == "" {
		return
	}
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	delete(d.persistMap, hash)
	d.savePersistLocked()
}

// IsPersistent reports whether a hash has been marked persistent.
func (d *DiskWarmupCache) IsPersistent(hash string) bool {
	if hash == "" {
		return false
	}
	d.persistMu.RLock()
	defer d.persistMu.RUnlock()
	_, ok := d.persistMap[hash]
	return ok
}

// PersistInfo returns the persistEntry for a hash, plus ok=false if
// not marked.
func (d *DiskWarmupCache) PersistInfo(hash string) (persistEntry, bool) {
	d.persistMu.RLock()
	defer d.persistMu.RUnlock()
	e, ok := d.persistMap[hash]
	return e, ok
}

// PersistSnapshot returns a copy of the persistMap for read-only use
// (e.g. eviction policy, status reporting).
func (d *DiskWarmupCache) PersistSnapshot() map[string]persistEntry {
	d.persistMu.RLock()
	defer d.persistMu.RUnlock()
	out := make(map[string]persistEntry, len(d.persistMap))
	for k, v := range d.persistMap {
		out[k] = v
	}
	return out
}

func (d *DiskWarmupCache) persistPath() string {
	return filepath.Join(d.dir, persistFileName)
}

// loadPersistLocked reads persist.json. Must be called with persistMu held.
// On parse error, logs a warning and starts with an empty map (does not crash).
func (d *DiskWarmupCache) loadPersistLocked() {
	d.persistMap = make(map[string]persistEntry)
	data, err := os.ReadFile(d.persistPath())
	if err != nil {
		if !os.IsNotExist(err) {
			logf.Printf("[Warmup] persist.json read error: %v; starting with empty map", err)
		}
		return
	}
	var pf persistFile
	if err := json.Unmarshal(data, &pf); err != nil {
		logf.Printf("[Warmup] persist.json parse error: %v; starting with empty map", err)
		return
	}
	if pf.Entries != nil {
		d.persistMap = pf.Entries
	}
}

// savePersistLocked writes persist.json atomically. Must be called with
// persistMu held.
func (d *DiskWarmupCache) savePersistLocked() {
	if d.dir == "" {
		return
	}
	pf := persistFile{Version: persistVersion, Entries: d.persistMap}
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		logf.Printf("[Warmup] persist.json marshal error: %v", err)
		return
	}
	tmp := d.persistPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		logf.Printf("[Warmup] persist.json write error: %v", err)
		return
	}
	if err := os.Rename(tmp, d.persistPath()); err != nil {
		logf.Printf("[Warmup] persist.json rename error: %v", err)
		_ = os.Remove(tmp)
	}
}

// initPersist sets up the persistMap (load from disk + initialise the
// in-memory map). Safe to call once during DiskWarmupCache construction.
func (d *DiskWarmupCache) initPersist() {
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	d.loadPersistLocked()
}

// persistMuType is sync.RWMutex; defined here to make the field
// declaration site in warmup.go obvious.
type persistMuType = sync.RWMutex
