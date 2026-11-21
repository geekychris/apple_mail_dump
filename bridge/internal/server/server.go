// Package server is the bridge's read-only HTTP API. Every request passes,
// in order: the source-address allowlist, a per-address rate limit, and (for
// everything but /healthz) the bearer token. There are no write endpoints, no
// SQL and no filesystem paths in requests; ids are integers.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geekychris/apple_mail_dump/bridge/internal/chatdb"
	"github.com/geekychris/apple_mail_dump/bridge/internal/maildb"
	"github.com/geekychris/apple_mail_dump/bridge/internal/secrets"
	"github.com/geekychris/apple_mail_dump/bridge/internal/snapshot"
)

// Version is reported by /healthz and /v1/status.
var Version = "dev"

// MailSource locates and reads Apple Mail's data.
type MailSource struct {
	Root        string // ~/Library/Mail
	SnapshotDir string
	MaxAge      time.Duration
	Emlx        *maildb.EmlxIndex

	mu       sync.Mutex
	snap     *snapshot.Snapshot
	lastFind time.Time
	findErr  error
	store    *maildb.Store
	storeDB  *sql.DB
}

func (m *MailSource) open(ctx context.Context) (*sql.DB, *maildb.Store, error) {
	m.mu.Lock()
	if m.snap == nil {
		if m.findErr != nil && time.Since(m.lastFind) < 10*time.Second {
			err := m.findErr
			m.mu.Unlock()
			return nil, nil, err
		}
		p, err := maildb.FindEnvelopeIndex(m.Root)
		m.lastFind = time.Now()
		if err != nil {
			m.findErr = classify(err)
			m.mu.Unlock()
			return nil, nil, m.findErr
		}
		m.findErr = nil
		m.snap = snapshot.New(p, m.SnapshotDir, m.MaxAge)
	}
	snap := m.snap
	m.mu.Unlock()
	db, err := snap.DB()
	if err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.storeDB != db {
		st, err := maildb.DiscoverSchema(ctx, db)
		if err != nil {
			return nil, nil, err
		}
		m.store, m.storeDB = st, db
	}
	return db, m.store, nil
}

func (m *MailSource) refreshedAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap == nil {
		return time.Time{}
	}
	return m.snap.RefreshedAt()
}

// ChatSource reads Messages' chat.db.
type ChatSource struct {
	Path        string
	SnapshotDir string
	MaxAge      time.Duration

	once    sync.Once
	snap    *snapshot.Snapshot
	mu      sync.Mutex
	store   *chatdb.Store
	storeDB *sql.DB
}

func (c *ChatSource) open(ctx context.Context) (*sql.DB, *chatdb.Store, error) {
	c.once.Do(func() { c.snap = snapshot.New(c.Path, c.SnapshotDir, c.MaxAge) })
	db, err := c.snap.DB()
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeDB != db {
		st, err := chatdb.DiscoverSchema(ctx, db)
		if err != nil {
			return nil, nil, err
		}
		c.store, c.storeDB = st, db
	}
	return db, c.store, nil
}

// Server wires the sources to HTTP.
type Server struct {
	Token   string
	Allow   []*net.IPNet
	Mail    *MailSource
	Chat    *ChatSource
	Log     *log.Logger
	Limiter *Limiter
}

// Handler returns the full middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /v1/mail/messages", s.mailList)
	mux.HandleFunc("GET /v1/mail/messages/{id}", s.mailGet)
	mux.HandleFunc("GET /v1/mail/messages/{id}/raw", s.mailRaw)
	mux.HandleFunc("GET /v1/messages/chats", s.chats)
	mux.HandleFunc("GET /v1/messages", s.messages)
	return s.middleware(mux)
}

type statusRecorder struct {
	http.ResponseWriter
	code  int
	bytes int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		ip := remoteIP(r)
		defer func() {
			// Access log: no query values, no bodies, no headers.
			s.Log.Printf("access remote=%s method=%s path=%s query_keys=%s status=%d bytes=%d dur_ms=%d",
				ip, r.Method, r.URL.Path, queryKeys(r), rec.code, rec.bytes, time.Since(start).Milliseconds())
		}()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if ip == nil || !s.allowed(ip) {
			writeErr(rec, http.StatusForbidden, "forbidden", "source address not allowed")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(rec, http.StatusMethodNotAllowed, "method_not_allowed", "read-only API")
			return
		}
		if s.Limiter != nil && !s.Limiter.Allow(ip.String()) {
			rec.Header().Set("Retry-After", "1")
			writeErr(rec, http.StatusTooManyRequests, "rate_limited", "slow down")
			return
		}
		if r.URL.Path != "/healthz" {
			if s.Limiter != nil && s.Limiter.AuthBlocked(ip.String()) {
				rec.Header().Set("Retry-After", "60")
				writeErr(rec, http.StatusTooManyRequests, "rate_limited", "too many failed authentications")
				return
			}
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !secrets.TokenEqual(strings.TrimSpace(tok), s.Token) {
				if s.Limiter != nil {
					s.Limiter.AuthFailed(ip.String())
				}
				rec.Header().Set("WWW-Authenticate", `Bearer realm="mailbridge"`)
				writeErr(rec, http.StatusUnauthorized, "unauthorized", "missing or wrong bearer token")
				return
			}
		}
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) allowed(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	for _, n := range s.Allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func queryKeys(r *http.Request) string {
	q := r.URL.Query()
	if len(q) == 0 {
		return "-"
	}
	ks := make([]string, 0, len(q))
	for k := range q {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"version": Version, "time": time.Now().UTC()}
	if s.Mail != nil {
		mail := map[string]any{}
		if db, st, err := s.Mail.open(r.Context()); err != nil {
			mail["ok"], mail["error"] = false, publicErr(err)
		} else if c, err := st.Counts(r.Context(), db); err != nil {
			mail["ok"], mail["error"] = false, "query failed"
		} else {
			mail["ok"], mail["counts"] = true, c
			mail["snapshot_at"] = s.Mail.refreshedAt().UTC()
			mail["emlx_indexed"] = s.Mail.Emlx.Size()
		}
		out["mail"] = mail
	}
	if s.Chat != nil {
		msgs := map[string]any{}
		if db, st, err := s.Chat.open(r.Context()); err != nil {
			msgs["ok"], msgs["error"] = false, publicErr(err)
		} else if c, err := st.Counts(r.Context(), db); err != nil {
			msgs["ok"], msgs["error"] = false, "query failed"
		} else {
			msgs["ok"], msgs["counts"] = true, c
			msgs["snapshot_at"] = s.Chat.snap.RefreshedAt().UTC()
		}
		out["messages"] = msgs
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) mailList(w http.ResponseWriter, r *http.Request) {
	if s.Mail == nil {
		writeErr(w, http.StatusNotFound, "disabled", "mail source disabled")
		return
	}
	since, ok1 := intParam(r, "since", 0)
	after, ok2 := intParam(r, "received_after", 0)
	limit, ok3 := intParam(r, "limit", 100)
	if !ok1 || !ok2 || !ok3 || since < 0 || after < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "since, received_after and limit must be non-negative integers")
		return
	}
	limit = clamp(limit, 1, 1000)
	db, st, err := s.Mail.open(r.Context())
	if err != nil {
		sourceErr(w, err)
		return
	}
	ms, err := st.List(r.Context(), db, since, after, int(limit))
	if err != nil {
		s.Log.Printf("mail list query error: %v", err)
		writeErr(w, http.StatusInternalServerError, "query_failed", "query failed")
		return
	}
	if ms == nil {
		ms = []maildb.Meta{}
	}
	next := since
	if len(ms) > 0 {
		next = ms[len(ms)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":   ms,
		"next_since": next,
		"has_more":   len(ms) == int(limit),
	})
}

func (s *Server) mailMeta(w http.ResponseWriter, r *http.Request) (*maildb.Meta, bool) {
	if s.Mail == nil {
		writeErr(w, http.StatusNotFound, "disabled", "mail source disabled")
		return nil, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "id must be a positive integer")
		return nil, false
	}
	db, st, err := s.Mail.open(r.Context())
	if err != nil {
		sourceErr(w, err)
		return nil, false
	}
	m, err := st.Get(r.Context(), db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query_failed", "query failed")
		return nil, false
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such message")
		return nil, false
	}
	return m, true
}

func (s *Server) loadRaw(id int64) ([]byte, bool, error) {
	p, err := s.Mail.Emlx.Lookup(id)
	if err != nil {
		return nil, false, classify(err)
	}
	if p == "" {
		return nil, false, nil
	}
	raw, partial, err := maildb.ReadRaw(p)
	if err != nil {
		return nil, false, classify(err)
	}
	return raw, partial, nil
}

func (s *Server) mailGet(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mailMeta(w, r)
	if !ok {
		return
	}
	raw, partial, err := s.loadRaw(m.ID)
	out := map[string]any{"meta": m}
	switch {
	case err != nil:
		sourceErr(w, err)
		return
	case raw == nil:
		// Mail has the envelope but never downloaded the body.
		out["body_status"] = "not_downloaded"
	default:
		p, err := maildb.Parse(raw)
		if err != nil {
			out["body_status"] = "unparseable"
		} else {
			p.Partial = partial
			out["body_status"] = "ok"
			out["headers"], out["text"], out["html"], out["attachments"], out["partial"] =
				p.Headers, p.Text, p.HTML, p.Attachments, p.Partial
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) mailRaw(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mailMeta(w, r)
	if !ok {
		return
	}
	raw, partial, err := s.loadRaw(m.ID)
	if err != nil {
		sourceErr(w, err)
		return
	}
	if raw == nil {
		writeErr(w, http.StatusNotFound, "not_downloaded", "message body is not on disk")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if partial {
		w.Header().Set("X-Mailbridge-Partial", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) chats(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		writeErr(w, http.StatusNotFound, "disabled", "messages source disabled")
		return
	}
	limit, ok := intParam(r, "limit", 500)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "limit must be an integer")
		return
	}
	db, st, err := s.Chat.open(r.Context())
	if err != nil {
		sourceErr(w, err)
		return
	}
	cs, err := st.Chats(r.Context(), db, int(clamp(limit, 1, 5000)))
	if err != nil {
		s.Log.Printf("chats query error: %v", err)
		writeErr(w, http.StatusInternalServerError, "query_failed", "query failed")
		return
	}
	if cs == nil {
		cs = []chatdb.Chat{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": cs})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		writeErr(w, http.StatusNotFound, "disabled", "messages source disabled")
		return
	}
	since, ok1 := intParam(r, "since", 0)
	chat, ok2 := intParam(r, "chat", 0)
	after, ok3 := intParam(r, "after", 0)
	limit, ok4 := intParam(r, "limit", 200)
	if !ok1 || !ok2 || !ok3 || !ok4 || since < 0 || chat < 0 || after < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "since, chat, after and limit must be non-negative integers")
		return
	}
	limit = clamp(limit, 1, 2000)
	db, st, err := s.Chat.open(r.Context())
	if err != nil {
		sourceErr(w, err)
		return
	}
	ms, err := st.Messages(r.Context(), db, since, chat, after, int(limit))
	if err != nil {
		s.Log.Printf("messages query error: %v", err)
		writeErr(w, http.StatusInternalServerError, "query_failed", "query failed")
		return
	}
	if ms == nil {
		ms = []chatdb.Message{}
	}
	next := since
	if len(ms) > 0 {
		next = ms[len(ms)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": ms, "next_since": next, "has_more": len(ms) == int(limit)})
}

func intParam(r *http.Request, name string, def int64) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func classify(err error) error {
	switch {
	case errors.Is(err, snapshot.ErrNoAccess), errors.Is(err, snapshot.ErrNotFound):
		return err
	case errors.Is(err, os.ErrPermission):
		return snapshot.ErrNoAccess
	case errors.Is(err, os.ErrNotExist):
		return snapshot.ErrNotFound
	}
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Err != nil && pe.Err.Error() == "operation not permitted" {
		return snapshot.ErrNoAccess
	}
	return err
}

func publicErr(err error) string {
	switch {
	case errors.Is(err, snapshot.ErrNoAccess):
		return "no_access: grant Full Disk Access to the mailbridge binary"
	case errors.Is(err, snapshot.ErrNotFound):
		return "not_found"
	}
	return "unavailable"
}

func sourceErr(w http.ResponseWriter, err error) {
	err = classify(err)
	switch {
	case errors.Is(err, snapshot.ErrNoAccess):
		writeErr(w, http.StatusServiceUnavailable, "no_access", "grant Full Disk Access to the mailbridge binary")
	case errors.Is(err, snapshot.ErrNotFound):
		writeErr(w, http.StatusServiceUnavailable, "not_found", "database not found")
	default:
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "source unavailable")
	}
}

func writeErr(w http.ResponseWriter, code int, kind, detail string) {
	writeJSON(w, code, map[string]string{"error": kind, "detail": detail})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
	}
}
