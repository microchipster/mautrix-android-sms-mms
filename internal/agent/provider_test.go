// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"reflect"
	"testing"
)

func TestSplitMergedAddresses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "+15551234567", []string{"+15551234567"}},
		{"merged", "+15551234567|+15557654321", []string{"+15551234567", "+15557654321"}},
		{"merged with spaces", " +15551234567 | +15557654321 ", []string{"+15551234567", "+15557654321"}},
		{"empty parts", "+15551234567||", []string{"+15551234567"}},
		{"only separators", "||", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitMergedAddresses(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitMergedAddresses(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
