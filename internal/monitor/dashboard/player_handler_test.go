package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gostream/internal/gostorm/torr/state"
	"gostream/internal/monitor/collector"
)

const playerTestHash = "abcdef0123456789abcdef0123456789abcdef01"

func playerPtr[T any](v T) *T { return &v }

type fakePlayerEngine struct {
	torrent state.PlayerTorrentStats
	stream  collector.StreamStats
	found   bool
	calls   int
	hash    string
}

func (f *fakePlayerEngine) PlayerStats(hash string) (state.PlayerTorrentStats, collector.StreamStats, bool) {
	f.calls++
	f.hash = hash
	return f.torrent, f.stream, f.found
}

func TestPlayerStatsStates(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Second)
	for _, tc := range []struct {
		name                     string
		active                   bool
		opened, request, pending time.Time
		read                     *time.Time
		want                     string
	}{
		{"idle no stream", false, time.Time{}, time.Time{}, time.Time{}, nil, "idle"},
		{"warmup before first read", true, recent, time.Time{}, time.Time{}, nil, "warmup"},
		{"warmup pending under threshold", true, recent, recent, now.Add(-10*time.Second + time.Nanosecond), nil, "warmup"},
		{"stalled initial read at threshold", true, recent, recent, now.Add(-10 * time.Second), nil, "stalled"},
		{"streaming even with zero download", true, recent, recent, time.Time{}, &recent, "streaming"},
		{"stalled after success", true, recent, recent, now.Add(-11 * time.Second), &recent, "stalled"},
		{"pending prevents idle", true, recent, now.Add(-time.Minute), now.Add(-time.Minute), &recent, "stalled"},
		{"not idle just below threshold", true, recent, now.Add(-30*time.Second + time.Nanosecond), time.Time{}, &recent, "streaming"},
		{"idle at threshold", true, recent, now.Add(-30 * time.Second), time.Time{}, &recent, "idle"},
		{"unused open becomes idle", true, now.Add(-30 * time.Second), time.Time{}, time.Time{}, nil, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakePlayerEngine{found: true,
				torrent: state.PlayerTorrentStats{Peers: playerPtr(7), Seeders: playerPtr(3), DownloadRateBytesPerSec: playerPtr(0.0), BytesCompleted: playerPtr(int64(8192)), Size: playerPtr(int64(99999)), TorrentSampledAt: &now},
				stream:  collector.StreamStats{Active: tc.active, StreamActivity: collector.StreamActivity{OpenedAt: tc.opened, LastRequestAt: tc.request, PendingSince: tc.pending, LastReadAt: tc.read, ReadOffset: playerPtr(int64(512))}, BufferedAheadBytes: playerPtr(int64(4096)), BitrateBytesPerSec: playerPtr(1024.0), SampledAt: &now},
			}
			h := NewLibraryHandler(LibraryConfig{AuthToken: "secret"}, nil)
			req := httptest.NewRequest(http.MethodGet, "/api/library/stats?hash="+strings.ToUpper(playerTestHash), nil)
			req.Header.Set("X-Gostream-Token", "secret")
			rec := httptest.NewRecorder()
			// Polling much later must not extrapolate a stale sample into a stall.
			h.statsHandler(engine, func() time.Time { return now.Add(time.Minute) })(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			var got playerStatsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.StreamState != tc.want {
				t.Fatalf("state=%s want=%s", got.StreamState, tc.want)
			}
			if got.Hash != playerTestHash || engine.hash != playerTestHash || engine.calls != 1 {
				t.Fatalf("lookup/normalization: %+v %+v", got, engine)
			}
			if *got.Peers != 7 || *got.Seeders != 3 || *got.DownloadRateBytesPerSec != 0 || *got.BytesCompleted != 8192 || *got.Size != 99999 || *got.ReadOffset != 512 || *got.BufferedAheadBytes != 4096 || *got.BufferedAheadSeconds != 4 {
				t.Fatalf("values not preserved: %s", rec.Body)
			}
			if (got.LastReadAt == nil) != (tc.read == nil) || got.LastReadAt != nil && !got.LastReadAt.Equal(*tc.read) {
				t.Fatalf("last read=%v", got.LastReadAt)
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("headers=%v", rec.Header())
			}
		})
	}
}

func TestPlayerStatsNulls(t *testing.T) {
	for _, rate := range []*float64{nil, playerPtr(0.0), playerPtr(-1.0), playerPtr(math.Inf(1)), playerPtr(math.NaN())} {
		engine := &fakePlayerEngine{found: true, stream: collector.StreamStats{BitrateBytesPerSec: rate}}
		req := httptest.NewRequest(http.MethodGet, "/api/library/stats?hash="+playerTestHash, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		NewLibraryHandler(LibraryConfig{}, nil).StatsHandler(engine)(rec, req)
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"peers", "seeders", "download_rate_bytes_per_sec", "bytes_completed", "size", "read_offset", "buffered_ahead_bytes", "buffered_ahead_seconds", "last_read_at", "torrent_sampled_at", "stream_sampled_at"} {
			if value, exists := got[key]; !exists || value != nil {
				t.Errorf("%s must be explicit null: %s", key, rec.Body)
			}
		}
	}
}

func TestPlayerStatsAuthAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, method, query, configuredToken, token, remote string
		found                                               bool
		status, calls                                       int
	}{
		{"unknown", "GET", "hash=" + playerTestHash, "secret", "secret", "192.0.2.1:1", false, 404, 1},
		{"missing token", "GET", "hash=" + playerTestHash, "secret", "", "127.0.0.1:1", true, 401, 0},
		{"wrong token", "GET", "hash=" + playerTestHash, "secret", "wrong", "127.0.0.1:1", true, 401, 0},
		{"loopback ipv4", "GET", "hash=" + playerTestHash, "", "", "127.0.0.1:1", true, 200, 1},
		{"loopback ipv6", "GET", "hash=" + playerTestHash, "", "", "[::1]:1", true, 200, 1},
		{"remote without configured token", "GET", "hash=" + playerTestHash, "", "", "192.0.2.1:1", true, 403, 0},
		{"post", "POST", "hash=" + playerTestHash, "secret", "secret", "192.0.2.1:1", true, 405, 0},
		{"missing hash", "GET", "", "secret", "secret", "192.0.2.1:1", true, 400, 0},
		{"short hash", "GET", "hash=abc", "secret", "secret", "192.0.2.1:1", true, 400, 0},
		{"non hex", "GET", "hash=" + strings.Repeat("z", 40), "secret", "secret", "192.0.2.1:1", true, 400, 0},
		{"duplicate", "GET", "hash=" + playerTestHash + "&hash=" + playerTestHash, "secret", "secret", "192.0.2.1:1", true, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakePlayerEngine{found: tc.found}
			req := httptest.NewRequest(tc.method, "/api/library/stats?"+tc.query, nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Gostream-Token", tc.token)
			req.Header.Set("X-Forwarded-For", "127.0.0.1") // never trust proxy headers for auth
			rec := httptest.NewRecorder()
			NewLibraryHandler(LibraryConfig{AuthToken: tc.configuredToken}, nil).StatsHandler(engine)(rec, req)
			if rec.Code != tc.status || engine.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, engine.calls, rec.Body)
			}
			if tc.status == 405 && rec.Header().Get("Allow") != "GET" {
				t.Fatal("missing Allow header")
			}
		})
	}
}
