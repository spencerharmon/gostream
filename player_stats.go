package main

import (
	"net/http"
	"time"

	"gostream/internal/gostorm/torr"
	"gostream/internal/gostorm/torr/state"
	"gostream/internal/monitor/collector"
	"gostream/internal/monitor/dashboard"
	"gostream/internal/warmup"
)

func registerPlayerStats(mux *http.ServeMux, lib *dashboard.LibraryHandler, c *collector.Collector) {
	mux.HandleFunc("/api/library/stats", lib.StatsHandler(dashboard.PlayerStatsFunc(func(hash string) (state.PlayerTorrentStats, collector.StreamStats, bool) {
		torrent, found := torr.PlayerStats(hash)
		return torrent, c.PlayerStream(hash), found
	})))
}

// samplePlayerStreams runs on the EXISTING collector tick, never on a request.
// One hash can have multiple files/handles: select the most recently requested
// handle (opened time breaks the no-read case). Do not sum unrelated buffers.
func samplePlayerStreams() map[string]collector.StreamStats {
	type target struct {
		stats  collector.StreamStats
		path   string
		fileID int
		size   int64
	}
	targets := make(map[string]target)
	activeHandles.Range(func(key, _ interface{}) bool {
		h := key.(*MkvHandle)
		h.mu.Lock()
		hash, path, fileID, size, activity := h.hash, h.path, h.fileID, h.size, h.playerActivity
		h.mu.Unlock()
		if hash == "" {
			return true
		}
		when := activity.LastRequestAt
		if when.IsZero() {
			when = activity.OpenedAt
		}
		if old, ok := targets[hash]; ok {
			prev := old.stats.LastRequestAt
			if prev.IsZero() {
				prev = old.stats.OpenedAt
			}
			if !when.After(prev) {
				return true
			}
		}
		targets[hash] = target{collector.StreamStats{StreamActivity: activity, Active: true}, path, fileID, size}
		return true
	})
	out := make(map[string]collector.StreamStats, len(targets))
	for hash, target := range targets {
		s := target.stats
		if s.ReadOffset != nil {
			off := *s.ReadOffset
			if off >= 0 && off <= target.size {
				end := off
				if warmup.DiskWarmup != nil {
					// This is recorded contiguous head coverage, NOT disk allocation size.
					if head := warmup.DiskWarmup.GetAvailableRange(hash, target.fileID); head > end {
						end = min(head, target.size)
					}
				}
				end = raCache.contiguousEnd(target.path, end, target.size)
				buffered := end - off
				s.BufferedAheadBytes = &buffered
			}
		}
		// No trustworthy file-specific duration/bitrate is presently retained by
		// FUSE. Leave bitrate (and therefore seconds) nil rather than substituting
		// download speed, read throughput, or a movie-size heuristic.
		now := time.Now()
		s.SampledAt = &now
		out[hash] = s
	}
	return out
}

// contiguousEnd returns the exclusive end of cached coverage beginning at off.
// A hole terminates coverage even if there are cached chunks far beyond it.
// No copying/touching LRU timestamps, and no claim that readahead capacity is data.
func (c *ReadAheadCache) contiguousEnd(path string, off, size int64) int64 {
	s := c.getShard(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	end := off
	for end < size {
		b, ok := s.buffers[c.chunkKey(path, end)]
		if !ok || end < b.start || end > b.end {
			break
		}
		end = min(b.end+1, size)
	}
	return end
}
