// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import "testing"

func TestCursorAfterReset(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		pageCursor int64
		current    int64
		want       int64
		wantAdv    bool
	}{
		{"advances past dropped window", 5, 0, 5, true},
		{"no cursor available", 0, 0, 0, false},
		{"same cursor does not advance", 3, 3, 3, false},
		{"never moves backwards", 2, 5, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, adv := cursorAfterReset(tc.pageCursor, tc.current)
			if got != tc.want || adv != tc.wantAdv {
				t.Fatalf("cursorAfterReset(%d, %d) = (%d, %v), want (%d, %v)", tc.pageCursor, tc.current, got, adv, tc.want, tc.wantAdv)
			}
		})
	}
}
