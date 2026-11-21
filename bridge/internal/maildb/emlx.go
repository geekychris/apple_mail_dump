package maildb

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

// EmlxIndex maps message ROWIDs to their .emlx / .partial.emlx files. Mail
// shards the files under deep per-mailbox directories, so one walk builds the
// whole map (apple_mail_dump's per-message recursive glob was O(n²)). The map
// is rebuilt on a miss, at most once per MinRebuild.
type EmlxIndex struct {
	Root       string // ~/Library/Mail
	MinRebuild time.Duration

	mu    sync.Mutex
	paths map[int64]string
	built time.Time
}

// NewEmlxIndex returns an index rooted at mailRoot.
func NewEmlxIndex(mailRoot string, minRebuild time.Duration) *EmlxIndex {
	return &EmlxIndex{Root: mailRoot, MinRebuild: minRebuild}
}

// Lookup returns the file for id, or "" if none is on disk.
func (x *EmlxIndex) Lookup(id int64) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if p, ok := x.paths[id]; ok {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if x.paths != nil && time.Since(x.built) < x.MinRebuild {
		return "", nil
	}
	if err := x.rebuild(); err != nil {
		return "", err
	}
	return x.paths[id], nil
}

// Size is the number of files indexed by the last walk.
func (x *EmlxIndex) Size() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.paths)
}

func (x *EmlxIndex) rebuild() error {
	paths := map[int64]string{}
	err := filepath.WalkDir(x.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == x.Root {
				return err
			}
			return nil // unreadable subtree: skip
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".emlx") {
			return nil
		}
		base := strings.TrimSuffix(name, ".emlx")
		partial := strings.HasSuffix(base, ".partial")
		base = strings.TrimSuffix(base, ".partial")
		id, err := strconv.ParseInt(base, 10, 64)
		if err != nil {
			return nil
		}
		// Prefer a full .emlx over a .partial.emlx for the same id.
		if old, ok := paths[id]; ok && !strings.HasSuffix(old, ".partial.emlx") && partial {
			return nil
		}
		paths[id] = p
		return nil
	})
	if err != nil {
		return err
	}
	x.paths, x.built = paths, time.Now()
	return nil
}

// ReadRaw returns the RFC 822 bytes stored in an .emlx file: the first line is
// the byte count, then the message, then an XML plist of Mail's flags.
func ReadRaw(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, false, fmt.Errorf("emlx header: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 0 || n > 512<<20 {
		return nil, false, errors.New("emlx header: bad length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, false, fmt.Errorf("emlx body: %w", err)
	}
	return buf, strings.HasSuffix(path, ".partial.emlx"), nil
}

// Attachment describes one non-body MIME part. Content is never inlined.
type Attachment struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline"`
}

// Parsed is the detail endpoint's body.
type Parsed struct {
	Headers     map[string][]string `json:"headers"`
	Text        string              `json:"text"`
	HTML        string              `json:"html"`
	Attachments []Attachment        `json:"attachments"`
	// Partial is true when Mail stored the attachments outside the .emlx
	// (a .partial.emlx); their parts then appear without content.
	Partial bool `json:"partial"`
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

func charsetReader(label string, in io.Reader) (io.Reader, error) {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" || l == "utf-8" || l == "us-ascii" || l == "utf8" {
		return in, nil
	}
	enc, err := htmlindex.Get(l)
	if err != nil {
		return in, nil // unknown charset: pass bytes through
	}
	return enc.NewDecoder().Reader(in), nil
}

// DecodeHeader decodes RFC 2047 encoded words.
func DecodeHeader(v string) string {
	d, err := wordDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return d
}

// Parse splits a raw message into headers, text and HTML bodies and an
// attachment list.
func Parse(raw []byte) (*Parsed, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	p := &Parsed{Headers: map[string][]string{}, Attachments: []Attachment{}}
	for k, vs := range msg.Header {
		for _, v := range vs {
			p.Headers[k] = append(p.Headers[k], DecodeHeader(v))
		}
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil, err
	}
	walkPart(p, textproto(msg.Header), body, 0)
	return p, nil
}

type header interface{ Get(string) string }

type textproto mail.Header

func (h textproto) Get(k string) string { return mail.Header(h).Get(k) }

func walkPart(p *Parsed, h header, body []byte, depth int) {
	if depth > 20 {
		return
	}
	ctype := h.Get("Content-Type")
	if ctype == "" {
		ctype = "text/plain; charset=us-ascii"
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt, params = "text/plain", map[string]string{}
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := DecodeHeader(firstNonEmpty(dparams["filename"], params["name"]))

	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := mr.NextRawPart()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(part)
			walkPart(p, part.Header, b, depth+1)
		}
	}
	if mt == "message/rfc822" && disp != "attachment" {
		if inner, err := mail.ReadMessage(bytes.NewReader(decodeTransfer(h.Get("Content-Transfer-Encoding"), body))); err == nil {
			ib, _ := io.ReadAll(inner.Body)
			walkPart(p, textproto(inner.Header), ib, depth+1)
			return
		}
	}
	data := decodeTransfer(h.Get("Content-Transfer-Encoding"), body)
	isBody := disp != "attachment" && filename == ""
	switch {
	case isBody && mt == "text/plain":
		p.Text += toUTF8(params["charset"], data)
		return
	case isBody && mt == "text/html":
		p.HTML += toUTF8(params["charset"], data)
		return
	}
	p.Attachments = append(p.Attachments, Attachment{
		Filename:    filename,
		ContentType: mt,
		Size:        len(data),
		ContentID:   strings.Trim(h.Get("Content-Id"), "<>"),
		Inline:      disp == "inline",
	})
}

func decodeTransfer(enc string, b []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, b)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil && n == 0 {
			return b
		}
		return out[:n]
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		if err != nil && len(out) == 0 {
			return b
		}
		return out
	}
	return b
}

func toUTF8(charset string, b []byte) string {
	r, _ := charsetReader(charset, bytes.NewReader(b))
	out, err := io.ReadAll(r)
	if err != nil {
		return string(b)
	}
	return strings.ToValidUTF8(string(out), "�")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
