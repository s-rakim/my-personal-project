// Package server exposes the cache over HTTP.
//
// Two things it deliberately does not do.
//
// It is not a transparent HTTPS proxy. Caching encrypted traffic generically
// requires terminating TLS with a certificate authority installed on every
// client, which means decrypting all of that device's traffic. That is a large
// thing to build into a box in order to save some bandwidth, and it is not built
// here. Content is cached because something asked for it to be cached.
//
// It does not fetch on demand by default either. On a slow or metered link, a
// cache miss that silently fetches is just a slow request with extra steps. The
// default is to record the miss and let the deferred-transfer queue collect it
// when a link worth using appears.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/cache/internal/catalog"
	"github.com/s-rakim/my-personal-project/cache/internal/store"
)

// Options configures the server.
type Options struct {
	Store   *store.Store
	Catalog *catalog.Catalog
	Log     *slog.Logger

	// BudgetBytes is the disk allowance. Eviction runs when it is exceeded.
	BudgetBytes int64

	// FetchOnMiss makes a miss fetch upstream immediately rather than deferring.
	// Reasonable behind a fast flat-rate link; wasteful behind a slow or metered
	// one, which is the case this exists for.
	FetchOnMiss bool

	// Client fetches upstream content. Its timeout should suit the slowest link
	// you expect, not the fastest.
	Client *http.Client
}

// Server serves cached content.
type Server struct {
	opts Options

	// inflight coalesces concurrent misses for the same name.
	mu       sync.Mutex
	inflight map[string]*fetch
}

// fetch is one upstream request that others can wait on.
type fetch struct {
	done  chan struct{}
	entry catalog.Entry
	err   error
}

// meta is the catalog metadata an entry must carry from the moment it is
// stored. It cannot be applied afterwards: eviction runs as soon as the content
// lands, so an entry that is not yet pinned can be discarded before anything
// gets the chance to pin it.
type meta struct {
	priority int
	pinned   bool
}

// New builds a server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil || opts.Catalog == nil {
		return nil, fmt.Errorf("server: store and catalog are required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Minute}
	}
	return &Server{opts: opts, inflight: make(map[string]*fetch)}, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("GET /v1/entries", s.handleEntries)
	mux.HandleFunc("POST /v1/prestage", s.handlePrestage)
	mux.HandleFunc("POST /v1/pin", s.handlePin)
	mux.HandleFunc("GET /c/{name...}", s.handleGet)

	return mux
}

// handleGet serves cached content by name.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a name is required"})
		return
	}

	if entry, ok := s.opts.Catalog.Lookup(name); ok {
		s.serve(w, r, entry)
		return
	}

	if !s.opts.FetchOnMiss {
		// Honest about what happened and what to do, rather than a bare 404: the
		// content is not here, and fetching it now over this link is a decision
		// the operator has already made once, in config.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "not cached",
			"name":  name,
			"hint": "pre-stage it with POST /v1/prestage; it will be fetched when a " +
				"link worth using is available",
		})
		return
	}

	entry, err := s.fetchCoalesced(r.Context(), name, name, meta{})
	if err != nil {
		s.opts.Log.Warn("upstream fetch failed", "name", name, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.serve(w, r, entry)
}

// serve streams stored content to the client.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, entry catalog.Entry) {
	f, err := s.opts.Store.Open(entry.Digest)
	if err != nil {
		// The catalog believed this was present and the store disagrees. Report
		// it rather than papering over it: it means something deleted content
		// out from under us and the catalog needs reconciling.
		s.opts.Log.Error("catalog and store disagree",
			"name", entry.Name, "digest", entry.Digest.Short(), "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "cached content is missing from the store"})
		return
	}
	defer f.Close()

	if entry.ContentType != "" {
		w.Header().Set("Content-Type", entry.ContentType)
	}
	w.Header().Set("X-Cache", "HIT")
	w.Header().Set("ETag", `"`+string(entry.Digest)+`"`)
	// Content is addressed by the hash of its own bytes, so a matching ETag
	// cannot be stale.
	if match := r.Header.Get("If-None-Match"); match == `"`+string(entry.Digest)+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	http.ServeContent(w, r, entry.Name, entry.AddedAt, f)
}

// fetchCoalesced fetches upstream, with concurrent callers for the same name
// sharing one request.
//
// Without this, a cold cache behind a slow link is worse than no cache: several
// devices asking for the same file at once each open their own upstream
// transfer, and they compete for the very bandwidth the cache exists to
// conserve.
func (s *Server) fetchCoalesced(ctx context.Context, name, upstreamURL string,
	m meta) (catalog.Entry, error) {
	s.mu.Lock()
	if existing, ok := s.inflight[name]; ok {
		s.mu.Unlock()
		select {
		case <-existing.done:
			return existing.entry, existing.err
		case <-ctx.Done():
			return catalog.Entry{}, ctx.Err()
		}
	}
	f := &fetch{done: make(chan struct{})}
	s.inflight[name] = f
	s.mu.Unlock()

	f.entry, f.err = s.fetch(ctx, name, upstreamURL, m)
	close(f.done)

	s.mu.Lock()
	delete(s.inflight, name)
	s.mu.Unlock()

	return f.entry, f.err
}

// fetch retrieves content from upstream and stores it.
func (s *Server) fetch(ctx context.Context, name, upstreamURL string,
	m meta) (catalog.Entry, error) {
	if _, err := url.ParseRequestURI(upstreamURL); err != nil {
		return catalog.Entry{}, fmt.Errorf("%q is not a fetchable URL", upstreamURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return catalog.Entry{}, err
	}
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return catalog.Entry{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return catalog.Entry{}, fmt.Errorf("upstream returned %s", resp.Status)
	}

	digest, written, err := s.opts.Store.Put(resp.Body)
	if err != nil {
		return catalog.Entry{}, err
	}

	entry := catalog.Entry{
		Name:        name,
		Digest:      digest,
		Size:        written,
		ContentType: resp.Header.Get("Content-Type"),
		UpstreamURL: upstreamURL,
		Priority:    m.priority,
		Pinned:      m.pinned,
	}

	// The full transfer is counted as uplink traffic even when the store already
	// held these bytes under another name. Deduplication saves disk, not
	// bandwidth: the content still crossed the link on this request. Counting it
	// as saved would inflate the one figure this is meant to report honestly.
	s.opts.Catalog.Put(entry, written)

	if s.opts.BudgetBytes > 0 {
		if evicted, err := s.opts.Catalog.Evict(s.opts.Store, s.opts.BudgetBytes); err != nil {
			s.opts.Log.Error("eviction failed", "error", err)
		} else if len(evicted) > 0 {
			s.opts.Log.Info("evicted to stay within budget", "entries", len(evicted))
		}
	}
	return entry, nil
}

// Prestage fetches content deliberately, outside any client request.
//
// This is the intended way in. Content arrives over whichever link the operator
// chose, at a time they chose, and is then available locally at full speed or
// with no link at all.
func (s *Server) Prestage(ctx context.Context, name, upstreamURL string,
	priority int, pin bool) (catalog.Entry, error) {

	return s.fetchCoalesced(ctx, name, upstreamURL, meta{priority: priority, pinned: pin})
}

// ---- handlers ----

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.opts.Catalog.Stats(s.opts.Store)
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":          st.Hits,
		"misses":        st.Misses,
		"hit_rate":      st.HitRate(),
		"bytes_served":  st.BytesServed,
		"bytes_fetched": st.BytesFetched,
		"bytes_saved":   st.BytesSaved(),
		"multiplier":    st.Multiplier(),
		"entries":       st.Entries,
		"blobs":         st.Blobs,
		"stored_bytes":  st.StoredBytes,
		"evictions":     st.Evictions,
		"budget_bytes":  s.opts.BudgetBytes,
	})
}

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opts.Catalog.Entries())
}

func (s *Server) handlePrestage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Priority int    `json:"priority"`
		Pin      bool   `json:"pin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	if req.Name == "" {
		req.Name = req.URL
	}

	entry, err := s.Prestage(r.Context(), req.Name, req.URL, req.Priority, req.Pin)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Pin  bool   `json:"pin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.opts.Catalog.Pin(req.Name, req.Pin) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such entry"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name, "pinned": req.Pin})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
