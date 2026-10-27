// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"strings"
	"testing"
)

func TestParseLabelArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantAddress string
		wantLabel   string
		wantClear   bool
		wantErr     bool
	}{
		{name: "simple", args: []string{"+15551234567", "family"}, wantAddress: "+15551234567", wantLabel: "family"},
		{name: "label with spaces", args: []string{"+15551234567", "very", "important"}, wantAddress: "+15551234567", wantLabel: "very important"},
		{name: "clear", args: []string{"+15551234567", "--clear"}, wantAddress: "+15551234567", wantClear: true},
		{name: "clear uppercase", args: []string{"+15551234567", "--CLEAR"}, wantAddress: "+15551234567", wantClear: true},
		{name: "missing label", args: []string{"+15551234567"}, wantErr: true},
		{name: "empty args", args: nil, wantErr: true},
		{name: "empty address", args: []string{" ", "family"}, wantErr: true},
		{name: "unknown option", args: []string{"+15551234567", "--remove"}, wantErr: true},
		{name: "clear with trailing text", args: []string{"+15551234567", "--clear", "spam"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, label, clear, err := parseLabelArgs(test.args)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseLabelArgs(%v) expected an error", test.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLabelArgs(%v) error: %v", test.args, err)
			}
			if address != test.wantAddress || label != test.wantLabel || clear != test.wantClear {
				t.Errorf("parseLabelArgs(%v) = (%q, %q, %t), want (%q, %q, %t)",
					test.args, address, label, clear, test.wantAddress, test.wantLabel, test.wantClear)
			}
		})
	}
}

func TestAddLabel(t *testing.T) {
	labels := addLabel(nil, "family")
	if len(labels) != 1 || labels[0] != "family" {
		t.Fatalf("addLabel(nil) = %v", labels)
	}
	labels = addLabel(labels, "Family")
	if len(labels) != 1 {
		t.Fatalf("duplicate label was appended: %v", labels)
	}
	labels = addLabel(labels, "  important  ")
	if len(labels) != 2 || labels[1] != "important" {
		t.Fatalf("addLabel with whitespace = %v", labels)
	}
	labels = addLabel(labels, "   ")
	if len(labels) != 2 {
		t.Fatalf("blank label was appended: %v", labels)
	}
}

func TestFormatLabeledContacts(t *testing.T) {
	if got := formatLabeledContacts(nil); !strings.Contains(got, "No labels yet") {
		t.Errorf("empty listing = %q", got)
	}
	got := formatLabeledContacts([]labeledContact{
		{Address: "+15550100100", Labels: []string{"family", "work"}},
		{Address: "BANK", Labels: []string{NewContactLabel}},
	})
	if !strings.Contains(got, "- +15550100100: family, work") {
		t.Errorf("listing missing first contact:\n%s", got)
	}
	if !strings.Contains(got, "- BANK: new contact") {
		t.Errorf("listing missing second contact:\n%s", got)
	}
}
