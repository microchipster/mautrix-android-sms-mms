// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"strconv"
	"strings"
)

// ParseContentQuery parses `content query` output of the form:
//
//	Row: 1 _id=1, thread_id=3, address=+1555..., body=hello
//
// Values are not escaped by the tool, so the projection order matters: put the
// potentially comma-containing column last (the value of the last column is
// everything up to the end of the line). Continuation lines (values containing
// newlines) are appended back to the previous row's last column.
func ParseContentQuery(out string, keys []string) []map[string]string {
	if len(keys) == 0 {
		return nil
	}
	lastKey := keys[len(keys)-1]
	var rows []map[string]string
	lines := strings.Split(out, "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if !strings.HasPrefix(line, "Row: ") {
			// Continuation of a multiline value (or trailing whitespace/empty line).
			if line == "" && (i == len(lines)-1 || lines[i+1] == "") {
				continue
			}
			if len(rows) > 0 {
				rows[len(rows)-1][lastKey] += "\n" + line
			}
			continue
		}
		rest := line[len("Row: "):]
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			continue
		}
		rest = rest[sp+1:]

		row := make(map[string]string, len(keys))
		pos := 0
		ok := true
		for idx, key := range keys {
			token := key + "="
			if idx == 0 {
				if !strings.HasPrefix(rest, token) {
					ok = false
					break
				}
				pos = len(token)
				continue
			}
			j := strings.Index(rest[pos:], ", "+token)
			if j < 0 {
				ok = false
				break
			}
			row[keys[idx-1]] = normalizeNull(rest[pos : pos+j])
			pos = pos + j + len(", "+token)
		}
		if !ok {
			continue
		}
		row[lastKey] = normalizeNull(rest[pos:])
		rows = append(rows, row)
	}
	return rows
}

// Field helpers. `content query` renders SQL NULL as "NULL".

// normalizeNull maps the literal "NULL" that `content query` prints for SQL
// NULL values to an empty string at parse time, so callers can use row maps
// directly without re-checking.
func normalizeNull(v string) string {
	if v == "NULL" {
		return ""
	}
	return v
}

func fieldString(row map[string]string, key string) string {
	v, ok := row[key]
	if !ok || v == "NULL" {
		return ""
	}
	return v
}

func fieldInt64(row map[string]string, key string) int64 {
	v := fieldString(row, key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func fieldBool(row map[string]string, key string) bool {
	return fieldInt64(row, key) != 0
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// splitSpaceList splits a space-separated list (threads.recipient_ids).
func splitSpaceList(s string) []int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NULL" {
		return nil
	}
	fields := strings.Fields(s)
	out := make([]int64, 0, len(fields))
	for _, f := range fields {
		if n, err := strconv.ParseInt(f, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}
