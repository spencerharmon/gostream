package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"gostream/internal/config"
	"gostream/internal/persist"
	"gostream/internal/warmup"
)

// PrestageProgress is the public progress record for an in-flight or
// completed background prestage read.
type PrestageProgress struct {
	Hash       string     `json:"hash"`
	BytesRead  int64      `json:"bytes_read"`
	TotalBytes int64      `json:"total_bytes"`
	Percent    float64    `json:"percent"`
	Finished   bool       `json:"finished"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Error      string     `json:"error,omitempty"`
}

// prestageState wraps PrestageProgress with a cancel channel for the
// background reader and the live byte counter.
type prestageState struct {
	mu        sync.Mutex
	progress  PrestageProgress
	cancel    context.CancelFunc
}

// VaultHandler exposes the prestage/unprestage HTTP endpoints.
type VaultHandler struct {
	cache        *warmup.DiskWarmupCache
	cfg          *config.Config
	progressMu   sync.Mutex
	progressMap  map[string]*prestageState // keyed by hash
	logger       *log.Logger
}

// NewVaultHandler constructs a handler. cache may be nil if disk warmup is
// disabled, in which case all endpoints return 503.
func NewVaultHandler(cache *warmup.DiskWarmupCache, cfg *config.Config) *VaultHandler {
	return &VaultHandler{
		cache:       cache,
		cfg:         cfg,
		progressMap: map[string]*prestageState{},
		logger:      log.New(os.Stdout, "[Vault] ", log.LstdFlags),
	}
}

func (h *VaultHandler) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *VaultHandler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, map[string]string{"error": msg})
}

type prestageReq struct {
	StubPath string `json:"stub_path"`
	Priority int    `json:"priority"`
}

// resolveStub reads a stub, validates it, extracts hash and FUSE path.
func (h *VaultHandler) resolveStub(stubPath string) (*persist.Stub, string, string, error) {
	if stubPath == "" {
		return nil, "", "", errors.New("stub_path required")
	}
	s, err := persist.ReadStub(stubPath)
	if err != nil {
		return nil, "", "", err
	}
	hash := persist.ExtractHashFromMagnet(s.Magnet)
	if hash == "" {
		return nil, "", "", errors.New("stub has no magnet hash")
	}
	fusePath, err := persist.RealToFuse(stubPath, h.cfg.PhysicalSourcePath, h.cfg.FuseMountPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("compute fuse path: %w", err)
	}
	return s, hash, fusePath, nil
}

// Prestage handles POST /api/library/prestage.
func (h *VaultHandler) Prestage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.cache == nil {
		h.writeError(w, http.StatusServiceUnavailable, "disk warmup disabled")
		return
	}
	var req prestageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.StubPath == "" {
		h.writeError(w, http.StatusBadRequest, "stub_path required")
		return
	}
	if req.Priority < 0 || req.Priority > 100 {
		h.writeError(w, http.StatusBadRequest, "priority must be 0..100")
		return
	}
	if req.Priority == 0 {
		req.Priority = 50
	}

	stub, hash, fusePath, err := h.resolveStub(req.StubPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.writeError(w, http.StatusNotFound, err.Error())
			return
		}
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.cache.MarkPersistent(hash, req.Priority)

	stub.Persist = true
	stub.PersistPriority = req.Priority
	if err := persist.WriteStubAtomic(req.StubPath, stub); err != nil {
		h.logger.Printf("rewrite stub failed for %s: %v", req.StubPath, err)
		h.writeError(w, http.StatusInternalServerError, "rewrite stub: "+err.Error())
		return
	}

	// Start (or restart) background read.
	h.startPrestageRead(hash, fusePath, stub.Size)

	h.writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"hash":        hash,
		"total_bytes": stub.Size,
	})
}

// startPrestageRead launches the throttled background read. If a prior
// read for the same hash is in-flight it is cancelled first.
func (h *VaultHandler) startPrestageRead(hash, fusePath string, total int64) {
	ctx, cancel := context.WithCancel(context.Background())
	st := &prestageState{
		progress: PrestageProgress{
			Hash:       hash,
			TotalBytes: total,
			StartedAt:  time.Now().UTC(),
		},
		cancel: cancel,
	}
	h.progressMu.Lock()
	if old, ok := h.progressMap[hash]; ok && old.cancel != nil {
		old.cancel()
	}
	h.progressMap[hash] = st
	h.progressMu.Unlock()

	go h.runPrestageRead(ctx, st, fusePath)
}

func (h *VaultHandler) runPrestageRead(ctx context.Context, st *prestageState, fusePath string) {
	const chunk = 4 * 1024 * 1024
	const throttle = 50 * time.Millisecond

	finish := func(errMsg string) {
		st.mu.Lock()
		now := time.Now().UTC()
		st.progress.FinishedAt = &now
		st.progress.Finished = true
		if errMsg != "" {
			st.progress.Error = errMsg
		}
		if st.progress.TotalBytes > 0 {
			st.progress.Percent = float64(st.progress.BytesRead) / float64(st.progress.TotalBytes) * 100
		}
		st.mu.Unlock()
	}

	f, err := os.Open(fusePath)
	if err != nil {
		h.logger.Printf("prestage open %s: %v", fusePath, err)
		finish("open: " + err.Error())
		return
	}
	defer f.Close()

	buf := make([]byte, chunk)
	for {
		select {
		case <-ctx.Done():
			h.logger.Printf("prestage cancelled for %s", st.progress.Hash)
			finish("cancelled")
			return
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			st.mu.Lock()
			st.progress.BytesRead += int64(n)
			if st.progress.TotalBytes > 0 {
				st.progress.Percent = float64(st.progress.BytesRead) / float64(st.progress.TotalBytes) * 100
			}
			st.mu.Unlock()
		}
		if err == io.EOF {
			finish("")
			return
		}
		if err != nil {
			h.logger.Printf("prestage read %s: %v", fusePath, err)
			finish("read: " + err.Error())
			return
		}
		time.Sleep(throttle)
	}
}

// Status handles GET /api/library/prestage/status?stub_path=...
func (h *VaultHandler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stubPath := r.URL.Query().Get("stub_path")
	if stubPath == "" {
		h.writeError(w, http.StatusBadRequest, "stub_path required")
		return
	}
	stub, err := persist.ReadStub(stubPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.writeError(w, http.StatusNotFound, "stub not found")
			return
		}
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash := persist.ExtractHashFromMagnet(stub.Magnet)
	if hash == "" {
		h.writeError(w, http.StatusBadRequest, "stub has no magnet hash")
		return
	}
	h.progressMu.Lock()
	st, ok := h.progressMap[hash]
	h.progressMu.Unlock()
	if !ok {
		h.writeError(w, http.StatusNotFound, "no prestage record for hash")
		return
	}
	st.mu.Lock()
	p := st.progress
	st.mu.Unlock()
	h.writeJSON(w, http.StatusOK, p)
}

type unprestageReq struct {
	StubPath string `json:"stub_path"`
}

// Unprestage handles POST /api/library/unprestage.
func (h *VaultHandler) Unprestage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.cache == nil {
		h.writeError(w, http.StatusServiceUnavailable, "disk warmup disabled")
		return
	}
	var req unprestageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	stub, err := persist.ReadStub(req.StubPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.writeError(w, http.StatusNotFound, "stub not found")
			return
		}
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash := persist.ExtractHashFromMagnet(stub.Magnet)
	if hash == "" {
		h.writeError(w, http.StatusBadRequest, "stub has no magnet hash")
		return
	}
	h.cache.UnmarkPersistent(hash)
	stub.Persist = false
	stub.PersistPriority = 0
	if err := persist.WriteStubAtomic(req.StubPath, stub); err != nil {
		h.writeError(w, http.StatusInternalServerError, "rewrite stub: "+err.Error())
		return
	}
	h.progressMu.Lock()
	if st, ok := h.progressMap[hash]; ok && st.cancel != nil {
		st.cancel()
	}
	h.progressMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
