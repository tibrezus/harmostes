package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// formatJSONLogLine parses a slog JSON line and formats it for display:
//
//	LEVEL  msg  key1=val1  key2=val2
//
// Returns (formatted, true) on success, ("", false) if the line is not valid
// JSON slog output.
func formatJSONLogLine(line string) (string, bool) {
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return "", false
	}

	level, _ := entry["level"].(string)
	msg, _ := entry["msg"].(string)
	ts, _ := entry["time"].(string)

	// Extract fields other than the standard slog keys.
	var fields []string
	for k, v := range entry {
		switch k {
		case "level", "msg", "time":
			continue
		default:
			fields = append(fields, fmt.Sprintf("%s=%v", k, formatLogValue(v)))
		}
	}

	var b strings.Builder
	// Timestamp (short HH:MM:SS)
	if ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			b.WriteString(t.Format("15:04:05 "))
		}
	}
	// Level (padded to 5)
	b.WriteString(strings.ToUpper(level))
	// Message
	if msg != "" {
		b.WriteString("  ")
		b.WriteString(msg)
	}
	// Extra fields
	for _, f := range fields {
		b.WriteString("  ")
		b.WriteString(f)
	}
	return b.String(), true
}

// formatLogValue renders a JSON value for display in a log line.
func formatLogValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return fmt.Sprintf("%v", val)
	case bool:
		return fmt.Sprintf("%v", val)
	case nil:
		return "null"
	default:
		b, _ := json.Marshal(val)
		return string(b)
	}
}

// splitLines splits a string into lines for template iteration.
func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// logLineClass returns a CSS class for a formatted log line based on its level
// prefix (for color-coded output in the run detail view).
func logLineClass(line string) string {
	upper := strings.ToUpper(line)
	switch {
	case strings.Contains(upper, "ERROR"):
		return "log-line--error"
	case strings.Contains(upper, "WARN"):
		return "log-line--warn"
	default:
		return ""
	}
}
