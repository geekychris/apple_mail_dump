// Package maildb reads Apple Mail's "Envelope Index" SQLite database and the
// .emlx files beside it. The schema is undocumented and changes between macOS
// releases, so the optional columns are discovered at run time and missing
// ones come back empty instead of failing the query.
package maildb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Address is one sender or recipient.
type Address struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

// Flags are the per-message state bits Mail keeps in the index.
type Flags struct {
	Read     bool  `json:"read"`
	Flagged  bool  `json:"flagged"`
	Deleted  bool  `json:"deleted"`
	Answered bool  `json:"answered"`
	Raw      int64 `json:"raw"`
}

// Meta is the metadata row the list endpoint returns.
type Meta struct {
	ID             int64      `json:"id"`
	MessageID      string     `json:"message_id,omitempty"`
	RemoteID       string     `json:"remote_id,omitempty"`
	DateReceived   *time.Time `json:"date_received,omitempty"`
	DateSent       *time.Time `json:"date_sent,omitempty"`
	From           *Address   `json:"from,omitempty"`
	To             []Address  `json:"to"`
	Cc             []Address  `json:"cc"`
	Subject        string     `json:"subject"`
	Snippet        string     `json:"snippet,omitempty"`
	Mailbox        string     `json:"mailbox,omitempty"`
	Flags          Flags      `json:"flags"`
	ConversationID int64      `json:"conversation_id,omitempty"`
	Size           int64      `json:"size"`
}

// Store queries one Envelope Index database handle.
type Store struct {
	cols map[string]map[string]bool // table -> column set
}

// DiscoverSchema reads the column lists of the tables the queries touch.
func DiscoverSchema(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{cols: map[string]map[string]bool{}}
	for _, t := range []string{"messages", "subjects", "addresses", "recipients", "mailboxes", "summaries", "message_global_data"} {
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
	if len(s.cols["messages"]) == 0 {
		return nil, fmt.Errorf("not an Envelope Index: no messages table")
	}
	return s, nil
}

func (s *Store) has(table, col string) bool { return s.cols[table][col] }

func (s *Store) col(table, alias, col string) string {
	if s.has(table, col) {
		return alias + "." + col
	}
	return "NULL"
}

// Query selects the metadata of messages with ROWID > since, oldest first.
// receivedAfter (unix seconds, 0 = no bound) limits to recent mail, which is
// how a consumer takes a bounded first batch.
func (s *Store) List(ctx context.Context, db *sql.DB, since int64, receivedAfter int64, limit int) ([]Meta, error) {
	q, args := s.selectSQL()
	where := " WHERE m.ROWID > ?"
	args = append(args, since)
	if receivedAfter > 0 && s.has("messages", "date_received") {
		where += " AND m.date_received >= ?"
		args = append(args, receivedAfter)
	}
	q += where + " ORDER BY m.ROWID ASC LIMIT ?"
	args = append(args, limit)
	out, err := s.scan(ctx, db, q, args...)
	if err != nil {
		return nil, err
	}
	if err := s.attachRecipients(ctx, db, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns one message's metadata, or nil if there is no such row.
func (s *Store) Get(ctx context.Context, db *sql.DB, id int64) (*Meta, error) {
	q, args := s.selectSQL()
	q += " WHERE m.ROWID = ?"
	out, err := s.scan(ctx, db, q, append(args, id)...)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	if err := s.attachRecipients(ctx, db, out); err != nil {
		return nil, err
	}
	return &out[0], nil
}

// Counts reports totals for /v1/status.
type Counts struct {
	Messages  int64      `json:"messages"`
	Mailboxes int64      `json:"mailboxes"`
	MaxID     int64      `json:"max_id"`
	Newest    *time.Time `json:"newest_received,omitempty"`
}

func (s *Store) Counts(ctx context.Context, db *sql.DB) (Counts, error) {
	var c Counts
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(ROWID),0) FROM messages`).Scan(&c.Messages, &c.MaxID); err != nil {
		return c, err
	}
	if len(s.cols["mailboxes"]) > 0 {
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes`).Scan(&c.Mailboxes)
	}
	if s.has("messages", "date_received") {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT max(date_received) FROM messages`).Scan(&v); err == nil {
			c.Newest = mailTime(v)
		}
	}
	return c, nil
}

func (s *Store) selectSQL() (string, []any) {
	msgID := "NULL"
	join := ""
	if s.has("messages", "global_message_id") && s.has("message_global_data", "message_id_header") {
		msgID = "g.message_id_header"
		join += " LEFT JOIN message_global_data g ON g.ROWID = m.global_message_id"
	} else if s.has("messages", "message_id_header") {
		msgID = "m.message_id_header"
	}
	subj := "NULL"
	if s.has("messages", "subject") && len(s.cols["subjects"]) > 0 {
		subj = "subj.subject"
		join += " LEFT JOIN subjects subj ON subj.ROWID = m.subject"
	}
	from, fromName := "NULL", "NULL"
	if s.has("messages", "sender") && len(s.cols["addresses"]) > 0 {
		from, fromName = "addr.address", s.col("addresses", "addr", "comment")
		join += " LEFT JOIN addresses addr ON addr.ROWID = m.sender"
	}
	summ := "NULL"
	if s.has("messages", "summary") && s.has("summaries", "summary") {
		summ = "summ.summary"
		join += " LEFT JOIN summaries summ ON summ.ROWID = m.summary"
	}
	mbox := "NULL"
	if s.has("messages", "mailbox") && s.has("mailboxes", "url") {
		mbox = "mb.url"
		join += " LEFT JOIN mailboxes mb ON mb.ROWID = m.mailbox"
	}
	q := fmt.Sprintf(`SELECT m.ROWID, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM messages m%s`,
		msgID,
		s.col("messages", "m", "remote_id"),
		s.col("messages", "m", "date_received"),
		s.col("messages", "m", "date_sent"),
		from, fromName, subj, summ, mbox,
		s.col("messages", "m", "flags"),
		s.col("messages", "m", "read"),
		s.col("messages", "m", "flagged"),
		s.col("messages", "m", "deleted"),
		s.col("messages", "m", "conversation_id"),
		s.col("messages", "m", "size"),
		join)
	return q, nil
}

func (s *Store) scan(ctx context.Context, db *sql.DB, q string, args ...any) ([]Meta, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Meta
	for rows.Next() {
		var (
			m                                     Meta
			msgID, remote, from, fromName         sql.NullString
			subj, summ, mbox                      sql.NullString
			recv, sent, flags, read, flagged, del sql.NullInt64
			conv, size                            sql.NullInt64
		)
		if err := rows.Scan(&m.ID, &msgID, &remote, &recv, &sent, &from, &fromName, &subj, &summ, &mbox,
			&flags, &read, &flagged, &del, &conv, &size); err != nil {
			return nil, err
		}
		m.MessageID = strings.Trim(msgID.String, "<> ")
		m.RemoteID = remote.String
		m.DateReceived = mailTime(recv)
		m.DateSent = mailTime(sent)
		if from.Valid && from.String != "" {
			m.From = &Address{Address: from.String, Name: fromName.String}
		}
		m.Subject = subj.String
		m.Snippet = summ.String
		m.Mailbox = mbox.String
		m.Flags = Flags{Raw: flags.Int64, Read: read.Int64 != 0, Flagged: flagged.Int64 != 0, Deleted: del.Int64 != 0}
		if !read.Valid {
			m.Flags.Read = flags.Int64&1 != 0
		}
		m.Flags.Answered = flags.Int64&(1<<2) != 0
		m.ConversationID = conv.Int64
		m.Size = size.Int64
		m.To, m.Cc = []Address{}, []Address{}
		out = append(out, m)
	}
	return out, rows.Err()
}

// attachRecipients fills To/Cc in one query for the whole batch.
func (s *Store) attachRecipients(ctx context.Context, db *sql.DB, ms []Meta) error {
	if len(ms) == 0 || !s.has("recipients", "message") || !s.has("recipients", "address") {
		return nil
	}
	idx := make(map[int64]int, len(ms))
	ph := make([]string, len(ms))
	args := make([]any, len(ms))
	for i, m := range ms {
		idx[m.ID] = i
		ph[i] = "?"
		args[i] = m.ID
	}
	typ, pos := s.col("recipients", "r", "type"), s.col("recipients", "r", "position")
	q := fmt.Sprintf(`SELECT r.message, %s, a.address, %s FROM recipients r
		JOIN addresses a ON a.ROWID = r.address
		WHERE r.message IN (%s) ORDER BY r.message, %s, %s`,
		typ, s.col("addresses", "a", "comment"), strings.Join(ph, ","), typ, pos)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			mid       int64
			t         sql.NullInt64
			addr, cmt sql.NullString
		)
		if err := rows.Scan(&mid, &t, &addr, &cmt); err != nil {
			return err
		}
		i, ok := idx[mid]
		if !ok || !addr.Valid {
			continue
		}
		a := Address{Address: addr.String, Name: cmt.String}
		switch t.Int64 {
		case 1:
			ms[i].Cc = append(ms[i].Cc, a)
		case 2:
			// bcc: only ever present on mail you sent; reported as cc-less to
			// keep the shape small.
		default:
			ms[i].To = append(ms[i].To, a)
		}
	}
	return rows.Err()
}

// mailTime converts an Envelope Index timestamp. Mail stores Unix seconds;
// a value below 1e9 would be a Cocoa (2001-based) time from an older schema.
func mailTime(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 <= 0 {
		return nil
	}
	sec := v.Int64
	if sec < 1_000_000_000 {
		sec += 978307200
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

// ErrNotSetUp means the Mail directory is readable but holds no Envelope
// Index: Mail.app has never been set up with an account on this Mac (or not
// since an old macOS, e.g. only an empty V8/MailData is left).
var ErrNotSetUp = errors.New("Mail.app not set up: no V*/MailData/Envelope Index")

// FindEnvelopeIndex returns the newest ~/Library/Mail/V*/MailData/Envelope Index.
func FindEnvelopeIndex(mailRoot string) (string, error) {
	vs, err := filepath.Glob(filepath.Join(mailRoot, "V*"))
	if err != nil {
		return "", err
	}
	sort.Slice(vs, func(i, j int) bool { return versionNum(vs[i]) > versionNum(vs[j]) })
	for _, v := range vs {
		p := filepath.Join(v, "MailData", "Envelope Index")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if errors.Is(err, os.ErrPermission) {
			return "", err
		}
	}
	// Glob hides EPERM; probe the root so callers can tell "no FDA" from
	// "Mail never set up".
	if _, err := os.ReadDir(mailRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return "", ErrNotSetUp
}

func versionNum(p string) int {
	n := 0
	fmt.Sscanf(strings.TrimPrefix(filepath.Base(p), "V"), "%d", &n)
	return n
}
