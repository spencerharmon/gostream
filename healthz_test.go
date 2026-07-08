package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gostream/internal/metadb"
)

// TestHealthzAlwaysOK verifies the liveness probe returns 200 regardless of
// FUSE/DB state — it must reflect only "process up + HTTP serving".
func TestHealthzAlwaysOK(t *testing.T) {
	// Force the least-ready state possible; liveness must still be 200.
	defer restoreProbeState(atomic.LoadInt32(&fuseMounted), globalConfig.EnableStateDB, stateDB)
	atomic.StoreInt32(&fuseMounted, 0)
	globalConfig.EnableStateDB = true
	stateDB = nil

	rec := httptest.NewRecorder()
	healthzHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("healthz body = %q, want %q", got, "ok")
	}
}

func TestReadyz(t *testing.T) {
	defer restoreProbeState(atomic.LoadInt32(&fuseMounted), globalConfig.EnableStateDB, stateDB)

	// A real, open state DB so the "enabled + open" case is exercised end-to-end.
	openDB, err := metadb.New(t.TempDir()+"/gostream.db", nil)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer openDB.Close()

	cases := []struct {
		name       string
		mounted    int32
		enableDB   bool
		db         *metadb.DB
		wantStatus int
	}{
		{"mounted+db-disabled", 1, false, nil, http.StatusOK},
		{"mounted+db-enabled-open", 1, true, openDB, http.StatusOK},
		{"not-mounted", 0, false, nil, http.StatusServiceUnavailable},
		{"mounted+db-enabled-nil", 1, true, nil, http.StatusServiceUnavailable},
		{"not-mounted+db-enabled-nil", 0, true, nil, http.StatusServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atomic.StoreInt32(&fuseMounted, tc.mounted)
			globalConfig.EnableStateDB = tc.enableDB
			stateDB = tc.db

			rec := httptest.NewRecorder()
			readyzHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("readyz status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK && strings.TrimSpace(rec.Body.String()) != "ok" {
				t.Fatalf("readyz ok body = %q, want %q", rec.Body.String(), "ok")
			}
			if tc.wantStatus == http.StatusServiceUnavailable && !strings.Contains(rec.Body.String(), "not ready") {
				t.Fatalf("readyz not-ready body = %q, want it to contain %q", rec.Body.String(), "not ready")
			}
		})
	}
}

func restoreProbeState(mounted int32, enableDB bool, db *metadb.DB) {
	atomic.StoreInt32(&fuseMounted, mounted)
	globalConfig.EnableStateDB = enableDB
	stateDB = db
}
