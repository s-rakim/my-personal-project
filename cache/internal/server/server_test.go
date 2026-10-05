package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/s-rakim/my-personal-project/cache/internal/catalog"
	"github.com/s-rakim/my-personal-project/cache/internal/store"
)

func newServer(t *testing.T, fetchOnMiss bool, budget int64) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	c, err := catalog.Open(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	srv, err := New(Options{
		Store: s, Catalog: c, BudgetBytes: budget, FetchOnMiss: fetchOnMiss,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client: &http.Client{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// The property that stops a cold cache being worse than no cache. Without
// coalescing, several devices asking for the same uncached file each open their
// own upstream transfer and compete for the bandwidth the cache exists to save.
func TestConcurrentMissesCauseOneUpstreamFetch(t *testing.T) {
	var upstreamCalls int64
	release := make(chan struct{})
	body := strings.Repeat("payload", 2000)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		<-release // hold every request open so they genuinely overlap
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	srv := newServer(t, true, 0)
	srv.opts.Client = upstream.Client()

	const callers = 20
	var wg sync.WaitGroup
	results := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := srv.fetchCoalesced(context.Background(), "shared", upstream.URL, meta{})
			results[idx] = err
		}(i)
	}

	// Give the goroutines time to pile up on the same name before releasing.
	for i := 0; i < 100; i++ {
		if atomic.LoadInt64(&upstreamCalls) > 0 {
			break
		}
	}
	close(release)
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&upstreamCalls); got != 1 {
		t.Errorf("%d callers caused %d upstream fetches, want 1", callers, got)
	}

	st := srv.opts.Catalog.Stats(srv.opts.Store)
	if st.BytesFetched != int64(len(body)) {
		t.Errorf("counted %d fetched bytes, want %d; coalesced callers must not "+
			"each be billed for the transfer", st.BytesFetched, len(body))
	}
}

func TestHitIsServedWithoutTouchingUpstream(t *testing.T) {
	var calls int64
	body := "cached content"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	srv := newServer(t, true, 0)
	srv.opts.Client = upstream.Client()

	if _, err := srv.Prestage(context.Background(), "thing", upstream.URL, 0, false); err != nil {
		t.Fatalf("Prestage: %v", err)
	}

	handler := srv.Handler()
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/thing", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
		if rec.Body.String() != body {
			t.Errorf("request %d: body mismatch", i)
		}
		if rec.Header().Get("X-Cache") != "HIT" {
			t.Errorf("request %d: expected a cache hit", i)
		}
	}
	// One fetch, five deliveries: the whole point.
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("upstream was called %d times, want 1", got)
	}
}

func TestMissWithoutFetchOnMissExplainsItself(t *testing.T) {
	srv := newServer(t, false, 0)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/absent", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	// A bare 404 would leave the operator guessing whether the cache is broken.
	if !strings.Contains(rec.Body.String(), "pre-stage") {
		t.Errorf("response should say how to get the content, got %s", rec.Body.String())
	}
}

func TestMatchingETagReturnsNotModified(t *testing.T) {
	body := "etag content"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	srv := newServer(t, true, 0)
	srv.opts.Client = upstream.Client()
	entry, err := srv.Prestage(context.Background(), "thing", upstream.URL, 0, false)
	if err != nil {
		t.Fatalf("Prestage: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/c/thing", nil)
	req.Header.Set("If-None-Match", `"`+string(entry.Digest)+`"`)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Content is addressed by the hash of its own bytes, so a matching ETag can
	// never be stale and the body need not be sent again.
	if rec.Code != http.StatusNotModified {
		t.Errorf("status %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 should carry no body, got %d bytes", rec.Body.Len())
	}
}

func TestPinnedPrestageSurvivesEviction(t *testing.T) {
	big := strings.Repeat("x", 8000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	// A budget far below what we are about to store, so eviction must run.
	srv := newServer(t, true, 1000)
	srv.opts.Client = upstream.Client()

	if _, err := srv.Prestage(context.Background(), "keep", upstream.URL, 10, true); err != nil {
		t.Fatalf("Prestage: %v", err)
	}
	if _, err := srv.Prestage(context.Background(), "spill", upstream.URL+"/other", 0, false); err != nil {
		t.Fatalf("Prestage: %v", err)
	}

	if _, ok := srv.opts.Catalog.Peek("keep"); !ok {
		t.Error("pinned content was evicted to make room")
	}
}

// Pre-staging with a priority must also survive, for the same reason: metadata
// applied after the content lands is applied after eviction has already run.
func TestPrestagedPriorityIsSetBeforeEvictionRuns(t *testing.T) {
	big := strings.Repeat("y", 8000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	srv := newServer(t, true, 1000)
	srv.opts.Client = upstream.Client()

	entry, err := srv.Prestage(context.Background(), "high", upstream.URL, 9, false)
	if err != nil {
		t.Fatalf("Prestage: %v", err)
	}
	if entry.Priority != 9 {
		t.Errorf("returned entry has priority %d, want 9", entry.Priority)
	}
	if entry.Name != "high" {
		t.Errorf("returned entry is named %q; metadata was applied to the wrong record",
			entry.Name)
	}
}

func TestUnfetchableURLIsRejected(t *testing.T) {
	srv := newServer(t, true, 0)
	if _, err := srv.Prestage(context.Background(), "x", "not a url", 0, false); err == nil {
		t.Error("expected a malformed URL to be rejected")
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
