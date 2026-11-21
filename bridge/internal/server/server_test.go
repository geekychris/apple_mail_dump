package server_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geekychris/apple_mail_dump/bridge/internal/fixtures"
	"github.com/geekychris/apple_mail_dump/bridge/internal/maildb"
	"github.com/geekychris/apple_mail_dump/bridge/internal/secrets"
	"github.com/geekychris/apple_mail_dump/bridge/internal/server"
)

const token = "test-token-0123456789-abcdefghijklmnopqrstuvwxyz"

type env struct {
	srv  *server.Server
	h    http.Handler
	logs *bytes.Buffer
	mail *fixtures.Mail
	chat *fixtures.Chat
}

func newEnv(t *testing.T) *env {
	t.Helper()
	m := fixtures.NewMail(t)
	now := time.Now().Unix()
	for i := int64(1); i <= 5; i++ {
		m.AddMessage(t, i, now-(10-i)*86400, "subject")
	}
	m.WriteEmlx(t, 1, fixtures.SampleMIME, false)
	c := fixtures.NewChat(t)
	c.AddMessage(t, 1, 1, 1, now-100, "hello", nil, false)
	c.AddMessage(t, 2, 2, 2, now-50, "", fixtures.AttributedBody("decoded"), false)
	_, lan, _ := net.ParseCIDR("192.168.1.0/24")
	logs := &bytes.Buffer{}
	snap := t.TempDir()
	s := &server.Server{
		Token: token, Allow: []*net.IPNet{lan}, Log: log.New(logs, "", 0),
		Limiter: server.NewLimiter(1000, 1000),
		Mail:    &server.MailSource{Root: m.Root, SnapshotDir: snap + "/mail", Emlx: maildb.NewEmlxIndex(m.Root, time.Hour)},
		Chat:    &server.ChatSource{Path: c.Path, SnapshotDir: snap + "/chat"},
	}
	return &env{srv: s, h: s.Handler(), logs: logs, mail: m, chat: c}
}

func (e *env) do(t *testing.T, method, path, remote, tok string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

const lanPeer = "192.168.1.50:41000"

func TestAuth(t *testing.T) {
	e := newEnv(t)
	if w := e.do(t, "GET", "/v1/status", lanPeer, ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := e.do(t, "GET", "/v1/status", lanPeer, "wrong"); w.Code != 401 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := e.do(t, "GET", "/v1/status", lanPeer, token+"x"); w.Code != 401 {
		t.Fatalf("token with suffix: %d", w.Code)
	}
	if w := e.do(t, "GET", "/healthz", lanPeer, ""); w.Code != 200 {
		t.Fatalf("healthz without token: %d", w.Code)
	}
	w := e.do(t, "GET", "/v1/status", lanPeer, token)
	if w.Code != 200 {
		t.Fatalf("good token: %d %s", w.Code, w.Body)
	}
	var st struct {
		Mail struct {
			OK     bool           `json:"ok"`
			Counts map[string]any `json:"counts"`
		} `json:"mail"`
		Messages struct {
			OK     bool           `json:"ok"`
			Counts map[string]any `json:"counts"`
		} `json:"messages"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if !st.Mail.OK || st.Mail.Counts["messages"].(float64) != 5 || !st.Messages.OK || st.Messages.Counts["messages"].(float64) != 2 {
		t.Fatalf("status: %s", w.Body)
	}
}

func TestAllowlistAndMethods(t *testing.T) {
	e := newEnv(t)
	for _, remote := range []string{"10.0.0.5:1234", "192.168.2.10:1234", "100.100.1.1:1234", "[2001:db8::1]:443"} {
		if w := e.do(t, "GET", "/v1/status", remote, token); w.Code != 403 {
			t.Fatalf("%s: %d", remote, w.Code)
		}
		if w := e.do(t, "GET", "/healthz", remote, ""); w.Code != 403 {
			t.Fatalf("healthz from %s: %d", remote, w.Code)
		}
	}
	if w := e.do(t, "GET", "/healthz", "127.0.0.1:5555", ""); w.Code != 200 {
		t.Fatalf("loopback: %d", w.Code)
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if w := e.do(t, m, "/v1/mail/messages", lanPeer, token); w.Code != 405 {
			t.Fatalf("%s: %d", m, w.Code)
		}
	}
	// Tailscale range only when configured.
	_, ts, _ := net.ParseCIDR("100.64.0.0/10")
	e.srv.Allow = append(e.srv.Allow, ts)
	if w := e.do(t, "GET", "/v1/status", "100.100.1.1:1234", token); w.Code != 200 {
		t.Fatalf("tailscale allowed: %d", w.Code)
	}
}

func TestRateLimitAndAuthLockout(t *testing.T) {
	e := newEnv(t)
	e.srv.Limiter = server.NewLimiter(0.001, 3)
	e.h = e.srv.Handler()
	codes := []int{}
	for i := 0; i < 5; i++ {
		codes = append(codes, e.do(t, "GET", "/healthz", lanPeer, "").Code)
	}
	if codes[2] != 200 || codes[3] != 429 || codes[4] != 429 {
		t.Fatalf("rate limit codes %v", codes)
	}
	// Another address has its own bucket.
	if w := e.do(t, "GET", "/healthz", "192.168.1.51:1", ""); w.Code != 200 {
		t.Fatalf("other peer: %d", w.Code)
	}

	e.srv.Limiter = server.NewLimiter(1000, 1000)
	e.h = e.srv.Handler()
	for i := 0; i < 10; i++ {
		e.do(t, "GET", "/v1/status", lanPeer, "bad")
	}
	if w := e.do(t, "GET", "/v1/status", lanPeer, token); w.Code != 429 {
		t.Fatalf("after 10 bad tokens even the good one is locked out for the window, got %d", w.Code)
	}
}

func TestMailEndpointsAndWatermark(t *testing.T) {
	e := newEnv(t)
	var page struct {
		Messages []maildb.Meta `json:"messages"`
		Next     int64         `json:"next_since"`
		More     bool          `json:"has_more"`
	}
	seen := 0
	since := int64(0)
	for i := 0; i < 10; i++ {
		w := e.do(t, "GET", "/v1/mail/messages?limit=2&since="+itoa(since), lanPeer, token)
		if w.Code != 200 {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		page.Messages = nil
		json.Unmarshal(w.Body.Bytes(), &page)
		seen += len(page.Messages)
		if !page.More {
			break
		}
		since = page.Next
	}
	if seen != 5 {
		t.Fatalf("paged through %d messages, want 5", seen)
	}
	after := time.Now().Add(-(7*24 + 12) * time.Hour).Unix() // messages are 9..5 days old
	w := e.do(t, "GET", "/v1/mail/messages?received_after="+itoa(after), lanPeer, token)
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Messages) != 3 {
		t.Fatalf("received_after: %d", len(page.Messages))
	}
	if w := e.do(t, "GET", "/v1/mail/messages?since=-1", lanPeer, token); w.Code != 400 {
		t.Fatalf("negative since: %d", w.Code)
	}
	if w := e.do(t, "GET", "/v1/mail/messages?limit=abc", lanPeer, token); w.Code != 400 {
		t.Fatalf("bad limit: %d", w.Code)
	}

	w = e.do(t, "GET", "/v1/mail/messages/1", lanPeer, token)
	var d struct {
		Status      string              `json:"body_status"`
		Text        string              `json:"text"`
		Attachments []maildb.Attachment `json:"attachments"`
	}
	json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d.Status != "ok" || !strings.Contains(d.Text, "Café") || len(d.Attachments) != 1 {
		t.Fatalf("detail: %d status=%s", w.Code, d.Status)
	}
	w = e.do(t, "GET", "/v1/mail/messages/2", lanPeer, token)
	json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d.Status != "not_downloaded" {
		t.Fatalf("detail without emlx: %d %s", w.Code, d.Status)
	}
	w = e.do(t, "GET", "/v1/mail/messages/1/raw", lanPeer, token)
	if w.Code != 200 || w.Header().Get("Content-Type") != "message/rfc822" || w.Body.String() != fixtures.SampleMIME {
		t.Fatalf("raw: %d", w.Code)
	}
	for _, p := range []string{"/v1/mail/messages/99", "/v1/mail/messages/2/raw"} {
		if w := e.do(t, "GET", p, lanPeer, token); w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/v1/mail/messages/..%2F..%2Fetc", "/v1/mail/messages/abc", "/v1/mail/messages/0"} {
		if w := e.do(t, "GET", p, lanPeer, token); w.Code != 400 && w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}

func TestMessagesEndpoints(t *testing.T) {
	e := newEnv(t)
	w := e.do(t, "GET", "/v1/messages/chats", lanPeer, token)
	var cs struct {
		Chats []map[string]any `json:"chats"`
	}
	json.Unmarshal(w.Body.Bytes(), &cs)
	if w.Code != 200 || len(cs.Chats) != 2 {
		t.Fatalf("chats: %d %d", w.Code, len(cs.Chats))
	}
	w = e.do(t, "GET", "/v1/messages?since=1", lanPeer, token)
	var ms struct {
		Messages []map[string]any `json:"messages"`
		Next     int64            `json:"next_since"`
	}
	json.Unmarshal(w.Body.Bytes(), &ms)
	if w.Code != 200 || len(ms.Messages) != 1 || ms.Messages[0]["text"] != "decoded" || ms.Next != 2 {
		t.Fatalf("messages since=1: %d rows next=%d", len(ms.Messages), ms.Next)
	}
	w = e.do(t, "GET", "/v1/messages?chat=1", lanPeer, token)
	json.Unmarshal(w.Body.Bytes(), &ms)
	if len(ms.Messages) != 1 {
		t.Fatalf("chat filter: %d", len(ms.Messages))
	}
}

func TestAccessLogHasNoContentOrSecrets(t *testing.T) {
	e := newEnv(t)
	e.do(t, "GET", "/v1/mail/messages/1", lanPeer, token)
	e.do(t, "GET", "/v1/messages?since=0&chat=2", lanPeer, token)
	e.do(t, "GET", "/v1/status", lanPeer, "a-wrong-token-value")
	logs := e.logs.String()
	for _, bad := range []string{token, "a-wrong-token-value", "Café", "decoded", "hello", "alice@example.com", "chat=2"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("access log leaked %q", bad)
		}
	}
	if !strings.Contains(logs, "query_keys=chat,since") || !strings.Contains(logs, "status=401") {
		t.Fatalf("access log missing fields:\n%s", logs)
	}
}

func TestNoAccessIsReportedAsFullDiskAccess(t *testing.T) {
	e := newEnv(t)
	e.srv.Mail.Root = t.TempDir() + "/missing"
	w := e.do(t, "GET", "/v1/mail/messages", lanPeer, token)
	if w.Code != 503 {
		t.Fatalf("missing mail root: %d", w.Code)
	}
}

// TestTLSPinning serves the API over TLS with a generated self-signed
// certificate and checks that a client pinning its fingerprint connects while
// one pinning a different fingerprint is refused.
func TestTLSPinning(t *testing.T) {
	e := newEnv(t)
	f := secrets.Files{Dir: t.TempDir()}
	if _, err := f.Ensure([]string{"127.0.0.1", "localhost"}); err != nil {
		t.Fatal(err)
	}
	cert, fp, err := f.LoadCert()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(e.h)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	defer ts.Close()

	client := func(pin string) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // replaced by the pin check below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || secrets.Fingerprint(raw[0]) != pin {
					return errors.New("fingerprint mismatch")
				}
				return nil
			},
		}}}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client(fp).Do(req)
	if err != nil {
		t.Fatalf("pinned client: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pinned client status %d", resp.StatusCode)
	}
	if _, err := client(strings.Repeat("00:", 31) + "00").Do(req); err == nil {
		t.Fatal("client with wrong pin connected")
	}
	// A client that trusts only the system roots refuses the self-signed cert.
	if _, err := http.Get(ts.URL + "/healthz"); err == nil {
		t.Fatal("unpinned default client accepted the self-signed certificate")
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
