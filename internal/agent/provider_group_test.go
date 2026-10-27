// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeAdbMu serializes the fake adb tests. The script is written and then
// exec'd from the same process; if another test forks in between, the child
// inherits the open writer file descriptor and exec fails with ETXTBSY. The
// mutex is held until the test ends, so no other fake adb test can fork while
// a script is open for writing.
var fakeAdbMu sync.Mutex

// fakeAdb writes a stand-in `adb` executable so the ContentProvider can be
// exercised without a phone. It extracts the URI and where clause from the
// `content query` script it receives and answers from the supplied case block;
// anything unexpected makes it fail loudly.
func fakeAdb(t *testing.T, queries string) *Adb {
	t.Helper()
	fakeAdbMu.Lock()
	t.Cleanup(fakeAdbMu.Unlock)
	dir := t.TempDir()
	path := filepath.Join(dir, "adb")
	script := `#!/bin/sh
script="$2"
uri=$(printf '%s' "$script" | sed -n 's/.*--uri \([^ ]*\).*/\1/p' | tr -d "'\"")
where=$(printf '%s' "$script" | sed -n 's/.*--where \([^ ]*\).*/\1/p' | tr -d "'\"")
` + queries
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake adb: %v", err)
	}
	return NewAdb(path, "")
}

// TestContentProviderThreadRecipientsExpandsMergedRows covers Android's merged
// multi-recipient address rows ("a|b", written for multi-recipient SMS when
// group messaging is disabled): they must expand into individual participants,
// and own numbers / duplicates must be dropped, so no fake ghost or group
// member ever appears.
func TestContentProviderThreadRecipientsExpandsMergedRows(t *testing.T) {
	t.Parallel()
	adb := fakeAdb(t, `case "$uri" in
content://mms-sms/conversations?simple=true)
	printf 'Row: 0 _id=12, date=1730000000000, message_count=4, recipient_ids=1 2 3 4, read=0, has_attachment=0, snippet=hey all\n'
	;;
content://mms-sms/canonical-address/1)
	printf 'Row: 0 address=+15551234567|+15557654321\n'
	;;
content://mms-sms/canonical-address/2)
	printf 'Row: 0 address=+15550001111\n'
	;;
content://mms-sms/canonical-address/3)
	printf 'Row: 0 address= +1 (555) 765-4321 \n'
	;;
content://mms-sms/canonical-address/4)
	printf 'Row: 0 address=+15550001111|+15550100100\n'
	;;
*)
	printf 'fake adb: unexpected query uri=%s where=%s\n' "$uri" "$where" >&2
	exit 1
	;;
esac
`)
	// Both own numbers are configured in different formats; the merged row 4
	// contains only own numbers and must disappear entirely.
	provider := NewContentProvider(adb, false, []string{"+15550001111", "+1 (555) 010-0100"}, nil)
	ctx := context.Background()

	threads, err := provider.Threads(ctx)
	if err != nil {
		t.Fatalf("Threads() error: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("Threads() = %+v, want one thread", threads)
	}
	want := []string{"+15551234567", "+15557654321"}
	if !slices.Equal(threads[0].Recipients, want) {
		t.Fatalf("thread recipients = %v, want %v", threads[0].Recipients, want)
	}
	for _, recipient := range threads[0].Recipients {
		if strings.Contains(recipient, "|") {
			t.Fatalf("merged address leaked into the participants: %q", recipient)
		}
	}
	if threads[0].ID != 12 || threads[0].MessageCount != 4 || threads[0].Snippet != "hey all" {
		t.Fatalf("unexpected thread metadata: %+v", threads[0])
	}

	recipients := provider.ThreadRecipients(ctx, 12)
	if !slices.Equal(recipients, want) {
		t.Fatalf("ThreadRecipients() = %v, want %v", recipients, want)
	}
}

// TestContentProviderMMSFromToCcMapping covers a multi-recipient MMS: the FROM
// address becomes the sender, TO (151) and CC (130) become recipients (sorted,
// deduplicated, own numbers dropped) and every non-text part becomes an
// attachment with its metadata.
func TestContentProviderMMSFromToCcMapping(t *testing.T) {
	t.Parallel()
	adb := fakeAdb(t, `case "$uri" in
content://mms)
	printf 'Row: 0 _id=534, thread_id=12, date=1730000000, msg_box=1, m_type=132, m_size=4096, m_id=NULL, sub=NULL, sub_id=1, read=1, seen=1, ct_t=application/vnd.wap.multipart.related, text_only=0\n'
	;;
content://mms/534/addr)
	printf 'Row: 0 address=+1 (555) 123-4567, type=137, charset=NULL\n'
	printf 'Row: 1 address=+15557654321, type=151, charset=NULL\n'
	printf 'Row: 2 address=+15558887777, type=130, charset=NULL\n'
	printf 'Row: 3 address=555-765-4321, type=151, charset=NULL\n'
	printf 'Row: 4 address=+15550001111, type=151, charset=NULL\n'
	printf 'Row: 5 address=+15559998888, type=129, charset=NULL\n'
	;;
content://mms/534/part)
	printf 'Row: 0 _id=901, mid=534, seq=1, ct=text/plain, name=NULL, fn=NULL, cid=NULL, cl=NULL, _data=NULL, text=group hello\n'
	printf 'Row: 1 _id=902, mid=534, seq=2, ct=application/smil, name=NULL, fn=NULL, cid=NULL, cl=NULL, _data=NULL, text=NULL\n'
	printf 'Row: 2 _id=903, mid=534, seq=3, ct=image/jpeg, name=pic.jpg, fn=NULL, cid=NULL, cl=NULL, _data=NULL, text=NULL\n'
	printf 'Row: 3 _id=904, mid=534, seq=4, ct=IMAGE/JPEG, name=NULL, fn=NULL, cid=<img2>, cl=NULL, _data=NULL, text=NULL\n'
	printf 'Row: 4 _id=905, mid=534, seq=5, ct=text/plain, name=NULL, fn=NULL, cid=NULL, cl=NULL, _data=NULL, text=second text is ignored\n'
	;;
*)
	printf 'fake adb: unexpected query uri=%s where=%s\n' "$uri" "$where" >&2
	exit 1
	;;
esac
`)
	provider := NewContentProvider(adb, false, []string{"+15550001111"}, nil)

	msg, err := provider.Message(context.Background(), "mms:1:534")
	if err != nil {
		t.Fatalf("Message() error: %v", err)
	}
	if msg.Sender != "+15551234567" {
		t.Fatalf("Sender = %q, want the FROM address", msg.Sender)
	}
	if want := []string{"+15557654321", "+15558887777"}; !slices.Equal(msg.Recipients, want) {
		t.Fatalf("Recipients = %v, want %v (TO+CC sorted, deduplicated, own number and BCC dropped)", msg.Recipients, want)
	}
	if msg.Body != "group hello" {
		t.Fatalf("Body = %q, want the first text/plain part", msg.Body)
	}
	if !msg.IsMMS || msg.ThreadID != 12 || msg.SubID != 1 || msg.Type != 1 {
		t.Fatalf("unexpected message identity: %+v", msg)
	}
	if msg.TimestampMS != 1730000000000 {
		t.Fatalf("TimestampMS = %d, want the mms date in milliseconds", msg.TimestampMS)
	}

	wantAttachments := []Attachment{
		{PartID: 903, ContentType: "image/jpeg", FileName: "pic.jpg", URL: "/v1/mms/534/parts/903"},
		{PartID: 904, ContentType: "image/jpeg", FileName: "<img2>", URL: "/v1/mms/534/parts/904"},
	}
	if !slices.Equal(msg.Attachments, wantAttachments) {
		t.Fatalf("Attachments = %+v, want %+v", msg.Attachments, wantAttachments)
	}
}

// TestContentProviderMMSOutgoingGroupMapping covers an outgoing multi-recipient
// MMS: the FROM address is the phone's own number (dropped, no sender) and the
// TO/CC list is the send target set.
func TestContentProviderMMSOutgoingGroupMapping(t *testing.T) {
	t.Parallel()
	adb := fakeAdb(t, `case "$uri" in
content://mms)
	printf 'Row: 0 _id=535, thread_id=12, date=1730000001, msg_box=2, m_type=132, m_size=4096, m_id=NULL, sub=NULL, sub_id=1, read=1, seen=1, ct_t=application/vnd.wap.multipart.related, text_only=0\n'
	;;
content://mms/535/addr)
	printf 'Row: 0 address=+15550001111, type=137, charset=NULL\n'
	printf 'Row: 1 address=+15557654321, type=151, charset=NULL\n'
	printf 'Row: 2 address=+15558887777, type=151, charset=NULL\n'
	;;
content://mms/535/part)
	printf 'Row: 0 _id=911, mid=535, seq=1, ct=text/plain, name=NULL, fn=NULL, cid=NULL, cl=NULL, _data=NULL, text=hi all\n'
	;;
*)
	printf 'fake adb: unexpected query uri=%s where=%s\n' "$uri" "$where" >&2
	exit 1
	;;
esac
`)
	provider := NewContentProvider(adb, false, []string{"+15550001111"}, nil)

	msg, err := provider.Message(context.Background(), "mms:1:535")
	if err != nil {
		t.Fatalf("Message() error: %v", err)
	}
	if msg.Sender != "" {
		t.Fatalf("Sender = %q, want empty for an outgoing MMS", msg.Sender)
	}
	if msg.Type != 2 {
		t.Fatalf("Type = %d, want 2 (sent)", msg.Type)
	}
	if want := []string{"+15557654321", "+15558887777"}; !slices.Equal(msg.Recipients, want) {
		t.Fatalf("Recipients = %v, want %v", msg.Recipients, want)
	}
	if msg.Body != "hi all" {
		t.Fatalf("Body = %q", msg.Body)
	}
}

// TestContentProviderSMSGroupSenderAttribution covers SMS rows: incoming rows
// use the address as sender, sent rows have none (the phone itself wrote
// them), merged dummy rows never leak into participants and own numbers never
// become senders or recipients.
func TestContentProviderSMSGroupSenderAttribution(t *testing.T) {
	t.Parallel()
	adb := fakeAdb(t, `case "$uri" in
content://sms)
	case "$where" in
	_id=66)
		printf 'Row: 0 _id=66, thread_id=12, address=+15551234567, date=1730000000000, date_sent=1730000001000, type=1, read=1, seen=1, status=-1, sub_id=1, body=incoming hello\n'
		;;
	_id=67)
		printf 'Row: 0 _id=67, thread_id=12, address=+15550001111, date=1730000002000, date_sent=1730000002000, type=2, read=1, seen=1, status=-1, sub_id=1, body=outgoing hello\n'
		;;
	_id=68)
		printf 'Row: 0 _id=68, thread_id=12, address=+15551234567|+15557654321, date=1730000003000, date_sent=1730000003000, type=2, read=1, seen=1, status=-1, sub_id=1, body=merged dummy\n'
		;;
	_id=69)
		printf 'Row: 0 _id=69, thread_id=12, address=+15550001111, date=1730000004000, date_sent=1730000004000, type=1, read=1, seen=1, status=-1, sub_id=1, body=note to self\n'
		;;
	*)
		printf 'fake adb: unexpected sms where=%s\n' "$where" >&2
		exit 1
		;;
	esac
	;;
content://mms-sms/conversations?simple=true)
	printf 'Row: 0 _id=12, date=1730000004000, message_count=4, recipient_ids=1 2 3, read=0, has_attachment=0, snippet=hi\n'
	;;
content://mms-sms/canonical-address/1)
	printf 'Row: 0 address=+15551234567|+15557654321\n'
	;;
content://mms-sms/canonical-address/2)
	printf 'Row: 0 address=+15550001111\n'
	;;
content://mms-sms/canonical-address/3)
	printf 'Row: 0 address=+15557654321|+15558887777\n'
	;;
*)
	printf 'fake adb: unexpected query uri=%s where=%s\n' "$uri" "$where" >&2
	exit 1
	;;
esac
`)
	provider := NewContentProvider(adb, false, []string{"+15550001111"}, nil)
	ctx := context.Background()

	// The thread participants come from two merged rows plus the own number:
	// three distinct other participants.
	threadRecipients := provider.ThreadRecipients(ctx, 12)
	wantAll := []string{"+15551234567", "+15557654321", "+15558887777"}
	if !slices.Equal(threadRecipients, wantAll) {
		t.Fatalf("ThreadRecipients() = %v, want %v", threadRecipients, wantAll)
	}

	incoming, err := provider.Message(ctx, "sms:1:66")
	if err != nil {
		t.Fatalf("Message(sms:1:66) error: %v", err)
	}
	if incoming.Sender != "+15551234567" {
		t.Fatalf("incoming Sender = %q, want the row address", incoming.Sender)
	}
	if want := []string{"+15557654321", "+15558887777"}; !slices.Equal(incoming.Recipients, want) {
		t.Fatalf("incoming Recipients = %v, want the thread minus the sender (%v)", incoming.Recipients, want)
	}

	outgoing, err := provider.Message(ctx, "sms:1:67")
	if err != nil {
		t.Fatalf("Message(sms:1:67) error: %v", err)
	}
	if outgoing.Sender != "" {
		t.Fatalf("outgoing Sender = %q, want empty", outgoing.Sender)
	}
	if outgoing.Type != 2 || !slices.Equal(outgoing.Recipients, wantAll) {
		t.Fatalf("outgoing message = %+v, want all thread participants", outgoing)
	}

	// The merged dummy row Android writes for multi-recipient SMS is a sent
	// row: it must never become a sender, and its address must never leak.
	dummy, err := provider.Message(ctx, "sms:1:68")
	if err != nil {
		t.Fatalf("Message(sms:1:68) error: %v", err)
	}
	if dummy.Sender != "" {
		t.Fatalf("dummy row Sender = %q, want empty", dummy.Sender)
	}
	if strings.Contains(dummy.Sender, "|") {
		t.Fatalf("merged address leaked as a sender: %q", dummy.Sender)
	}
	for _, recipient := range dummy.Recipients {
		if strings.Contains(recipient, "|") {
			t.Fatalf("merged address leaked as a recipient: %q", recipient)
		}
	}

	// An incoming row from the phone's own number (a note to self) must not
	// produce an own-number sender either.
	self, err := provider.Message(ctx, "sms:1:69")
	if err != nil {
		t.Fatalf("Message(sms:1:69) error: %v", err)
	}
	if self.Sender != "" {
		t.Fatalf("self-SMS Sender = %q, want empty", self.Sender)
	}
	if slices.Contains(self.Recipients, "+15550001111") {
		t.Fatalf("own number leaked into the self-SMS recipients: %v", self.Recipients)
	}
}
