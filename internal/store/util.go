package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// rfc renders a nullable time as an RFC3339 string for storage (nil → NULL).
func rfc(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseNullTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullJSON stores raw JSON, or NULL when empty.
func nullJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return string(j)
}

func rawJSON(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
