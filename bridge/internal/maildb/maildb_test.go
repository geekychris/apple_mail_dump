package maildb_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geekychris/apple_mail_dump/bridge/internal/fixtures"
	"github.com/geekychris/apple_mail_dump/bridge/internal/maildb"
	"github.com/geekychris/apple_mail_dump/bridge/internal/snapshot"
)

func TestFindEnvelopeIndexPicksNewestVersion(t *testing.T) {
	m := fixtures.NewMail(t)
	p, err := maildb.FindEnvelopeIndex(m.Root)
	if err != nil || p != m.Envelope {
		t.Fatalf("got %q %v", p, err)
	}
	if _, err := maildb.FindEnvelopeIndex(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestListWatermarkAndRecipients(t *testing.T) {
	m := fixtures.NewMail(t)
	now := time.Now().Unix()
	m.AddMessage(t, 1, now-30*86400, "old")
	m.AddMessage(t, 2, now-3*86400, "recent")
	m.AddMessage(t, 5, now-86400, "newest")
	s := snapshot.New(m.Envelope, t.TempDir(), 0)
	defer s.Close()
	db, err := s.DB()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := maildb.DiscoverSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.List(ctx, db, 0, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != 1 || page[1].ID != 2 {
		t.Fatalf("page ids: %+v", page)
	}
	g := page[0]
	if g.MessageID != "msg-1@example.com" || g.Subject != "old" || g.From == nil || g.From.Address != "alice@example.com" ||
		g.From.Name != "Alice Example" || len(g.To) != 1 || g.To[0].Address != "bob@example.org" || len(g.Cc) != 1 ||
		g.Mailbox == "" || !g.Flags.Read || !g.Flags.Answered || g.Size != 1235 || g.Snippet != "summary of old" {
		t.Fatalf("meta mapping: %+v", g)
	}
	if g.DateReceived == nil || g.DateReceived.Unix() != now-30*86400 {
		t.Fatalf("date: %v", g.DateReceived)
	}
	page, _ = st.List(ctx, db, 2, 0, 10)
	if len(page) != 1 || page[0].ID != 5 {
		t.Fatalf("after watermark 2: %+v", page)
	}
	page, _ = st.List(ctx, db, 0, now-7*86400, 10)
	if len(page) != 2 || page[0].ID != 2 {
		t.Fatalf("received_after filter: %d rows", len(page))
	}
	one, err := st.Get(ctx, db, 5)
	if err != nil || one == nil || one.Subject != "newest" {
		t.Fatalf("get: %+v %v", one, err)
	}
	if none, _ := st.Get(ctx, db, 99); none != nil {
		t.Fatal("get missing should be nil")
	}
	c, err := st.Counts(ctx, db)
	if err != nil || c.Messages != 3 || c.MaxID != 5 || c.Mailboxes != 1 || c.Newest == nil {
		t.Fatalf("counts %+v %v", c, err)
	}
}

func TestEmlxIndexAndParse(t *testing.T) {
	m := fixtures.NewMail(t)
	full := m.WriteEmlx(t, 1, fixtures.SampleMIME, false)
	m.WriteEmlx(t, 1, fixtures.SampleMIME, true) // a partial for the same id must lose
	partial := m.WriteEmlx(t, 22, fixtures.SampleMIME, true)
	x := maildb.NewEmlxIndex(m.Root, time.Hour)
	if p, err := x.Lookup(1); err != nil || p != full {
		t.Fatalf("lookup 1: %q %v", p, err)
	}
	if p, _ := x.Lookup(22); p != partial {
		t.Fatalf("lookup 22: %q", p)
	}
	if p, _ := x.Lookup(404); p != "" {
		t.Fatalf("lookup missing: %q", p)
	}
	raw, isPartial, err := maildb.ReadRaw(full)
	if err != nil || isPartial || string(raw) != fixtures.SampleMIME {
		t.Fatalf("raw: partial=%v err=%v equal=%v", isPartial, err, string(raw) == fixtures.SampleMIME)
	}
	p, err := maildb.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Headers["Subject"][0] != "Hello ✓" {
		t.Fatalf("subject decode: %q", p.Headers["Subject"])
	}
	if strings.TrimSpace(p.Text) != "Café at noon." {
		t.Fatalf("text (latin-1 + QP): %q", p.Text)
	}
	if !strings.Contains(p.HTML, "Café") {
		t.Fatalf("html: %q", p.HTML)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "agenda.pdf" || p.Attachments[0].ContentType != "application/pdf" || p.Attachments[0].Size != 15 {
		t.Fatalf("attachments: %+v", p.Attachments)
	}
}
