// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

func TestParseStatsWindow(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantLabel string
		wantDur   time.Duration
		wantAll   bool
		wantErr   bool
	}{
		{name: "default", args: nil, wantLabel: "30d", wantDur: 30 * 24 * time.Hour},
		{name: "24h", args: []string{"24h"}, wantLabel: "24h", wantDur: 24 * time.Hour},
		{name: "7d", args: []string{"7d"}, wantLabel: "7d", wantDur: 7 * 24 * time.Hour},
		{name: "all", args: []string{"all"}, wantLabel: "all time", wantAll: true},
		{name: "uppercase", args: []string{"ALL"}, wantLabel: "all time", wantAll: true},
		{name: "invalid", args: []string{"5x"}, wantErr: true},
		{name: "zero", args: []string{"0"}, wantErr: true},
		{name: "too many", args: []string{"24h", "7d"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window, err := parseStatsWindow(test.args)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseStatsWindow(%v) expected an error", test.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStatsWindow(%v) error: %v", test.args, err)
			}
			if window.Label != test.wantLabel {
				t.Errorf("label = %q, want %q", window.Label, test.wantLabel)
			}
			if window.All != test.wantAll {
				t.Errorf("all = %t, want %t", window.All, test.wantAll)
			}
			if !test.wantAll && window.Duration != test.wantDur {
				t.Errorf("duration = %s, want %s", window.Duration, test.wantDur)
			}
		})
	}
}

func TestStatsWindowBounds(t *testing.T) {
	_, end := (statsWindow{All: true}).Bounds()
	if end.IsZero() {
		t.Fatal("all-time window has a zero end")
	}
	start, end := (statsWindow{Duration: 24 * time.Hour}).Bounds()
	if got := end.Sub(start); got != 24*time.Hour {
		t.Errorf("window width = %s, want 24h", got)
	}
}

func TestAggregateStatsByCarrierAndDirection(t *testing.T) {
	messages := []statsMessage{
		{SubID: 1, IsMMS: false, FromMe: false},
		{SubID: 1, IsMMS: true, FromMe: true},
		{SubID: 1, IsMMS: true, FromMe: false},
		{SubID: 2, IsMMS: false, FromMe: true},
		{SubID: 0, IsMMS: false, FromMe: false},
	}
	subscriptions := map[int]SubscriptionInfo{
		1: {SubID: 1, Carrier: "ExampleMobile", PhoneNumber: "+1 (555) 010-0100"},
		2: {SubID: 2, Carrier: "SecondCarrier", PhoneNumber: "15550100200", DisplayName: "Work"},
	}
	report := aggregateStats(messages, subscriptions)

	if report.Counts.Total != 5 || report.Counts.In != 3 || report.Counts.Out != 2 {
		t.Errorf("total/in/out = %d/%d/%d, want 5/3/2", report.Counts.Total, report.Counts.In, report.Counts.Out)
	}
	if report.Counts.SMS != 3 || report.Counts.MMS != 2 {
		t.Errorf("sms/mms = %d/%d, want 3/2", report.Counts.SMS, report.Counts.MMS)
	}
	if len(report.Carriers) != 3 {
		t.Fatalf("carrier entries = %d, want 3 (unknown, sub 1, sub 2)", len(report.Carriers))
	}
	if report.Carriers[0].SubID != 0 || report.Carriers[0].Counts.Total != 1 {
		t.Errorf("unknown-SIM entry = %+v, want sub 0 with 1 message", report.Carriers[0])
	}
	sub1 := report.Carriers[1]
	if sub1.SubID != 1 || sub1.Carrier != "ExampleMobile" || sub1.Number != "+15550100100" {
		t.Errorf("sub 1 identity = %+v", sub1)
	}
	if sub1.Counts.Total != 3 || sub1.Counts.In != 2 || sub1.Counts.Out != 1 || sub1.Counts.SMS != 1 || sub1.Counts.MMS != 2 {
		t.Errorf("sub 1 counts = %+v", sub1.Counts)
	}
	sub2 := report.Carriers[2]
	if sub2.SubID != 2 || sub2.Counts.Out != 1 || sub2.Counts.SMS != 1 {
		t.Errorf("sub 2 counts = %+v", sub2)
	}
}

func TestAggregateStatsWithoutSubscriptions(t *testing.T) {
	report := aggregateStats([]statsMessage{{SubID: 0, FromMe: true}}, nil)
	if report.Counts.Total != 1 || report.Counts.Out != 1 {
		t.Fatalf("aggregate without subscriptions = %+v", report.Counts)
	}
	if len(report.Carriers) != 1 || report.Carriers[0].SubID != 0 {
		t.Fatalf("carrier entries = %+v, want one unknown-SIM entry", report.Carriers)
	}
}

func TestFormatStatsReport(t *testing.T) {
	report := statsReport{
		Window: "7d",
		Counts: statsCounts{Total: 2, In: 1, Out: 1, SMS: 1, MMS: 1},
		Carriers: []statsCarrierStats{{
			SubID:   1,
			Carrier: "ExampleMobile",
			Number:  "+15550100100",
			Counts:  statsCounts{Total: 2, In: 1, Out: 1, SMS: 1, MMS: 1},
		}},
	}
	out := formatStatsReport(report)
	for _, want := range []string{
		"Message statistics (7d)",
		"2 total (in 1 / out 1)",
		"SMS 1 / MMS 1",
		"sub 1",
		"ExampleMobile",
		"+15550100100",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted report does not contain %q:\n%s", want, out)
		}
	}

	unknown := formatStatsReport(statsReport{Window: "24h", Carriers: []statsCarrierStats{{SubID: 0, Counts: statsCounts{Total: 3}}}})
	if !strings.Contains(unknown, "unknown SIM") {
		t.Errorf("unknown SIM entry missing:\n%s", unknown)
	}

	empty := formatStatsReport(statsReport{Window: "30d"})
	if !strings.Contains(empty, "No subscriptions known") {
		t.Errorf("empty report should explain missing subscriptions:\n%s", empty)
	}
}

func TestMessageSubID(t *testing.T) {
	tests := []struct {
		id   string
		want int
	}{
		{id: "sms:1:42", want: 1},
		{id: "mms:2:7", want: 2},
		{id: "sms:0:1", want: 0},
		{id: "mms:12:345", want: 12},
		{id: "pending-abc", want: 0},
		{id: "sms:x:1", want: 0},
		{id: "sms:-1:1", want: 0},
		{id: "sms:1", want: 0},
	}
	for _, test := range tests {
		if got := messageSubID(test.id); got != test.want {
			t.Errorf("messageSubID(%q) = %d, want %d", test.id, got, test.want)
		}
	}
}

func TestStatsFromMe(t *testing.T) {
	own := map[networkid.UserID]bool{MakeUserID("+15550100100"): true}
	user := id.UserID("@alice:example.com")

	if !statsFromMe(MakeUserID("+15550100100"), "", own, user) {
		t.Error("phone-originated message should count as outgoing")
	}
	if !statsFromMe("", user, own, user) {
		t.Error("Matrix-originated message should count as outgoing")
	}
	if statsFromMe(MakeUserID("+15551234567"), "", own, user) {
		t.Error("message from another number should count as incoming")
	}
	if statsFromMe("", "", own, user) {
		t.Error("message without a sender should count as incoming")
	}
}
