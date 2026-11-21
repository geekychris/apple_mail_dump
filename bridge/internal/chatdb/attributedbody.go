// Adapted from geekychris/message_mcp desktop/internal/ingest/chatdb/attributedbody.go.

package chatdb

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

// decodeAttributedBody extracts the plain-text message body from an Apple
// typedstream-encoded NSAttributedString blob (the `attributedBody` column in
// modern chat.db). Returns "" if nothing usable is found — callers should fall
// back to a placeholder body in that case.
//
// The typedstream format is Apple's binary NSKeyedArchiver-lite:
//
//	04 0B "streamtyped" <version bytes> <class hierarchy> <objects>
//
// Inside the encoded NSAttributedString the message text lives as a
// length-prefixed C-string (type code '+' = 0x2B) immediately after the
// NSString class declaration. Attribute names like
// "__kIMMessagePartAttributeName" appear later using a different marker
// sequence (0x96 0x96 <len>), so scanning for the FIRST `84 01 2B` occurrence
// reliably picks up the actual message text on modern iOS/macOS.
//
// This is a pragmatic subset — enough for the common case, not a full parser.
// A future slice can replace this with a proper typedstream walker (imessage-
// exporter has one in Rust that we can port).
func decodeAttributedBody(blob []byte) string {
	if len(blob) < 20 {
		return ""
	}
	// Require the typedstream header so we don't accidentally decode random
	// binary payloads.
	if !bytes.HasPrefix(blob, []byte{0x04, 0x0B}) {
		return ""
	}
	if !bytes.Contains(blob[:minInt(len(blob), 32)], []byte("streamtyped")) {
		return ""
	}

	marker := []byte{0x84, 0x01, 0x2B}
	// Look at every occurrence — the first "clean" string wins. This defeats
	// the rare blobs where a leading `+` prefix is used for something else.
	from := 0
	for {
		i := bytes.Index(blob[from:], marker)
		if i < 0 {
			return ""
		}
		start := from + i + len(marker)
		if start >= len(blob) {
			return ""
		}
		length, n := readTypedstreamLength(blob[start:])
		if n <= 0 || length <= 0 || start+n+length > len(blob) {
			from = start
			continue
		}
		s := string(blob[start+n : start+n+length])
		if utf8.ValidString(s) && !isTypedstreamAttrName(s) {
			return s
		}
		from = start + n + length
	}
}

// readTypedstreamLength decodes the length prefix used for `+`-typed strings.
// Length encoding:
//
//	byte <  0x81 → length is the byte itself
//	byte == 0x81 → next 2 bytes as little-endian uint16
//	byte == 0x82 → next 4 bytes as little-endian uint32
//	byte == 0x83 → next 8 bytes as little-endian uint64 (rare)
//
// Returns (length, bytesConsumed) — bytesConsumed is 0 on failure.
func readTypedstreamLength(b []byte) (int, int) {
	if len(b) == 0 {
		return 0, 0
	}
	switch b[0] {
	case 0x81:
		if len(b) < 3 {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint16(b[1:3])), 3
	case 0x82:
		if len(b) < 5 {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint32(b[1:5])), 5
	case 0x83:
		if len(b) < 9 {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint64(b[1:9])), 9
	default:
		if b[0] >= 0x80 {
			// 0x84..0x9F are class/object markers; not a length.
			return 0, 0
		}
		return int(b[0]), 1
	}
}

// isTypedstreamAttrName recognizes Apple's internal attribute-name strings
// that shouldn't be surfaced as message body. These all begin with "__kIM" and
// live later in the blob than the message text does — the FIRST `01 2B` we
// find should be the message text, but this guard catches the pathological
// case where a blob has an empty/absent text field and the first hit is an
// attribute key.
func isTypedstreamAttrName(s string) bool {
	if strings.HasPrefix(s, "__kIM") {
		return true
	}
	if strings.HasPrefix(s, "NSAttributedString") || strings.HasPrefix(s, "NSString") ||
		strings.HasPrefix(s, "NSDictionary") || strings.HasPrefix(s, "NSObject") ||
		strings.HasPrefix(s, "NSValue") || strings.HasPrefix(s, "NSNumber") {
		return true
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
