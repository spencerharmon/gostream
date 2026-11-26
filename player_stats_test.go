package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gostream/internal/config"
	"gostream/internal/gostorm/settings"
	"gostream/internal/monitor/collector"
	"gostream/internal/monitor/dashboard"
)

func TestPlayerStatsRoute(t *testing.T) {
	mux := http.NewServeMux()
	registerPlayerStats(mux, dashboard.NewLibraryHandler(dashboard.LibraryConfig{AuthToken: "secret"}, nil), &collector.Collector{})
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {"secret", 404}} {
		r := httptest.NewRequest("GET", "/api/library/stats?hash=abcdef0123456789abcdef0123456789abcdef01", nil)
		r.Header.Set("X-Gostream-Token", tc.token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
}

func TestPlayerBufferSampling(t *testing.T) {
	oldConfig, oldCache, oldSettings := gc(), raCache, settings.BTsets
	settings.BTsets = &settings.BTSets{}
	globalConfig.Store(&config.Config{ReadAheadBase: 4, ReadAheadBudget: 1024})
	raCache = newReadAheadCache()
	t.Cleanup(func() { globalConfig.Store(oldConfig); raCache = oldCache; settings.BTsets = oldSettings })
	path := "/media/example.mkv"
	raCache.Put(path, 0, 3, []byte("abcd"))
	raCache.Put(path, 4, 7, []byte("efgh"))
	raCache.Put(path, 12, 15, []byte("mnop")) // hole [8,12) must not count
	for _, tc := range []struct{ off, size, end int64 }{{2, 16, 8}, {8, 16, 8}, {12, 16, 16}, {0, 6, 6}, {16, 16, 16}} {
		if got := raCache.contiguousEnd(path, tc.off, tc.size); got != tc.end {
			t.Fatalf("off=%d size=%d end=%d want=%d", tc.off, tc.size, got, tc.end)
		}
	}
	now := time.Now()
	h := &MkvHandle{path: path, hash: "abc", size: 16}
	h.playerActivity.OpenedAt = now
	seq := h.playerActivity.BeginRead(0, now)
	h.playerActivity.EndRead(seq, 2, now)
	activeHandles.Store(h, true)
	t.Cleanup(func() { activeHandles.Delete(h) })
	s := samplePlayerStreams()["abc"]
	if !s.Active || s.ReadOffset == nil || *s.ReadOffset != 2 || s.BufferedAheadBytes == nil || *s.BufferedAheadBytes != 6 || s.BitrateBytesPerSec != nil {
		t.Fatalf("sample: %+v", s)
	}
	// A newer handle for another file of the same torrent wins; never sum buffers.
	other := &MkvHandle{path: "/media/other.mkv", hash: "abc", size: 20}
	other.playerActivity.OpenedAt = now.Add(time.Second)
	activeHandles.Store(other, true)
	t.Cleanup(func() { activeHandles.Delete(other) })
	if s := samplePlayerStreams()["abc"]; s.ReadOffset != nil || s.BufferedAheadBytes != nil {
		t.Fatal("used wrong file's cursor/buffer")
	}
	activeHandles.Delete(other)
	// Exercise the actual FUSE Read bookkeeping on a cache hit, without mounting FUSE.
	h.lastGlobalUpdate = now // avoid unrelated cleanup manager
	raCache.Put(path, 8, 11, []byte("ijkl"))
	result, errno := h.Read(context.Background(), make([]byte, 2), 2)
	if errno != 0 || result.Size() != 2 {
		t.Fatalf("Read: %v %v", result, errno)
	}
	if h.playerActivity.LastReadAt == nil || *h.playerActivity.ReadOffset != 4 || !h.playerActivity.PendingSince.IsZero() {
		t.Fatalf("read activity: %+v", h.playerActivity)
	}
	// The no-slot/probe Release path returns early; it must still unregister.
	activePumps.Store(path, &NativePumpState{})
	t.Cleanup(func() { activePumps.Delete(path) })
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatal(errno)
	}
	if _, found := samplePlayerStreams()["abc"]; found {
		t.Fatal("released handle reported as active")
	}
}
