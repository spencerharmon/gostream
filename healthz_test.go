package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gostream/internal/metadb"
)

// TestHealthzHandlerAlwaysReturns200 verifies /healthz (liveness) never
// depends on process state: it must answer 200 unconditionally, since it
// only signals "the process is up and the HTTP mux is answering".
func TestHealthzHandlerAlwaysReturns200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Fatalf("unexpected body: %q", got)
	}
}

// TestReadyzHandler covers the /readyz (readiness) truth table: FUSE-mounted
// flag AND (state DB disabled OR state DB open). Never touches an
// external upstream (Prowlarr/TMDB/Plex), so none of those being down can
// affect the outcome.
func TestReadyzHandler(t *testing.T) {
	// main package globals are shared process-wide state; save/restore so
	// this test can't leak into others run in the same binary.
	origFuseReady := fuseMountReady.Load()
	origEnableStateDB := globalConfig.EnableStateDB
	origStateDB := stateDB
	defer func() {
		fuseMountReady.Store(origFuseReady)
		globalConfig.EnableStateDB = origEnableStateDB
		stateDB = origStateDB
	}()

	// Zero-value *metadb.DB: enough to be a non-nil "open" handle for this
	// handler's purposes — it only ever checks stateDB != nil, never calls
	// through it, so no real sqlite file is needed.
	openDB := &metadb.DB{}

	cases := []struct {
		name          string
		fuseReady     bool
		enableStateDB bool
		db            *metadb.DB
		wantStatus    int
	}{
		{"not ready: nothing initialized yet", false, false, nil, http.StatusServiceUnavailable},
		{"ready: fuse mounted, state db feature disabled", true, false, nil, http.StatusOK},
		{"not ready: fuse mounted, state db enabled but failed to open", true, true, nil, http.StatusServiceUnavailable},
		{"ready: fuse mounted, state db enabled and open", true, true, openDB, http.StatusOK},
		{"not ready: state db open but fuse not mounted yet", false, true, openDB, http.StatusServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fuseMountReady.Store(tc.fuseReady)
			globalConfig.EnableStateDB = tc.enableStateDB
			stateDB = tc.db

			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			readyzHandler(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d (body=%q)", tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}
