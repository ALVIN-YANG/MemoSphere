package memory

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// Python uuid5 uses json.dumps with ensure_ascii=True for automatic entity IDs.
func asciiJSON(value any) string {
	var out strings.Builder
	for _, r := range encoded(value) {
		if r < 127 {
			out.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			a, b := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", a, b)
		}
	}
	return out.String()
}

// Legacy body hashes use JSON with literal Unicode, including line separators.
// Escaped backslashes must survive unchanged when the text itself contains "\u2028".
func literalSeparators(value string) string {
	if !strings.Contains(value, `\u202`) {
		return value
	}
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] == '\\' && i+1 < len(value) {
			switch {
			case strings.HasPrefix(value[i:], `\u2028`):
				out.WriteRune(0x2028)
				i += 6
				continue
			case strings.HasPrefix(value[i:], `\u2029`):
				out.WriteRune(0x2029)
				i += 6
				continue
			default:
				out.WriteString(value[i : i+2])
				i += 2
				continue
			}
		}
		out.WriteByte(value[i])
		i++
	}
	return out.String()
}
