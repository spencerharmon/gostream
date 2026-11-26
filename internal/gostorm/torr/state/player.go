package state

import "time"

// PlayerTorrentStats is an immutable sample from the existing engine stats tick.
// Nil means not sampled/computable yet, not a measured zero.
type PlayerTorrentStats struct {
	Peers                   *int       `json:"peers"`
	Seeders                 *int       `json:"seeders"`
	DownloadRateBytesPerSec *float64   `json:"download_rate_bytes_per_sec"`
	BytesCompleted          *int64     `json:"bytes_completed"`
	Size                    *int64     `json:"size"`
	TorrentSampledAt        *time.Time `json:"torrent_sampled_at"`
}
