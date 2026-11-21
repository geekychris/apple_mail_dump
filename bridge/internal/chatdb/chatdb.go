// Package chatdb reads Messages' chat.db (iMessage, SMS, RCS). The query
// shapes follow geekychris/message_mcp's ingester; optional columns are
// discovered at run time because the schema drifts between macOS releases.
package chatdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Chat is one conversation.
type Chat struct {
	ID           int64      `json:"id"`
	GUID         string     `json:"guid"`
	Identifier   string     `json:"identifier"`
	DisplayName  string     `json:"display_name,omitempty"`
	Service      string     `json:"service,omitempty"`
	Participants []string   `json:"participants"`
	MessageCount int64      `json:"message_count"`
	LastMessage  *time.Time `json:"last_message_at,omitempty"`
}

// Attachment is metadata only; files are never served.
type Attachment struct {
	Filename string `json:"filename,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size"`
}

// Message is one row of the message table with its decoded text.
type Message struct {
	ID             int64        `json:"id"`
	GUID           string       `json:"guid"`
	ChatID         int64        `json:"chat_id,omitempty"`
	ChatIdentifier string       `json:"chat_identifier,omitempty"`
	Date           *time.Time   `json:"date,omitempty"`
	DateRead       *time.Time   `json:"date_read,omitempty"`
	IsFromMe       bool         `json:"is_from_me"`
	Sender         string       `json:"sender,omitempty"`
	Service        string       `json:"service,omitempty"`
	Text           string       `json:"text"`
	TextSource     string       `json:"text_source"` // text | attributedBody | none
	AssociatedType int64        `json:"associated_type"`
	ItemType       int64        `json:"item_type"`
	ThreadOrigin   string       `json:"thread_originator_guid,omitempty"`
	Attachments    []Attachment `json:"attachments"`
}

// Store queries one chat.db handle.
type Store struct {
	cols map[string]map[string]bool
}

// DiscoverSchema reads column lists.
func DiscoverSchema(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{cols: map[string]map[string]bool{}}
	for _, t := range []string{"message", "chat", "handle", "chat_message_join", "chat_handle_join", "attachment", "message_attachment_join"} {
		rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return nil, err
			}
			set[strings.ToLower(n)] = true
		}
		rows.Close()
		s.cols[t] = set
	}
	if len(s.cols["message"]) == 0 {
		return nil, fmt.Errorf("not a chat.db: no message table")
	}
	return s, nil
}

func (s *Store) col(table, alias, col string) string {
	if s.cols[table][strings.ToLower(col)] {
		return alias + "." + col
	}
	return "NULL"
}

// Chats lists conversations, most recently active first.
func (s *Store) Chats(ctx context.Context, db *sql.DB, limit int) ([]Chat, error) {
	q := fmt.Sprintf(`SELECT c.ROWID, %s, %s, %s, %s,
		(SELECT count(*) FROM chat_message_join j WHERE j.chat_id = c.ROWID),
		(SELECT max(m.date) FROM chat_message_join j JOIN message m ON m.ROWID = j.message_id WHERE j.chat_id = c.ROWID) AS last
		FROM chat c ORDER BY last DESC NULLS LAST LIMIT ?`,
		s.col("chat", "c", "guid"), s.col("chat", "c", "chat_identifier"),
		s.col("chat", "c", "display_name"), s.col("chat", "c", "service_name"))
	rows, err := db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	var out []Chat
	for rows.Next() {
		var (
			c                      Chat
			guid, ident, name, svc sql.NullString
			last                   sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &guid, &ident, &name, &svc, &c.MessageCount, &last); err != nil {
			rows.Close()
			return nil, err
		}
		c.GUID, c.Identifier, c.DisplayName, c.Service = guid.String, ident.String, name.String, svc.String
		c.LastMessage = appleTime(last)
		c.Participants = []string{}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	idx := map[int64]int{}
	ph := make([]string, len(out))
	args := make([]any, len(out))
	for i, c := range out {
		idx[c.ID], ph[i], args[i] = i, "?", c.ID
	}
	prow, err := db.QueryContext(ctx, `SELECT chj.chat_id, h.id FROM chat_handle_join chj
		JOIN handle h ON h.ROWID = chj.handle_id WHERE chj.chat_id IN (`+strings.Join(ph, ",")+`) ORDER BY chj.chat_id, h.id`, args...)
	if err != nil {
		return nil, err
	}
	defer prow.Close()
	for prow.Next() {
		var cid int64
		var h sql.NullString
		if err := prow.Scan(&cid, &h); err != nil {
			return nil, err
		}
		if i, ok := idx[cid]; ok && h.Valid {
			out[i].Participants = append(out[i].Participants, h.String)
		}
	}
	return out, prow.Err()
}

// Messages returns rows with ROWID > since, oldest first. chat (0 = all)
// restricts to one conversation; after (unix seconds, 0 = none) bounds by date.
func (s *Store) Messages(ctx context.Context, db *sql.DB, since, chat, after int64, limit int) ([]Message, error) {
	q := fmt.Sprintf(`SELECT m.ROWID, %s, cmj.chat_id, c.chat_identifier, %s, %s, %s, h.id, %s, %s, %s, %s, %s, %s, %s
		FROM message m
		LEFT JOIN handle h ON h.ROWID = m.handle_id
		LEFT JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
		LEFT JOIN chat c ON c.ROWID = cmj.chat_id
		WHERE m.ROWID > ?`,
		s.col("message", "m", "guid"), s.col("message", "m", "date"), s.col("message", "m", "date_read"),
		s.col("message", "m", "is_from_me"), s.col("message", "m", "service"),
		s.col("message", "m", "text"), s.col("message", "m", "attributedBody"),
		s.col("message", "m", "associated_message_type"), s.col("message", "m", "item_type"),
		s.col("message", "m", "thread_originator_guid"), s.col("message", "m", "cache_has_attachments"))
	args := []any{since}
	if chat > 0 {
		q += " AND cmj.chat_id = ?"
		args = append(args, chat)
	}
	if after > 0 && s.cols["message"]["date"] {
		// chat.db dates are nanoseconds since 2001-01-01 (seconds on very old
		// databases); compare in both units.
		cocoa := after - cocoaEpochOffset
		q += " AND (m.date >= ? OR (m.date < 100000000000 AND m.date >= ?))"
		args = append(args, cocoa*1_000_000_000, cocoa)
	}
	q += " GROUP BY m.ROWID ORDER BY m.ROWID ASC LIMIT ?"
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []Message
	withAtt := []int{}
	for rows.Next() {
		var (
			m                                   Message
			guid, ident, sender, svc, text, thr sql.NullString
			chatID, date, dateRead, fromMe      sql.NullInt64
			assoc, item, hasAtt                 sql.NullInt64
			attr                                []byte
		)
		if err := rows.Scan(&m.ID, &guid, &chatID, &ident, &date, &dateRead, &fromMe, &sender, &svc,
			&text, &attr, &assoc, &item, &thr, &hasAtt); err != nil {
			rows.Close()
			return nil, err
		}
		m.GUID, m.ChatID, m.ChatIdentifier = guid.String, chatID.Int64, ident.String
		m.Date, m.DateRead = appleTime(date), appleTime(dateRead)
		m.IsFromMe = fromMe.Int64 != 0
		if !m.IsFromMe {
			m.Sender = sender.String
		}
		m.Service = svc.String
		m.AssociatedType, m.ItemType, m.ThreadOrigin = assoc.Int64, item.Int64, thr.String
		m.TextSource = "none"
		switch {
		case text.Valid && text.String != "":
			m.Text, m.TextSource = text.String, "text"
		case len(attr) > 0:
			if t := decodeAttributedBody(attr); t != "" {
				m.Text, m.TextSource = t, "attributedBody"
			}
		}
		m.Attachments = []Attachment{}
		if hasAtt.Int64 != 0 {
			withAtt = append(withAtt, len(out))
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(withAtt) > 0 && len(s.cols["attachment"]) > 0 {
		if err := s.attachments(ctx, db, out, withAtt); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) attachments(ctx context.Context, db *sql.DB, ms []Message, which []int) error {
	idx := map[int64]int{}
	ph := make([]string, len(which))
	args := make([]any, len(which))
	for i, w := range which {
		idx[ms[w].ID], ph[i], args[i] = w, "?", ms[w].ID
	}
	q := fmt.Sprintf(`SELECT maj.message_id, %s, %s, %s FROM message_attachment_join maj
		JOIN attachment a ON a.ROWID = maj.attachment_id WHERE maj.message_id IN (%s)`,
		s.col("attachment", "a", "filename"), s.col("attachment", "a", "mime_type"),
		s.col("attachment", "a", "total_bytes"), strings.Join(ph, ","))
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var mid int64
		var fn, mt sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&mid, &fn, &mt, &size); err != nil {
			return err
		}
		if i, ok := idx[mid]; ok {
			a := Attachment{MimeType: mt.String, Size: size.Int64}
			if fn.String != "" {
				a.Filename = filepath.Base(fn.String) // never expose the on-disk path
			}
			ms[i].Attachments = append(ms[i].Attachments, a)
		}
	}
	return rows.Err()
}

// Counts for /v1/status.
type Counts struct {
	Messages int64      `json:"messages"`
	Chats    int64      `json:"chats"`
	MaxID    int64      `json:"max_id"`
	Newest   *time.Time `json:"newest,omitempty"`
}

func (s *Store) Counts(ctx context.Context, db *sql.DB) (Counts, error) {
	var c Counts
	var newest sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(ROWID),0), max(`+s.col("message", "message", "date")+`) FROM message`).
		Scan(&c.Messages, &c.MaxID, &newest); err != nil {
		return c, err
	}
	c.Newest = appleTime(newest)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM chat`).Scan(&c.Chats)
	return c, nil
}

const cocoaEpochOffset = 978307200

// appleTime converts a chat.db timestamp (ns since 2001, or s on old DBs).
func appleTime(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 <= 0 {
		return nil
	}
	var t time.Time
	if v.Int64 > 100_000_000_000 {
		t = time.Unix(cocoaEpochOffset+v.Int64/1_000_000_000, v.Int64%1_000_000_000)
	} else {
		t = time.Unix(cocoaEpochOffset+v.Int64, 0)
	}
	t = t.UTC()
	return &t
}
