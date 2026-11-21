package chatdb_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/geekychris/apple_mail_dump/bridge/internal/chatdb"
	"github.com/geekychris/apple_mail_dump/bridge/internal/fixtures"
	"github.com/geekychris/apple_mail_dump/bridge/internal/snapshot"
)

func open(t *testing.T, c *fixtures.Chat) (*sql.DB, *chatdb.Store) {
	t.Helper()
	s := snapshot.New(c.Path, t.TempDir(), 0)
	t.Cleanup(func() { s.Close() })
	db, err := s.DB()
	if err != nil {
		t.Fatal(err)
	}
	st, err := chatdb.DiscoverSchema(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return db, st
}

func TestMessagesDecodeAndWatermark(t *testing.T) {
	c := fixtures.NewChat(t)
	long := strings.Repeat("synthetic long message ", 10)
	c.AddMessage(t, 1, 1, 1, 1_700_000_000, "plain text", nil, false)
	c.AddMessage(t, 2, 1, 1, 1_700_000_060, "", fixtures.AttributedBody("from attributed body"), true)
	c.AddMessage(t, 3, 2, 2, 1_700_000_120, "", fixtures.AttributedBody(long), false)
	c.AddMessage(t, 4, 2, 3, 1_700_000_180, "", nil, false)
	c.AddAttachment(t, 4, 1, "~/Library/Messages/Attachments/ab/12/UUID/photo.heic", "image/heic", 2048)
	db, st := open(t, c)
	ctx := context.Background()

	ms, err := st.Messages(ctx, db, 0, 0, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != 1 || ms[1].ID != 2 {
		t.Fatalf("first page ids wrong: %+v", ms)
	}
	if ms[0].Text != "plain text" || ms[0].TextSource != "text" || ms[0].Sender != "+15550000001" {
		t.Fatalf("row 1: %+v", ms[0])
	}
	if ms[1].Text != "from attributed body" || ms[1].TextSource != "attributedBody" || !ms[1].IsFromMe || ms[1].Sender != "" {
		t.Fatalf("row 2: %+v", ms[1])
	}
	if ms[0].Date == nil || ms[0].Date.Unix() != 1_700_000_000 {
		t.Fatalf("date conversion: %v", ms[0].Date)
	}

	ms, err = st.Messages(ctx, db, 2, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != 3 {
		t.Fatalf("second page: %d rows", len(ms))
	}
	if ms[0].Text != long {
		t.Fatal("0x81 length branch not decoded")
	}
	if ms[1].TextSource != "none" || len(ms[1].Attachments) != 1 {
		t.Fatalf("attachment row: %+v", ms[1])
	}
	if a := ms[1].Attachments[0]; a.Filename != "photo.heic" || a.MimeType != "image/heic" || a.Size != 2048 {
		t.Fatalf("attachment must expose basename only: %+v", a)
	}

	ms, _ = st.Messages(ctx, db, 0, 2, 0, 100)
	if len(ms) != 2 || ms[0].ChatID != 2 {
		t.Fatalf("chat filter: %d rows", len(ms))
	}
	ms, _ = st.Messages(ctx, db, 0, 0, 1_700_000_100, 100)
	if len(ms) != 2 || ms[0].ID != 3 {
		t.Fatalf("after filter: %d rows", len(ms))
	}
}

func TestChatsAndCounts(t *testing.T) {
	c := fixtures.NewChat(t)
	c.AddMessage(t, 1, 1, 1, 1_700_000_000, "a", nil, false)
	c.AddMessage(t, 2, 2, 2, 1_700_000_500, "b", nil, false)
	c.AddMessage(t, 3, 2, 3, 1_700_000_600, "c", nil, false)
	db, st := open(t, c)
	cs, err := st.Chats(context.Background(), db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].ID != 2 || cs[0].MessageCount != 2 || len(cs[0].Participants) != 2 || cs[0].DisplayName != "Test Group" {
		t.Fatalf("chats: %+v", cs)
	}
	n, err := st.Counts(context.Background(), db)
	if err != nil || n.Messages != 3 || n.Chats != 2 || n.MaxID != 3 {
		t.Fatalf("counts %+v %v", n, err)
	}
}
