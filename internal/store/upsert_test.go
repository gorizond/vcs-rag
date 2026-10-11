package store

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qdrant/go-client/qdrant"
)

// T158: the payload sanitizer keeps the Qdrant client from panicking on
// invalid UTF-8 (file contents and names may contain arbitrary bytes); the
// content is preserved with U+FFFD replacements and valid values stay intact.
func TestSanitizePayloadInvalidUTF8(t *testing.T) {
	bad := "=3334 : \x88\xac\xef"
	payload := map[string]any{
		"text":        bad,
		"file_path":   "main.go",
		"chunk_index": 3,
		"list":        []string{"\x88", "ok"},
		"anylist":     []any{"\xac", int64(7)},
		"nested":      map[string]any{"key\x88": "\xef"},
	}
	fixed := sanitizePayload(payload)

	if got := fixed["text"].(string); !utf8.ValidString(got) || !strings.Contains(got, "\uFFFD") {
		t.Errorf("text = %q, want a valid string with U+FFFD", got)
	}
	if fixed["file_path"] != "main.go" || fixed["chunk_index"] != 3 {
		t.Errorf("valid fields changed: %+v", fixed)
	}
	if got := fixed["list"].([]string); got[0] != "\uFFFD" || got[1] != "ok" {
		t.Errorf("list = %q", got)
	}
	if got := fixed["anylist"].([]any); got[0] != "\uFFFD" || got[1] != int64(7) {
		t.Errorf("anylist = %#v", got)
	}
	nested := fixed["nested"].(map[string]any)
	if v, ok := nested["key\uFFFD"]; !ok || !utf8.ValidString(v.(string)) {
		t.Errorf("nested = %#v", nested)
	}
	// the caller's payload is untouched.
	if payload["text"].(string) != bad {
		t.Error("the original payload was modified")
	}

	// the sanitized payload marshals without a panic…
	_ = qdrant.NewValueMap(fixed)
	// …while the raw one panics (negative control): the exact T158 crash.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("qdrant.NewValueMap(payload) did not panic — the fixture is stale")
			}
		}()
		_ = qdrant.NewValueMap(payload)
	}()
}
