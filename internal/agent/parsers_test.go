// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"testing"
)

func TestParseContentQueryBasic(t *testing.T) {
	out := "Row: 0 _id=1, thread_id=42, date=1730000000000, address=+15551234567, body=hello world\n" +
		"Row: 1 _id=2, thread_id=42, date=1730000001000, address=+15551234567, body=second\n"
	rows := ParseContentQuery(out, []string{"_id", "thread_id", "date", "address", "body"})
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0]["body"] != "hello world" {
		t.Fatalf("body[0] = %q", rows[0]["body"])
	}
	if rows[1]["_id"] != "2" || rows[1]["date"] != "1730000001000" {
		t.Fatalf("row[1] wrong: %#v", rows[1])
	}
}

func TestParseContentQueryCommasInLastColumn(t *testing.T) {
	out := "Row: 0 _id=7, address=+15550001111, body=soup, salad, bread\n"
	rows := ParseContentQuery(out, []string{"_id", "address", "body"})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0]["body"] != "soup, salad, bread" {
		t.Fatalf("body = %q", rows[0]["body"])
	}
}

func TestParseContentQueryNullAndMultiline(t *testing.T) {
	out := "Row: 0 _id=3, addr=NULL, body=line one\ncontinued line\n"
	rows := ParseContentQuery(out, []string{"_id", "addr", "body"})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0]["addr"] != "" {
		t.Fatalf("NULL should parse to empty, got %q", rows[0]["addr"])
	}
	if rows[0]["body"] != "line one\ncontinued line" {
		t.Fatalf("body = %q", rows[0]["body"])
	}
}

func TestParseContentQueryEmpty(t *testing.T) {
	if rows := ParseContentQuery("No result found.\n", []string{"_id", "body"}); len(rows) != 0 {
		t.Fatalf("expected no rows, got %d", len(rows))
	}
}

func TestParseMessageID(t *testing.T) {
	kind, subID, rowID, err := ParseMessageID("sms:2:1234")
	if err != nil || kind != "sms" || subID != 2 || rowID != 1234 {
		t.Fatalf("got %q %d %d %v", kind, subID, rowID, err)
	}
	kind, subID, rowID, err = ParseMessageID("mms:-1:99")
	if err != nil || kind != "mms" || subID != -1 || rowID != 99 {
		t.Fatalf("got %q %d %d %v", kind, subID, rowID, err)
	}
	if _, _, _, err := ParseMessageID("bogus"); err == nil {
		t.Fatal("expected error for bogus id")
	}
}

func TestCanonicalAddress(t *testing.T) {
	cases := map[string]string{
		"+1 (555) 123-4567":  "+15551234567",
		"555-123-4567":       "+15551234567",
		"5551234":            "+15551234",
		"12345":              "12345",
		"Testsender":         "TESTSENDER",
		"user@example.com":   "user@example.com",
		"+15551234567;foo=1": "+15551234567",
		"":                   "",
	}
	for in, want := range cases {
		if got := CanonicalAddress(in); got != want {
			t.Errorf("CanonicalAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote("hello world"); got != "'hello world'" {
		t.Fatalf("got %q", got)
	}
	if got := ShellQuote("plain"); got != "plain" {
		t.Fatalf("got %q", got)
	}
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("got %q", got)
	}
}

func TestCountUTF16AndSplit(t *testing.T) {
	if n := countUTF16("hello"); n != 5 {
		t.Fatalf("countUTF16 = %d", n)
	}
	if n := countUTF16("héllo"); n != 5 {
		t.Fatalf("countUTF16 accented = %d", n)
	}
	long := ""
	for i := 0; i < 400; i++ {
		long += "a"
	}
	parts := splitMessage(long, 153)
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	if total != 400 {
		t.Fatalf("split lost characters: %d", total)
	}
}

func TestIsmsSendTxCode(t *testing.T) {
	cases := map[int]int{34: 5, 30: 5, 29: 6, 23: 6, 22: 9, 19: 5}
	for sdk, want := range cases {
		if got := ismsSendTxCode(sdk); got != want {
			t.Errorf("ismsSendTxCode(%d) = %d, want %d", sdk, got, want)
		}
	}
}
