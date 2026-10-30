// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import "testing"

func TestSanitizeNumberLabel(t *testing.T) {
	cases := map[string]string{
		"personal":                       "personal",
		" Personal ":                     "personal",
		"(work)":                         "work",
		"burner phone":                   "burner-phone",
		"BURNER  phone":                  "burner-phone",
		"work--phone":                    "work-phone",
		"family.plan":                    "family-plan",
		"work_phone":                     "work-phone",
		"!!!":                            "",
		"":                               "",
		"a":                              "a",
		"this-is-a-very-long-label-name": "this-is-a-very-long-labe",
	}
	for in, want := range cases {
		if got := SanitizeNumberLabel(in); got != want {
			t.Errorf("SanitizeNumberLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoomNameSuffix(t *testing.T) {
	subs := map[int]SubscriptionInfo{
		1: {SubID: 1, PhoneNumber: "+14155550100"},
		2: {SubID: 2, PhoneNumber: "+14155550101"},
		3: {SubID: 3},
	}
	labels := map[string]string{"+14155550100": "personal", "+14155550101": "work"}
	cases := []struct {
		name  string
		subID int
		want  string
	}{
		{"labeled sub", 1, "(sms-personal)"},
		{"other labeled sub", 2, "(sms-work)"},
		{"subscription without a number", 3, "(sms)"},
		{"unknown sub", 9, "(sms)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoomNameSuffix(tc.subID, subs, labels); got != tc.want {
				t.Errorf("RoomNameSuffix(%d) = %q, want %q", tc.subID, got, tc.want)
			}
		})
	}
	if got := RoomNameSuffix(1, nil, nil); got != RoomNameSuffixDefault {
		t.Errorf("nil maps = %q, want %q", got, RoomNameSuffixDefault)
	}
	if got := RoomNameSuffix(1, subs, map[string]string{"+14155550100": ""}); got != RoomNameSuffixDefault {
		t.Errorf("empty label = %q, want %q", got, RoomNameSuffixDefault)
	}
}

func TestNumberLabelTargets(t *testing.T) {
	subs := map[int]SubscriptionInfo{
		2: {SubID: 2, PhoneNumber: "+14155550101"},
		1: {SubID: 1, PhoneNumber: "+14155550100"},
		3: {SubID: 3},
	}
	targets := numberLabelTargets(subs)
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want 2 entries", targets)
	}
	if targets[0].SubID != 1 || targets[0].Number != "+14155550100" {
		t.Errorf("targets[0] = %+v", targets[0])
	}
	if targets[1].SubID != 2 || targets[1].Number != "+14155550101" {
		t.Errorf("targets[1] = %+v", targets[1])
	}
	if _, ok := resolveNumberLabelTarget("+15555550199", targets); ok {
		t.Error("an unknown number was resolved")
	}
	if target, ok := resolveNumberLabelTarget("+14155550101", targets); !ok || target.SubID != 2 {
		t.Errorf("resolve by number = %+v, %v", target, ok)
	}
	if target, ok := resolveNumberLabelTarget("1", targets); !ok || target.Number != "+14155550100" {
		t.Errorf("resolve by sub id = %+v, %v", target, ok)
	}
}
