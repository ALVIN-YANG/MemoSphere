package memory

import (
	"strings"
	"time"
)

// Keep the sender's offset for relative event dates. Store the original representation.
func spokenTime(value string) (time.Time, bool) {
	if len(value) < 12 || len(value) > 40 {
		return time.Time{}, false
	}
	if value[10] == ' ' {
		value = value[:10] + "T" + value[11:]
	}
	value = strings.Replace(value, ",", ".", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07", "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700", "2006-01-02T15:04Z07"} {
		if t, e := time.Parse(layout, value); e == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
