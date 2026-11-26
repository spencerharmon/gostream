package torr

import (
	"encoding/hex"

	"github.com/anacrolix/torrent/metainfo"
	"gostream/internal/gostorm/torr/state"
)

// PlayerStats looks up a loaded torrent without waking it, reading the database,
// extending its expiry, or enumerating peers/files/pieces. Polling is O(1).
// Persisted but unloaded torrents are not live engine state and return false.
func PlayerStats(hashHex string) (state.PlayerTorrentStats, bool) {
	var hash metainfo.Hash
	data, err := hex.DecodeString(hashHex)
	if err != nil || len(data) != len(hash) {
		return state.PlayerTorrentStats{}, false
	}
	copy(hash[:], data)
	btsMu.RLock()
	defer btsMu.RUnlock()
	if bts == nil {
		return state.PlayerTorrentStats{}, false
	}
	t := bts.GetTorrent(hash)
	if t == nil {
		return state.PlayerTorrentStats{}, false
	}
	if sample := t.playerStats.Load(); sample != nil {
		return *sample, true
	}
	return state.PlayerTorrentStats{}, true
}
