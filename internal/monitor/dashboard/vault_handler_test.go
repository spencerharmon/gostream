package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gostream/internal/config"
	"gostream/internal/warmup"
)

func writeStub(t *testing.T, dir, name, hash string, size int64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := map[string]interface{}{
		"url":    "http://127.0.0.1:8090/stream?link=" + hash + "&index=0&play",
		"size":   size,
		"magnet": "magnet:?xt=urn:btih:" + hash,
		"imdb":   "tt1234567",
	}
	b, _ := json.Marshal(body)
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newHandler(t *testing.T) (*VaultHandler, string, string) {
	t.Helper()
	realRoot := t.TempDir()
	fuseRoot := t.TempDir()
	cache := warmup.NewDiskWarmupCache(t.TempDir())
	cfg := &config.Config{
		PhysicalSourcePath: realRoot,
		FuseMountPath:      fuseRoot,
	}
	return NewVaultHandler(cache, cfg), realRoot, fuseRoot
}

func TestPrestageHappyPath(t *testing.T) {
	h, realRoot, fuseRoot := newHandler(t)

	hash := "aabbccddeeff00112233445566778899aabbccdd"
	stubPath := writeStub(t, realRoot, "movie.mkv", hash, 12345)

	// Provide a real FUSE-side file so background reader can open it
	// (it will read EOF immediately).
	if err := os.WriteFile(filepath.Join(fuseRoot, "movie.mkv"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"stub_path":"` + stubPath + `","priority":70}`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/prestage", body)
	w := httptest.NewRecorder()
	h.Prestage(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["hash"] != hash {
		t.Fatalf("hash mismatch: %v", resp["hash"])
	}
	if !h.cache.IsPersistent(hash) {
		t.Fatal("hash should be marked persistent")
	}
	// Stub rewritten with persist=true.
	raw, _ := os.ReadFile(stubPath)
	if !strings.Contains(string(raw), `"persist": true`) {
		t.Fatalf("stub missing persist=true: %s", raw)
	}
	if !strings.Contains(string(raw), `"persist_priority": 70`) {
		t.Fatalf("stub missing persist_priority=70: %s", raw)
	}
}

func TestPrestageMissingStub(t *testing.T) {
	h, _, _ := newHandler(t)
	body := bytes.NewBufferString(`{"stub_path":"/nonexistent/path.mkv"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/prestage", body)
	w := httptest.NewRecorder()
	h.Prestage(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPrestageBadBody(t *testing.T) {
	h, _, _ := newHandler(t)
	body := bytes.NewBufferString(`not json`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/prestage", body)
	w := httptest.NewRecorder()
	h.Prestage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestStatusBeforePrestage(t *testing.T) {
	h, realRoot, _ := newHandler(t)
	hash := "1111111111111111111111111111111111111111"
	stubPath := writeStub(t, realRoot, "x.mkv", hash, 1000)

	req := httptest.NewRequest(http.MethodGet, "/api/library/prestage/status?stub_path="+stubPath, nil)
	w := httptest.NewRecorder()
	h.Status(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 before prestage, got %d", w.Code)
	}
}

func TestStatusAfterPrestage(t *testing.T) {
	h, realRoot, fuseRoot := newHandler(t)
	hash := "2222222222222222222222222222222222222222"
	stubPath := writeStub(t, realRoot, "y.mkv", hash, 5)
	if err := os.WriteFile(filepath.Join(fuseRoot, "y.mkv"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Kick off.
	body := bytes.NewBufferString(`{"stub_path":"` + stubPath + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/prestage", body)
	w := httptest.NewRecorder()
	h.Prestage(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("prestage failed: %d %s", w.Code, w.Body.String())
	}

	// Give background reader a moment to finish (5-byte file → 1 read + EOF).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req2 := httptest.NewRequest(http.MethodGet, "/api/library/prestage/status?stub_path="+stubPath, nil)
		w2 := httptest.NewRecorder()
		h.Status(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("expected 200 status, got %d", w2.Code)
		}
		var p PrestageProgress
		if err := json.Unmarshal(w2.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Hash != hash {
			t.Fatalf("hash mismatch: %s", p.Hash)
		}
		if p.Finished {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("prestage never finished")
}

func TestUnprestage(t *testing.T) {
	h, realRoot, fuseRoot := newHandler(t)
	hash := "3333333333333333333333333333333333333333"
	stubPath := writeStub(t, realRoot, "z.mkv", hash, 1000)
	if err := os.WriteFile(filepath.Join(fuseRoot, "z.mkv"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	// Mark first.
	body := bytes.NewBufferString(`{"stub_path":"` + stubPath + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/library/prestage", body)
	w := httptest.NewRecorder()
	h.Prestage(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("prestage: %d", w.Code)
	}
	if !h.cache.IsPersistent(hash) {
		t.Fatal("expected persistent")
	}

	// Unmark.
	body2 := bytes.NewBufferString(`{"stub_path":"` + stubPath + `"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/library/unprestage", body2)
	w2 := httptest.NewRecorder()
	h.Unprestage(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", w2.Code, w2.Body.String())
	}
	if h.cache.IsPersistent(hash) {
		t.Fatal("expected non-persistent after unprestage")
	}
	raw, _ := os.ReadFile(stubPath)
	if strings.Contains(string(raw), `"persist": true`) {
		t.Fatalf("stub should not have persist=true after unprestage: %s", raw)
	}
}
