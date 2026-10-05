// Package api serves the terminal and operator HTTP interfaces.
//
// Two audiences on one listener, with different trust: terminals authenticate
// with their own per-device token and may only report about themselves, while
// operators hold a single admin token and can see and change everything. A
// terminal is physically at a subscriber's house and should be assumed
// compromised, so nothing it sends is trusted beyond its own telemetry.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/config"
	"github.com/s-rakim/my-personal-project/controlplane/internal/dataplane"
	"github.com/s-rakim/my-personal-project/controlplane/internal/ipam"
	"github.com/s-rakim/my-personal-project/controlplane/internal/registry"
	"github.com/s-rakim/my-personal-project/controlplane/internal/scheduler"
	"github.com/s-rakim/my-personal-project/controlplane/internal/secret"
	"github.com/s-rakim/my-personal-project/controlplane/internal/session"
	"github.com/s-rakim/my-personal-project/controlplane/internal/telemetry"
)

// maxBodyBytes caps request bodies. Telemetry reports are small; anything larger
// is a bug or an attempt to exhaust memory.
const maxBodyBytes = 256 << 10

// tokenCacheTTL is how long a verified terminal token stays cached.
//
// Verification is PBKDF2 at 600k iterations, which costs roughly a fifth of a
// second of CPU on purpose: that is what makes an offline attack on a stolen
// database expensive. But terminals poll every tick, so paying it on every
// request would spend the whole machine on authentication. Caching the verified
// token for a minute keeps the slow path for the first contact and the stolen
// database, and off the hot path.
const tokenCacheTTL = time.Minute

type cachedToken struct {
	digest  [32]byte
	expires time.Time
}

// Server is the HTTP API.
type Server struct {
	cfg      config.Config
	reg      *registry.Store
	leases   *ipam.Allocator
	sessions *session.Manager
	engine   *scheduler.Engine
	dp       dataplane.Dataplane
	metrics  *telemetry.Registry
	log      *slog.Logger

	mu     sync.Mutex
	tokens map[string]cachedToken
}

// New builds the server.
func New(cfg config.Config, reg *registry.Store, leases *ipam.Allocator,
	sessions *session.Manager, engine *scheduler.Engine, dp dataplane.Dataplane,
	metrics *telemetry.Registry, log *slog.Logger) *Server {

	return &Server{
		cfg: cfg, reg: reg, leases: leases, sessions: sessions,
		engine: engine, dp: dp, metrics: metrics, log: log,
		tokens: make(map[string]cachedToken),
	}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness and readiness are separate on purpose. Liveness says the process
	// is up; readiness says it has actually produced an allocation. An
	// orchestrator that conflates them restarts a daemon that is merely waiting
	// for its first tick.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.HandleFunc("POST /v1/terminal/register", s.terminalAuth(s.handleRegister))
	mux.HandleFunc("POST /v1/terminal/telemetry", s.terminalAuth(s.handleTelemetry))
	mux.HandleFunc("GET /v1/terminal/config", s.terminalAuth(s.handleTerminalConfig))

	mux.HandleFunc("GET /v1/status", s.adminAuth(s.handleStatus))
	mux.HandleFunc("GET /v1/plan", s.adminAuth(s.handlePlan))
	mux.HandleFunc("GET /v1/problem", s.adminAuth(s.handleProblem))
	mux.HandleFunc("GET /v1/sessions", s.adminAuth(s.handleSessions))
	mux.HandleFunc("GET /v1/inventory", s.adminAuth(s.handleGetInventory))
	mux.HandleFunc("PUT /v1/inventory", s.adminAuth(s.handlePutInventory))
	mux.HandleFunc("GET /v1/leases", s.adminAuth(s.handleLeases))
	mux.HandleFunc("GET /v1/dataplane", s.adminAuth(s.handleDataplane))
	mux.HandleFunc("POST /v1/subscribers/{id}/suspend", s.adminAuth(s.handleSuspend))
	mux.HandleFunc("POST /v1/subscribers/{id}/resume", s.adminAuth(s.handleResume))

	return s.withLogging(mux)
}

// ---- middleware ----

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Metrics endpoints are polled constantly; logging them buries everything
		// else.
		if r.URL.Path != "/metrics" && r.URL.Path != "/healthz" {
			s.log.Debug("http request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "took", time.Since(started).String())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// adminAuth guards operator routes.
func (s *Server) adminAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// An unset admin token disables these routes rather than leaving them
		// open. Failing closed is the only safe default for a route that can
		// suspend accounts and rewrite inventory.
		if s.cfg.API.AdminToken == "" {
			writeError(w, http.StatusForbidden,
				"administrative routes are disabled because api.admin_token is unset")
			return
		}
		token, ok := bearer(r)
		if !ok || !secret.EqualConstantTime(token, s.cfg.API.AdminToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bswisp"`)
			writeError(w, http.StatusUnauthorized, "invalid or missing admin token")
			return
		}
		next(w, r)
	}
}

type terminalContextKey struct{}

// terminalAuth guards terminal routes and pins the request to one terminal.
//
// The terminal ID comes from the credential, never from the request body. A CPE
// that could name its own terminal ID could report telemetry as its neighbour
// and have the scheduler move that neighbour onto a worse sector.
func (s *Server) terminalAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearer(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bswisp-terminal"`)
			writeError(w, http.StatusUnauthorized, "missing terminal credential")
			return
		}
		id, token, found := strings.Cut(raw, ".")
		if !found || id == "" || token == "" {
			writeError(w, http.StatusUnauthorized,
				"terminal credential must be <terminal-id>.<token>")
			return
		}

		term, exists := s.reg.Terminal(id)
		if !exists || !term.Enabled || term.TokenHash == "" {
			// One message for every failure mode, so the endpoint cannot be used
			// to enumerate which terminal IDs are real.
			writeError(w, http.StatusUnauthorized, "invalid terminal credential")
			return
		}
		if !s.verifyTerminalToken(id, term.TokenHash, token) {
			writeError(w, http.StatusUnauthorized, "invalid terminal credential")
			return
		}

		ctx := context.WithValue(r.Context(), terminalContextKey{}, term)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) verifyTerminalToken(id, hash, token string) bool {
	digest := sha256.Sum256([]byte(token))

	s.mu.Lock()
	cached, ok := s.tokens[id]
	s.mu.Unlock()

	if ok && time.Now().Before(cached.expires) {
		var diff byte
		for i := range digest {
			diff |= digest[i] ^ cached.digest[i]
		}
		if diff == 0 {
			return true
		}
		// A different token for a known terminal falls through to the slow path,
		// so a rotated token takes effect without waiting for the cache to expire.
	}

	valid, err := secret.Verify(hash, token)
	if err != nil || !valid {
		return false
	}

	s.mu.Lock()
	s.tokens[id] = cachedToken{digest: digest, expires: time.Now().Add(tokenCacheTTL)}
	s.mu.Unlock()
	return true
}

func terminalFrom(r *http.Request) (registry.Terminal, bool) {
	t, ok := r.Context().Value(terminalContextKey{}).(registry.Terminal)
	return t, ok
}

// ---- terminal routes ----

type registerRequest struct {
	// Visible is the terminal's own scan. Measured values beat the propagation
	// model, so this is the single most valuable thing a CPE reports.
	Visible []session.SectorObservation `json:"visible"`

	Firmware string `json:"firmware,omitempty"`
}

type registerResponse struct {
	TerminalID    string  `json:"terminal_id"`
	V4            string  `json:"v4"`
	V4Netmask     string  `json:"v4_netmask"`
	V6Prefix      string  `json:"v6_prefix"`
	SectorID      string  `json:"sector_id"`
	GrantDownMbps float64 `json:"grant_down_mbps"`
	GrantUpMbps   float64 `json:"grant_up_mbps"`
	ReportSeconds float64 `json:"report_seconds"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	term, _ := terminalFrom(r)

	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	snap := s.reg.Snapshot()
	sub, ok := snap.Subscribers[term.SubscriberID]
	if !ok {
		writeError(w, http.StatusConflict, "terminal is not linked to a subscriber")
		return
	}
	if sub.Suspended {
		writeError(w, http.StatusPaymentRequired, "account suspended")
		return
	}

	lease, err := s.leases.Lease(term.ID)
	if err != nil && !lease.V4.IsValid() {
		s.log.Error("register: address allocation failed", "terminal", term.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "address allocation failed")
		return
	}
	if err != nil {
		s.log.Error("register: lease not persisted", "terminal", term.ID, "error", err)
	}

	sess := s.sessions.Start(term.ID, sub.ID, lease)
	if len(req.Visible) > 0 {
		s.sessions.RecordTelemetry(term.ID, session.Telemetry{
			ReportedAt: time.Now().UTC(),
			Visible:    req.Visible,
		})
	}

	writeJSON(w, http.StatusOK, registerResponse{
		TerminalID:    term.ID,
		V4:            lease.V4.String(),
		V4Netmask:     "255.255.255.255",
		V6Prefix:      lease.V6.String(),
		SectorID:      sess.SectorID,
		GrantDownMbps: sess.GrantDownMbps,
		GrantUpMbps:   sess.GrantUpMbps,
		// Report at half the tick interval so the scheduler always has fresh data
		// to plan from rather than data from the previous allocation.
		ReportSeconds: s.cfg.Scheduler.TickSeconds / 2,
	})
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	term, _ := terminalFrom(r)

	var t session.Telemetry
	if err := decodeJSON(r, &t); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Stamped server-side. A CPE with a wrong clock would otherwise make its own
	// telemetry look permanently stale, or permanently fresh.
	t.ReportedAt = time.Now().UTC()

	if !s.sessions.RecordTelemetry(term.ID, t) {
		writeError(w, http.StatusConflict, "no active session; call register first")
		return
	}
	s.handleTerminalConfig(w, r)
}

func (s *Server) handleTerminalConfig(w http.ResponseWriter, r *http.Request) {
	term, _ := terminalFrom(r)

	sess, ok := s.sessions.Get(term.ID)
	if !ok {
		writeError(w, http.StatusConflict, "no active session; call register first")
		return
	}
	writeJSON(w, http.StatusOK, registerResponse{
		TerminalID:    term.ID,
		V4:            sess.Lease.V4.String(),
		V4Netmask:     "255.255.255.255",
		V6Prefix:      sess.Lease.V6.String(),
		SectorID:      sess.SectorID,
		GrantDownMbps: sess.GrantDownMbps,
		GrantUpMbps:   sess.GrantUpMbps,
		ReportSeconds: s.cfg.Scheduler.TickSeconds / 2,
	})
}

// ---- operator routes ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	v4Used, v4Cap, v6Used, v6Cap := s.leases.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":   s.cfg.NodeID,
		"scheduler": s.engine.Status(),
		"ipam": map[string]any{
			"v4_used": v4Used, "v4_capacity": v4Cap,
			"v6_used": v6Used, "v6_capacity": v6Cap,
		},
		"sessions_online": len(s.sessions.Online()),
		"sessions_total":  len(s.sessions.All()),
	})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.LastPlan())
}

// handleProblem returns the last problem document. This is the thing to capture
// and pipe into the solver by hand when an allocation looks wrong: the solver is
// deterministic, so the problem reproduces the decision exactly.
func (s *Server) handleProblem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.LastProblem())
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sessions.All())
}

func (s *Server) handleGetInventory(w http.ResponseWriter, r *http.Request) {
	snap := s.reg.Snapshot()
	// Credentials never leave the daemon, even to an authenticated operator. An
	// admin token that leaks should not also hand over every password hash.
	for id, sub := range snap.Subscribers {
		sub.PasswordHash = ""
		snap.Subscribers[id] = sub
	}
	for id, t := range snap.Terminals {
		t.TokenHash = ""
		snap.Terminals[id] = t
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handlePutInventory(w http.ResponseWriter, r *http.Request) {
	var snap registry.Snapshot
	if err := decodeJSON(r, &snap); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// A replace with blank credentials would lock every subscriber out, since the
	// provisioning system that sends inventory never sees password hashes. Carry
	// the existing ones forward.
	current := s.reg.Snapshot()
	for id, sub := range snap.Subscribers {
		if sub.PasswordHash == "" {
			if prior, ok := current.Subscribers[id]; ok {
				sub.PasswordHash = prior.PasswordHash
				snap.Subscribers[id] = sub
			}
		}
	}
	for id, t := range snap.Terminals {
		if t.TokenHash == "" {
			if prior, ok := current.Terminals[id]; ok {
				t.TokenHash = prior.TokenHash
				snap.Terminals[id] = t
			}
		}
	}

	if err := s.reg.Replace(snap); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("inventory replaced",
		"sites", len(snap.Sites), "sectors", len(snap.Sectors),
		"subscribers", len(snap.Subscribers), "terminals", len(snap.Terminals))
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (s *Server) handleLeases(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.leases.Leases())
}

func (s *Server) handleDataplane(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.dp.Describe())
}

func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	s.setSuspended(w, r, true)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.setSuspended(w, r, false)
}

func (s *Server) setSuspended(w http.ResponseWriter, r *http.Request, suspended bool) {
	id := r.PathValue("id")
	snap := s.reg.Snapshot()
	sub, ok := snap.Subscribers[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no subscriber %q", id))
		return
	}
	sub.Suspended = suspended
	if err := s.reg.PutSubscriber(sub); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The change reaches the dataplane on the next tick. Saying so explicitly
	// stops an operator from watching a dashboard and assuming nothing happened.
	s.log.Info("subscriber suspension changed", "subscriber", id, "suspended", suspended)
	writeJSON(w, http.StatusOK, map[string]any{
		"subscriber_id":   id,
		"suspended":       suspended,
		"effective_after": fmt.Sprintf("%.0fs (next allocation tick)", s.cfg.Scheduler.TickSeconds),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Status()
	if !st.Healthy {
		reason := st.LastError
		if reason == "" {
			reason = "no allocation tick has completed yet"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not ready", "reason": reason,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "epoch": st.Epoch})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.metrics.Render(w); err != nil {
		s.log.Warn("metrics render failed", "error", err)
	}
}

// ---- helpers ----

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
