// Package fixtures builds synthetic Envelope Index and chat.db databases and
// .emlx files for tests. Everything here is invented; no real mail or
// messages are used anywhere in the test suite.
package fixtures

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Mail is a synthetic ~/Library/Mail tree.
type Mail struct {
	Root     string // the "~/Library/Mail" directory
	Envelope string // path of the Envelope Index
	DB       *sql.DB
}

const envelopeSchema = `
PRAGMA journal_mode=WAL;
PRAGMA wal_autocheckpoint=0;
CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT);
CREATE TABLE summaries (ROWID INTEGER PRIMARY KEY, summary TEXT);
CREATE TABLE message_global_data (ROWID INTEGER PRIMARY KEY, message_id INTEGER, message_id_header TEXT);
CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, message_id INTEGER, global_message_id INTEGER, remote_id TEXT,
  sender INTEGER, subject INTEGER, summary INTEGER, date_sent INTEGER, date_received INTEGER, mailbox INTEGER,
  flags INTEGER, read INTEGER, flagged INTEGER, deleted INTEGER, conversation_id INTEGER, size INTEGER);
CREATE TABLE recipients (ROWID INTEGER PRIMARY KEY, message INTEGER, address INTEGER, type INTEGER, position INTEGER);
INSERT INTO mailboxes VALUES (1, 'imap://11111111-2222-3333-4444-555555555555/INBOX');
INSERT INTO addresses VALUES (1, 'alice@example.com', 'Alice Example'), (2, 'bob@example.org', 'Bob'), (3, 'carol@example.net', '');
`

// NewMail creates a Mail tree with an empty Envelope Index. The returned DB
// keeps the database open with auto-checkpointing off, so rows inserted
// through it stay in the -wal file (as they do while Mail is running).
func NewMail(t testing.TB) *Mail {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Mail")
	dataDir := filepath.Join(root, "V10", "MailData")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An older version directory that must be ignored.
	_ = os.MkdirAll(filepath.Join(root, "V9", "MailData"), 0o700)
	p := filepath.Join(dataDir, "Envelope Index")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(envelopeSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Mail{Root: root, Envelope: p, DB: db}
}

// AddMessage inserts message id received at unix time recv, from alice to bob
// with carol on cc, and returns it.
func (m *Mail) AddMessage(t testing.TB, id int64, recv int64, subject string) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := m.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO subjects (ROWID, subject) VALUES (?, ?)`, id, subject)
	exec(`INSERT INTO summaries (ROWID, summary) VALUES (?, ?)`, id, "summary of "+subject)
	exec(`INSERT INTO message_global_data (ROWID, message_id, message_id_header) VALUES (?, ?, ?)`, id, id*7, fmt.Sprintf("<msg-%d@example.com>", id))
	exec(`INSERT INTO messages VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, 1, ?, ?, ?, 0, ?, ?)`,
		id, id*7, id, fmt.Sprintf("%d", 1000+id), id, id, recv-60, recv, int64(1|1<<2), 1, id%2, 42, 1234+id)
	exec(`INSERT INTO recipients (message, address, type, position) VALUES (?, 2, 0, 0), (?, 3, 1, 0)`, id, id)
}

// WriteEmlx writes message id as an .emlx (or .partial.emlx) file in the
// sharded layout Mail uses.
func (m *Mail) WriteEmlx(t testing.TB, id int64, raw string, partial bool) string {
	t.Helper()
	dir := filepath.Join(m.Root, "V10", "11111111-2222-3333-4444-555555555555", "INBOX.mbox",
		"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", "Data", fmt.Sprint(id%10), "Messages")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%d.emlx", id)
	if partial {
		name = fmt.Sprintf("%d.partial.emlx", id)
	}
	p := filepath.Join(dir, name)
	content := fmt.Sprintf("%d\n%s<?xml version=\"1.0\"?><plist><dict><key>flags</key><integer>8590195713</integer></dict></plist>", len(raw), raw)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// SampleMIME is a multipart/mixed message with text, HTML and one attachment.
const SampleMIME = "From: Alice Example <alice@example.com>\r\n" +
	"To: Bob <bob@example.org>\r\n" +
	"Subject: =?UTF-8?B?SGVsbG8g4pyT?=\r\n" +
	"Message-ID: <msg-1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=iso-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Caf=E9 at noon.\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Caf\xc3\xa9 at noon.</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"agenda.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"agenda.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJcfsj6IK\r\n" +
	"--outer--\r\n"

// Chat is a synthetic chat.db.
type Chat struct {
	Path string
	DB   *sql.DB
}

const chatSchema = `
PRAGMA journal_mode=WAL;
PRAGMA wal_autocheckpoint=0;
CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT, service TEXT);
CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, chat_identifier TEXT, display_name TEXT, service_name TEXT);
CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, text TEXT, attributedBody BLOB, handle_id INTEGER,
  service TEXT, date INTEGER, date_read INTEGER, is_from_me INTEGER, associated_message_type INTEGER DEFAULT 0,
  item_type INTEGER DEFAULT 0, thread_originator_guid TEXT, cache_has_attachments INTEGER DEFAULT 0, is_empty INTEGER DEFAULT 0);
CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, guid TEXT, filename TEXT, mime_type TEXT, total_bytes INTEGER);
CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
INSERT INTO handle VALUES (1, '+15550000001', 'iMessage'), (2, 'dave@example.com', 'iMessage'), (3, '+15550000003', 'SMS');
INSERT INTO chat VALUES (1, 'iMessage;-;+15550000001', '+15550000001', '', 'iMessage'),
                        (2, 'iMessage;+;chat123', 'chat123', 'Test Group', 'iMessage');
INSERT INTO chat_handle_join VALUES (1, 1), (2, 2), (2, 3);
`

// NewChat creates an empty synthetic chat.db.
func NewChat(t testing.TB) *Chat {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Messages")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "chat.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(chatSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Chat{Path: p, DB: db}
}

// CocoaNanos converts unix seconds to chat.db's nanoseconds since 2001.
func CocoaNanos(unix int64) int64 { return (unix - 978307200) * 1_000_000_000 }

// AddMessage inserts a message into chat chatID. Either text or attributed
// (an attributedBody blob) carries the body.
func (c *Chat) AddMessage(t testing.TB, id, chatID, handle int64, unix int64, text string, attributed []byte, fromMe bool) {
	t.Helper()
	var txt any
	if text != "" {
		txt = text
	}
	me := 0
	if fromMe {
		me = 1
	}
	if _, err := c.DB.Exec(`INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, service, date, date_read, is_from_me)
		VALUES (?, ?, ?, ?, ?, 'iMessage', ?, 0, ?)`, id, fmt.Sprintf("GUID-%d", id), txt, attributed, handle, CocoaNanos(unix), me); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Exec(`INSERT INTO chat_message_join VALUES (?, ?)`, chatID, id); err != nil {
		t.Fatal(err)
	}
}

// AddAttachment links a synthetic attachment to message id.
func (c *Chat) AddAttachment(t testing.TB, msgID, attID int64, filename, mime string, size int64) {
	t.Helper()
	if _, err := c.DB.Exec(`INSERT INTO attachment VALUES (?, ?, ?, ?, ?)`, attID, fmt.Sprintf("ATT-%d", attID), filename, mime, size); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Exec(`INSERT INTO message_attachment_join VALUES (?, ?)`, msgID, attID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Exec(`UPDATE message SET cache_has_attachments = 1 WHERE ROWID = ?`, msgID); err != nil {
		t.Fatal(err)
	}
}

// AttributedBody builds a minimal typedstream NSAttributedString blob that
// carries text, in the layout Messages writes.
func AttributedBody(text string) []byte {
	b := []byte{0x04, 0x0B}
	b = append(b, "streamtyped"...)
	b = append(b, 0x81, 0xE8, 0x03, 0x84, 0x01, 0x40, 0x84, 0x84, 0x84, 0x12)
	b = append(b, "NSAttributedString"...)
	b = append(b, 0x00, 0x84, 0x84, 0x08)
	b = append(b, "NSObject"...)
	b = append(b, 0x00, 0x85, 0x92, 0x84, 0x84, 0x84, 0x08)
	b = append(b, "NSString"...)
	b = append(b, 0x01, 0x94, 0x84, 0x01, 0x2B)
	n := len(text)
	if n < 0x80 {
		b = append(b, byte(n))
	} else {
		b = append(b, 0x81, byte(n), byte(n>>8))
	}
	b = append(b, text...)
	b = append(b, 0x86, 0x84, 0x02, 0x69, 0x49, 0x01)
	b = append(b, 0x92, 0x84, 0x84, 0x84, 0x0C)
	b = append(b, "NSDictionary"...)
	b = append(b, 0x00, 0x94, 0x84, 0x01, 0x69, 0x01, 0x92, 0x84, 0x96, 0x96, 0x1D)
	b = append(b, "__kIMMessagePartAttributeName"...)
	b = append(b, 0x86, 0x86)
	return b
}
