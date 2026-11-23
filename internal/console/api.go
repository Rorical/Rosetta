package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	errUnknownChart = errors.New("unknown chart or range")
	errBadQuery     = errors.New("invalid query")
)

// competitionWindows are the spans the competition view summarizes.
var competitionWindows = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

const (
	maxRows        = 500
	maxReasonBytes = 200
	operatorActor  = "operator"
	sessionRecheck = 5 * time.Second
)

// Server is the console HTTP surface.
type Server struct {
	hub        *Hub
	store      *Store
	auth       *Auth
	prometheus *Prometheus
	assets     fs.FS
	hosts      map[string]bool
	logins     chan struct{}
	now        func() time.Time
}

// NewServer accepts loopback hosts plus publicHosts, the names a gateway serves.
func NewServer(hub *Hub, store *Store, auth *Auth, prometheus *Prometheus, assets fs.FS, publicHosts []string) *Server {
	hosts := make(map[string]bool, len(publicHosts))
	for _, host := range publicHosts {
		hosts[host] = true
	}
	return &Server{hub: hub, store: store, auth: auth, prometheus: prometheus, assets: assets, hosts: hosts, logins: make(chan struct{}, loginSlots), now: time.Now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", s.login)
	mux.HandleFunc("GET /api/session", s.authenticated(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"prometheus": s.prometheus != nil})
	}))
	mux.HandleFunc("DELETE /api/session", s.authenticated(s.logout))
	mux.HandleFunc("GET /api/instances", s.authenticated(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.hub.Views()) }))
	mux.HandleFunc("GET /api/events", s.authenticated(s.events))
	mux.HandleFunc("GET /api/instances/{name}/history/{chart}", s.authenticated(s.history))
	mux.HandleFunc("PUT /api/instances/{name}/gate", s.authenticated(s.setGate))
	mux.HandleFunc("GET /api/alerts", s.authenticated(s.alerts))
	mux.HandleFunc("GET /api/instances/{name}/sends", s.authenticated(s.instance(s.sends)))
	mux.HandleFunc("GET /api/instances/{name}/candidates", s.authenticated(s.instance(s.candidates)))
	mux.HandleFunc("GET /api/instances/{name}/pnl", s.authenticated(s.instance(s.pnl)))
	mux.HandleFunc("GET /api/instances/{name}/execution", s.authenticated(s.instance(s.execution)))
	mux.HandleFunc("GET /api/instances/{name}/competition", s.authenticated(s.instance(s.competition)))
	mux.HandleFunc("GET /api/instances/{name}/assets", s.authenticated(s.instance(s.tokenUnits)))
	mux.HandleFunc("GET /api/audit", s.authenticated(s.audit))
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.hub.WriteMetrics(w)
	})
	mux.Handle("/", s.spa())
	return s.secured(mux)
}

func (s *Server) secured(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host, s.hosts) {
			http.Error(w, "unrecognized host", http.StatusForbidden)
			return
		}
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Valid(sessionFrom(r), s.now()) {
			writeError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		next(w, r)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	select {
	case s.logins <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.logins }()
	session, err := s.auth.Login(request.Token, s.now())
	if err != nil {
		slog.Warn("console: failed sign-in")
		// Hold the slot even if the client disconnects, or dropping the
		// connection would bypass the cap.
		time.Sleep(failedLoginDelay)
		writeError(w, http.StatusUnauthorized, "token not recognized")
		return
	}
	http.SetCookie(w, s.cookie(r, session, int(sessionLifetime.Seconds())))
	writeJSON(w, http.StatusOK, map[string]any{"prometheus": s.prometheus != nil})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(sessionFrom(r))
	http.SetCookie(w, s.cookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// The Secure flag follows the browser's scheme: a gateway terminates TLS, while
// an SSH tunnel to localhost is plain HTTP. The header can only strengthen it.
func (s *Server) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	updates, cancel := s.hub.Subscribe()
	defer cancel()
	initial, err := json.Marshal(s.hub.Views())
	if err != nil {
		return
	}
	send := func(payload []byte) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send(initial) {
		return
	}
	session := sessionFrom(r)
	// Polling publishes every second, but a ticker keeps the check independent
	// of that, so a stream cannot outlive sign-out or expiry while idle.
	recheck := time.NewTicker(sessionRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-recheck.C:
			if !s.auth.Valid(session, s.now()) {
				return
			}
		case payload := <-updates:
			if !s.auth.Valid(session, s.now()) || !send(payload) {
				return
			}
		}
	}
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	if s.prometheus == nil {
		writeError(w, http.StatusServiceUnavailable, "prometheus not configured")
		return
	}
	name := r.PathValue("name")
	if !s.hub.Known(name) {
		writeError(w, http.StatusNotFound, "unknown instance")
		return
	}
	history, err := s.prometheus.Chart(r.Context(), name, r.PathValue("chart"), r.URL.Query().Get("range"), s.now())
	if errors.Is(err, errUnknownChart) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		slog.Warn("console: chart query", "error", err)
		writeError(w, http.StatusBadGateway, "prometheus query failed")
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	if s.prometheus == nil {
		writeJSON(w, http.StatusOK, []Alert{})
		return
	}
	alerts, err := s.prometheus.Alerts(r.Context())
	if err != nil {
		slog.Warn("console: alerts query", "error", err)
		writeError(w, http.StatusBadGateway, "prometheus query failed")
		return
	}
	writeJSON(w, http.StatusOK, alerts)
}

// setGate records intent before acting, so a refused or crashed change still
// leaves a trail. Without a writable audit log the action does not happen.
func (s *Server) setGate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.hub.Known(name) {
		writeError(w, http.StatusNotFound, "unknown instance")
		return
	}
	var request struct {
		Paused bool   `json:"paused"`
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" || len(request.Reason) > maxReasonBytes {
		writeError(w, http.StatusBadRequest, "reason is required, up to 200 bytes")
		return
	}
	action := "resume_delivery"
	if request.Paused {
		action = "pause_delivery"
	}
	// The audit trail outlives this request: a disconnected browser must not
	// strand the entry as pending.
	ctx := context.WithoutCancel(r.Context())
	id, err := s.store.beginAudit(ctx, AuditEntry{AtMS: s.now().UnixMilli(), Actor: operatorActor, Instance: name, Action: action, Reason: request.Reason})
	if err != nil {
		slog.Error("console: audit refused gate change", "error", err)
		writeError(w, http.StatusInternalServerError, "audit log unavailable; gate unchanged")
		return
	}
	state, err := s.hub.clients[name].setGate(ctx, request.Paused, operatorActor, request.Reason)
	if err != nil {
		if auditErr := s.store.finishAudit(ctx, id, outcomeFailed, err.Error()); auditErr != nil {
			slog.Error("console: finish audit", "error", auditErr)
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := s.store.finishAudit(ctx, id, outcomeApplied, ""); err != nil {
		slog.Error("console: finish audit", "error", err)
	}
	writeJSON(w, http.StatusOK, state)
}

// instance resolves the path's instance before running next.
func (s *Server) instance(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !s.hub.Known(name) {
			writeError(w, http.StatusNotFound, "unknown instance")
			return
		}
		next(w, r, name)
	}
}

func (s *Server) sends(w http.ResponseWriter, r *http.Request, name string) {
	query := r.URL.Query()
	sends, err := s.store.Sends(r.Context(), name, query.Get("outcome"), rowLimit(query.Get("limit")), s.now())
	if errors.Is(err, errBadQuery) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.respond(w, sends, err)
}

func (s *Server) candidates(w http.ResponseWriter, r *http.Request, name string) {
	query := r.URL.Query()
	candidates, err := s.store.Candidates(r.Context(), name, query.Get("source"), query.Get("sort"), rowLimit(query.Get("limit")))
	if errors.Is(err, errBadQuery) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.respond(w, candidates, err)
}

func (s *Server) pnl(w http.ResponseWriter, r *http.Request, name string) {
	days, err := s.store.PnL(r.Context(), name)
	s.respond(w, days, err)
}

// execution counts skips since the given time, normally the run's start, so
// they line up with the run's funnel counters.
func (s *Server) execution(w http.ResponseWriter, r *http.Request, name string) {
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || since < 0 {
		writeError(w, http.StatusBadRequest, "since must be a time in milliseconds")
		return
	}
	execution, err := s.store.Execution(r.Context(), name, since, s.now())
	s.respond(w, execution, err)
}

func (s *Server) competition(w http.ResponseWriter, r *http.Request, name string) {
	window, ok := competitionWindows[r.URL.Query().Get("range")]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown range")
		return
	}
	competition, err := s.store.Competition(r.Context(), name, s.now().Add(-window).UnixMilli(), rowLimit(r.URL.Query().Get("limit")))
	s.respond(w, competition, err)
}

func (s *Server) tokenUnits(w http.ResponseWriter, r *http.Request, name string) {
	assets, err := s.store.Assets(r.Context(), name)
	s.respond(w, assets, err)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.Audit(r.Context(), rowLimit(r.URL.Query().Get("limit")))
	s.respond(w, entries, err)
}

func (s *Server) respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		slog.Error("console: database query", "error", err)
		writeError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func rowLimit(text string) int {
	limit, err := strconv.Atoi(text)
	if err != nil || limit <= 0 {
		return 100
	}
	return min(limit, maxRows)
}

// spa serves built assets and falls back to index.html for client routes.
func (s *Server) spa() http.Handler {
	files := http.FileServerFS(s.assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if _, err := fs.Stat(s.assets, strings.TrimPrefix(r.URL.Path, "/")); err == nil && r.URL.Path != "/" {
			files.ServeHTTP(w, r)
			return
		}
		index, err := fs.ReadFile(s.assets, "index.html")
		if err != nil {
			http.Error(w, "web assets missing; run make build-console", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("console: write response", "error", err)
	}
}
