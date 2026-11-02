// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"testing"
	"time"
)

func TestCdnRefsRoundtrip(t *testing.T) {
	service := testCdnService(t, nil)
	now := time.Now().UTC()
	id := "ref-aaaaaaaaaaaaaaaaaaaaaa"
	id2 := "ref-bbbbbbbbbbbbbbbbbbbbbb"
	service.RecordRef("@alice:example.com", CdnRef{
		ID: id, URL: "https://cdn.example.com/f/" + id + "#key1",
		Direction: cdnRefDirectionSent, Party: "+15550001", CreatedAt: now,
	})
	service.RecordRef("@alice:example.com", CdnRef{
		ID: id2, URL: "https://cdn.example.com/f/" + id2 + "#key2",
		Direction: cdnRefDirectionReceived, Party: "+15550002", RoomID: "!room:example.com",
		CreatedAt: now.Add(time.Minute),
	})
	service.RecordRef("@bob:example.com", CdnRef{
		ID: id, Direction: cdnRefDirectionSent, CreatedAt: now,
	})

	alice := service.Refs("@alice:example.com")
	if len(alice) != 2 {
		t.Fatalf("alice refs = %d, want 2", len(alice))
	}
	if alice[0].CreatedAt.Before(alice[1].CreatedAt) {
		t.Fatal("refs must be newest first")
	}
	ref, ok := service.Ref("@alice:example.com", id2)
	if !ok || ref.Party != "+15550002" || ref.Direction != cdnRefDirectionReceived {
		t.Fatalf("ref = %+v, ok = %v", ref, ok)
	}
	if _, ok := service.Ref("@bob:example.com", id2); ok {
		t.Fatal("refs must be per account")
	}
	if !service.ForgetRef("@alice:example.com", id2) {
		t.Fatal("ForgetRef reported no removal")
	}
	if len(service.Refs("@alice:example.com")) != 1 {
		t.Fatal("ForgetRef did not drop the record")
	}
}

func TestCdnRefsDeduplicate(t *testing.T) {
	service := testCdnService(t, nil)
	id := "dedupe-aaaaaaaaaaaaaaaaaaaa"
	first := time.Now().UTC().Add(-time.Hour)
	service.RecordRef("@a:example.com", CdnRef{ID: id, Direction: cdnRefDirectionSent, CreatedAt: first})
	service.RecordRef("@a:example.com", CdnRef{ID: id, Direction: cdnRefDirectionSent, Party: "+1", CreatedAt: time.Now().UTC()})

	refs := service.Refs("@a:example.com")
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	ref, _ := service.Ref("@a:example.com", id)
	if !ref.CreatedAt.Equal(first) {
		t.Fatalf("first sighting time not kept: %v", ref.CreatedAt)
	}
	if ref.Party != "+1" {
		t.Fatalf("party = %q, want +1", ref.Party)
	}
}

func TestCdnDeleteAnyOwner(t *testing.T) {
	service := testCdnService(t, nil)
	upload, err := service.Store("@alice:example.com", []byte("sealed"), time.Hour)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	deleted, err := service.Delete(upload.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete = %v, %v, want true", deleted, err)
	}
	if entry, _ := service.loadEntry(upload.ID); entry != nil {
		t.Fatal("entry still loads after Delete")
	}
	if deleted, _ := service.Delete(upload.ID); deleted {
		t.Fatal("second Delete reported a removal")
	}
	if deleted, _ := service.Delete("short"); deleted {
		t.Fatal("Delete accepted an invalid id")
	}
}

func TestCdnRefsFromText(t *testing.T) {
	base := "https://cdn.example.com"
	id := "abcdefghijklmnopqrstuvwx-abc"
	text := "Look: " + base + "/f/" + id + "#frag-key_1. Also https://other.example/f/x and " +
		base + "/f/" + id + " trailing."
	refs := cdnRefsFromText(base, text)
	if len(refs) != 2 {
		t.Fatalf("refs = %q, want 2", refs)
	}
	if refs[0] != base+"/f/"+id+"#frag-key_1" {
		t.Fatalf("refs[0] = %q", refs[0])
	}
	if refs[1] != base+"/f/"+id {
		t.Fatalf("refs[1] = %q", refs[1])
	}
	if got := cdnRefIDFromLink(base, refs[0]); got != id {
		t.Fatalf("id = %q, want %q", got, id)
	}
	if got := cdnRefIDFromLink(base, "https://other.example/f/"+id); got != "" {
		t.Fatalf("foreign link id = %q, want empty", got)
	}
}
