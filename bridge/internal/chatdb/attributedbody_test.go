package chatdb

import (
	"testing"

	"github.com/geekychris/apple_mail_dump/bridge/internal/fixtures"
)

func TestDecodeAttributedBody(t *testing.T) {
	for _, s := range []string{"hi", "Ünïcödé ✓ text", string(make([]byte, 0)) + "x"} {
		if got := decodeAttributedBody(fixtures.AttributedBody(s)); got != s {
			t.Errorf("want %q got %q", s, got)
		}
	}
	if got := decodeAttributedBody([]byte{0xff, 0xff, 'j', 'u', 'n', 'k'}); got != "" {
		t.Errorf("non-typedstream decoded to %q", got)
	}
	if got := decodeAttributedBody([]byte{0x04, 0x0B}); got != "" {
		t.Errorf("truncated decoded to %q", got)
	}
	// Truncated length: must not panic or read past the end.
	b := fixtures.AttributedBody("abcdefghij")
	if got := decodeAttributedBody(b[:len(b)-60]); got != "" && got != "abcdefghij" {
		t.Errorf("truncated blob decoded to %q", got)
	}
}
