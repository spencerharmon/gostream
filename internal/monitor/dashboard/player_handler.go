package dashboard

import (
	"encoding/hex"
	"math"
	"net/http"
	"strings"
	"time"

	"gostream/internal/gostorm/torr/state"
	"gostream/internal/monitor/collector"
)

const (
	playerStallThreshold = 10 * time.Second
	playerIdleThreshold  = 30 * time.Second
)

// PlayerStatsSource must perform an indexed lookup, not call torrent.Status or
// walk the dashboard torrent list. The bool identifies a loaded engine torrent.
type PlayerStatsSource interface {
	PlayerStats(hash string) (state.PlayerTorrentStats, collector.StreamStats, bool)
}

type PlayerStatsFunc func(string) (state.PlayerTorrentStats, collector.StreamStats, bool)

func (f PlayerStatsFunc) PlayerStats(hash string) (state.PlayerTorrentStats, collector.StreamStats, bool) {
	return f(hash)
}

type playerStatsResponse struct {
	Hash string `json:"hash"`
	state.PlayerTorrentStats
	ReadOffset           *int64     `json:"read_offset"`
	BufferedAheadBytes   *int64     `json:"buffered_ahead_bytes"`
	BufferedAheadSeconds *float64   `json:"buffered_ahead_seconds"`
	StreamState          string     `json:"stream_state"`
	LastReadAt           *time.Time `json:"last_read_at"`
	StreamSampledAt      *time.Time `json:"stream_sampled_at"`
}

func playerStreamState(s collector.StreamStats, now time.Time) string {
	if !s.Active {
		return "idle"
	}
	if !s.PendingSince.IsZero() && now.Sub(s.PendingSince) >= playerStallThreshold {
		return "stalled"
	}
	lastActivity := s.LastRequestAt
	if lastActivity.IsZero() {
		lastActivity = s.OpenedAt
	}
	if s.PendingSince.IsZero() && now.Sub(lastActivity) >= playerIdleThreshold {
		return "idle"
	}
	if s.LastReadAt == nil {
		return "warmup"
	}
	return "streaming"
}

// StatsHandler uses exactly the same authorization as Add, including loopback
// restriction when the token is unset. It never refreshes validation leases.
func (h *LibraryHandler) StatsHandler(source PlayerStatsSource) http.HandlerFunc {
	return h.statsHandler(source, time.Now)
}

func (h *LibraryHandler) statsHandler(source PlayerStatsSource, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !h.authorize(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		hashes := r.URL.Query()["hash"]
		if len(hashes) != 1 || len(hashes[0]) != 40 {
			writeJSONError(w, http.StatusBadRequest, "invalid_infohash")
			return
		}
		hash := strings.ToLower(hashes[0])
		if _, err := hex.DecodeString(hash); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_infohash")
			return
		}
		torrent, stream, found := source.PlayerStats(hash)
		if !found {
			writeJSONError(w, http.StatusNotFound, "torrent_not_found")
			return
		}
		observedAt := now()
		if stream.SampledAt != nil {
			// Age the observation only to its sample time. An old pending-read
			// sample must not turn into a false stall after that read completed.
			observedAt = *stream.SampledAt
		}
		resp := playerStatsResponse{
			Hash: hash, PlayerTorrentStats: torrent,
			ReadOffset: stream.ReadOffset, BufferedAheadBytes: stream.BufferedAheadBytes,
			StreamState: playerStreamState(stream, observedAt), LastReadAt: stream.LastReadAt,
			StreamSampledAt: stream.SampledAt,
		}
		if stream.BufferedAheadBytes != nil && stream.BitrateBytesPerSec != nil {
			rate := *stream.BitrateBytesPerSec
			if rate > 0 && !math.IsInf(rate, 0) && !math.IsNaN(rate) {
				seconds := float64(*stream.BufferedAheadBytes) / rate
				resp.BufferedAheadSeconds = &seconds
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
