# ADB / telephony provider reference (condensed from research)

Verified against AOSP main (tags 8.1→16), Termux sources, capcom6 docs, mautrix sources.
Operational reference for the agent. Group/attachment correctness depends on these details.

## DB locations & schema (AOSP TelephonyProvider, schema v69)

- `/data/user/0/com.android.providers.telephony/databases/mmssms.db` (CE, after first unlock)
- `/data/user_de/0/com.android.providers.telephony/databases/...` (DE: `raw` table only)
- `app_parts/`: `/data/user/0/com.android.providers.telephony/app_parts/PART_<millis>_<cl>`
- WAL: AOSP main disables WAL (`setWriteAheadLoggingEnabled(false)`) → rollback journal.
  Never copy bare `.db`; use `sqlite3 db ".backup out.db"` or `VACUUM INTO`.

Key tables:

```sql
sms (_id INTEGER PRIMARY KEY /* no AUTOINCREMENT → rowids reusable */, thread_id, address, person,
     date ms, date_sent ms, protocol, read, status, type /* 1 in 2 sent 3 draft 4 outbox 5 failed 6 queued */,
     reply_path_present, subject, body, service_center, locked, sub_id DEFAULT -1, error_code, creator, seen)
threads (_id, date, message_count, recipient_ids /* space-separated canonical_addresses._id */,
     snippet, snippet_charset, read, archived, type, error, has_attachment, sub_id)
canonical_addresses (_id, address, sub_id DEFAULT -1)
pdu (_id, thread_id, date SECONDS, date_sent SECONDS, msg_box /* 1 in 2 sent 3 draft 4 outbox */, read,
     m_id, sub, sub_cs, ct_t, ct_l, exp, m_cls, m_type /* 128 SEND_REQ 130 NOTIF_IND 132 RETRIEVE_CONF */,
     v, m_size, pri, rr, rpt_a, resp_st, st, tr_id, retr_st, retr_txt, retr_txt_cs, read_status,
     ct_cls, resp_txt, d_tm, d_rpt, locked, sub_id, seen, creator, text_only)
addr (_id, msg_id, contact_id, address, type /* 129 BCC 130 CC 137 FROM 151 TO */, charset, sub_id)
part (_id, mid, seq, ct, name, chset, cd, fn, cid, cl, ctt_s, ctt_t, _data, text, sub_id)
```

- `text/plain` + `application/smil` parts store content inline in `part.text`; other types
  are files whose path is in `part._data`.
- Multipart SMS is reassembled by framework into ONE `sms` row (raw segments in `raw`
  during delivery). `raw.pdu` is the only PDU column.
- Group MMS: FROM=sender, TO=everyone incl. self (AOSP excludes self via
  `TelephonyManager.getLine1Number()` — null on MVNO/eSIM → self can leak into
  `recipient_ids`). CC sometimes used (AOSP loads TO+CC with excludeMyNumber).
- SMIL: `<img src="cid:1.jpg">` etc.; match `part.cid`/`cl`/`name`; may be absent (use
  `part.seq` order, first `text/plain` part = body).
- `mms-sms` URIs: `content://mms-sms/conversations?simple=true`,
  `content://mms-sms/canonical-address/<id>`, `content://mms-sms/threadID`.
- Restricted views for non-default callers: `sms_restricted` (type 1|2),
  `pdu_restricted` (msg_box 1|2 AND m_type != 130); no subqueries in selection/sort/proj.

## Non-root adb shell (works on AOSP-derived ROMs; com.android.shell has SMS perms)

```bash
adb shell 'content query --uri content://sms/inbox \
  --projection _id:thread_id:address:date:date_sent:type:read:seen:sub_id:body \
  --where "date > 1730000000000" --sort "date ASC"'
adb shell 'content query --uri "content://mms-sms/conversations?simple=true" \
  --projection _id:recipient_ids:date:snippet:read:type'
adb shell 'content query --uri content://mms-sms/canonical-address/17 --projection address'
adb shell 'content query --uri content://mms --where "date > 1730000000" --sort "date ASC" \
  --projection _id:thread_id:date:msg_box:m_type:m_size:m_id:sub:sub_id:read:seen:ct_t:text_only'
adb shell 'content query --uri content://mms/123/addr --projection address:type:charset'
adb shell 'content query --uri content://mms/123/part --projection _id:mid:seq:ct:name:fn:cid:cl:_data:text'
adb exec-out 'content read --uri content://mms/part/456' > attachment.bin
adb shell 'content update --uri content://sms --where "_id=7" --bind read:i:1'
adb shell 'content update --uri content://mms --where "_id=123" --bind read:i:1'
adb shell 'service call isms 5 i32 <subId> s16 "com.android.shell" s16 "null" s16 "+15551234567" \
  s16 "null" s16 "hello" s16 "null" s16 "null" i32 1 i64 0'   # Android 11+; i32 1 = persist to DB
adb shell 'content query --uri content://telephony/siminfo --projection _id:sim_id:display_name:carrier_name'
```

`service call isms` transaction codes (sendText/subscriber/multipart): 4.4: 5/-/6;
5.1: 9/10/12/13; 6.0–10: 6/7/9; **11–16+ (current): 5/-/8**. Text-only, single PDU, no
multipart, no delivery PendingIntents; version-fragile. Pass a caller-owned package name.
- `content query` output `Row: N k=v, k=v` is unescaped — projections and last-column tricks.
  **`content query` has no `--limit`** (verified on LineageOS 21 / Android 14: the flag
  prints usage and the query returns nothing) — apply limits client-side; only
  newest-first/primary-key queries may be truncated, ascending sweeps must be bounded by
  date windows instead.

## Root-only capabilities

- `su -c sqlite3 <db>` (sqlite3 present on most AOSP builds, not guaranteed: fallbacks =
  Termux `sqlite3`, pushed static binary, or `content query`).
- `.backup` snapshots, `app_parts` reads, `su -c content ...`, `pm grant`, `cmd role`, and
  `inotifyd` watch of the DB dir for cheap wakeups:
  `su -c 'inotifyd /data/local/tmp/onchange.sh /data/user/0/com.android.providers.telephony/databases'`
- Still no broadcast push: `SMS_RECEIVED`/`WAP_PUSH_DELIVER` are manifest-app only.
- GrapheneOS cannot be rooted → non-root path is mandatory there.

## Sending

- SMS: `service call isms` (short, persists with i32 1, no multipart) or Termux
  `RUN_COMMAND` (proper multipart via `sendMultipartTextMessage`, but non-default app →
  NOT persisted to provider → invisible to phone UI; needs Termux + Termux:API:
  `am startservice -n com.termux/com.termux.app.RunCommandService -a com.termux.RUN_COMMAND
  --es .../RUN_COMMAND_PATH /data/data/com.termux/files/usr/bin/termux-sms-send
  --esa .../RUN_COMMAND_ARGUMENTS "-n,+1555...,text"`).
- MMS: **no adb/root path**. `MmsServiceBroker` needs SEND_SMS/RECEIVE_MMS; `MmsService`
  enforces system uid; app must hand a composed M-Send.req PDU to
  `SmsManager.sendMultimediaMessage`; MMS APN only reachable from the radio. Requires an
  app on the phone (default-SMS role strongly recommended). `service call mms` cannot pass
  nested parcels.

## Transport

- Pin device: `adb -s <serial>`; supervise with `adb wait-for-device` + `reconnect`.
- USB > Wi-Fi; wireless debugging as fallback.
- `adb reverse tcp:9090 tcp:9090` → phone can POST webhooks to host's 127.0.0.1:9090 over
  USB; `adb forward tcp:8080 tcp:8080` → host can call a phone-side REST API.
- Use `adb exec-out` for binary (shell mangles newlines).

## Gotchas (group correctness / duplicates)

1. `sms._id` no AUTOINCREMENT → ids reusable; sweep by `date` with overlap, dedupe.
2. SMS dates ms; MMS dates **seconds**.
3. Ignore MMS notifications (`m_type=130`, no parts); emit only downloaded inbox/sent.
4. Restricted views hide notifications/drafts; root/app must filter.
5. Group identity = recipient set; thread changes when membership changes.
6. Own number exclusion can fail (null line1) → config `own_numbers` and filter.
7. Duplicate/degenerate TO/CC, TO==FROM common → dedupe normalized.
8. Outgoing MMS often lacks FROM → sender is self.
9. Addresses may be emails or short codes; don't force E.164 everywhere.
10. Group MMS sometimes `ct_t=application/vnd.wap.multipart.related`, members in CC.
11. SMIL may be absent; `_data` may point to deleted file → fall back to `text` parts.
12. Carrier retries store duplicate SMS rows → dedupe `(address, date_sent, body)` TTL.
13. Row ids restart per device → namespace events by device serial.
14. Non-default sends not persisted → phone UI never sees them.
15. Mark-read via provider (triggers recompute threads); direct sqlite UPDATE skips
    triggers/notifyChange → use `content update`; set both `read` and `seen`.
16. Never inject faked rows via sqlite (FTS/thread counters break).
17. FBE: CE DB + parts unavailable until first unlock; phone should not have a lock.
18. Watermarks per `(device, sub_id, last_date, last_id)` with overlap on resume.
19. Multi-SIM: same conversation = two threads (`sub_id`); key rooms by
    `(sub_id, thread_id)`.
20. `content query` output unescaped; `adb shell` corrupts binary streams.
21. `service call isms` codes version-specific; test per device.
22. Android 15+: sideloaded app SMS perms grayed in Settings; grant via `pm grant`    (SEND_SMS, RECEIVE_SMS, RECEIVE_MMS, READ_PHONE_STATE) then reboot; toggle stays gray.
23. Dedupe app-level MMS events by `deviceId:event:messageId` (mms rows restart per device).
24. Disable RCS (Google Messages) or RCS chats silently bypass SMS/MMS hooks.
25. MMS requires mobile data (MMS APN); auto-retrieve must be on or notifications never
    become downloaded messages.
26. MMS size cap default 300 KB (carrier config raises to 600KB–2MB); platform does not
    transcode — sending app must resize (HEIC→JPEG, downscale, video→3GPP/MP4 if small).
27. Never read binary part files with `su -c cat`: Magisk's `su` runs the command in a
    pty with ONLCR translation, turning each `0x0A` byte into `0x0D 0x0A` (observed +159
    bytes on a 46 KB JPEG on LineageOS 21). Use `content read --uri content://mms/part/N`
    (byte-exact, works non-root) or plain `cat` (part files are world-readable).
