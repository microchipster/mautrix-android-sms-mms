# Agent API v1 (draft, implemented incrementally)

`sms-mms-adb-agent` exposes this HTTP/JSON API. Default bind: `127.0.0.1:8093`.
Auth: `Authorization: Bearer <token>` on every route except `/v1/health` (safe subset).
All addresses are normalized by the agent (E.164 where possible, short codes / sender IDs
kept raw, email gateways lowercased). All timestamps are Unix milliseconds.

Message IDs are namespaced per device by the bridge; on the wire they are:
`sms:<sub_id>:<row_id>` and `mms:<sub_id>:<row_id>`.

## Health / device

```
GET /v1/health
→ {
  "ok": true,
  "protocol_version": 1,
  "device": {"serial": "...", "model": "...", "android_sdk": 35, "android_release": "15"},
  "root": false,
  "backend": {"read": "content-provider", "send_sms": "shell", "send_mms": false, "can_mark_read": true},
  "default_sms_app": "com.android.messaging",
  "capabilities": ["threads", "messages", "mms-parts", "mark-read", "send-sms"]
}
```

## Subscriptions (SIMs)

```
GET /v1/subscriptions
→ [{"sub_id": 1, "sim_slot": 0, "display_name": "SIM 1", "carrier": "...", "number": "+1555...", "default_sms": true}]
```

## Threads and messages

```
GET /v1/threads
→ [{"id": 12, "sub_id": 1, "date_ms": 1730000000000, "message_count": 42,
    "recipients": ["+15551234567"], "snippet": "…", "read": true, "has_attachment": false}]

GET /v1/threads/{id}
→ thread object + {"recipients": [...], "unread_count": 2}

GET /v1/threads/{id}/messages?before_ms=&after_ms=&limit=100
→ [message objects, oldest first]

GET /v1/messages/{message_id}
→ full message object (for MMS: body from text part, attachments with part IDs + URLs)

POST /v1/read
{"thread_id": 12, "up_to_ms": 1730000000000}      (or "message_ids": ["sms:1:77"])
→ {"updated": 3, "supported": true}
```

Message object:

```json
{
  "id": "mms:1:534", "thread_id": 12, "sub_id": 1,
  "timestamp_ms": 1730000000000, "timestamp_sent_ms": 1730000000000,
  "sender": "+15551234567", "recipients": ["+15550001111", "+15550002222"],
  "body": "text content", "is_mms": true, "type": 1, "read": false, "seen": false,
  "status": 0, "text_only": false,
  "attachments": [{"part_id": 901, "content_type": "image/jpeg", "file_name": "photo.jpg",
                   "size": 123456, "url": "/v1/mms/534/parts/901"}]
}
```

- `sender` is empty for messages sent from this phone.
- `recipients` is the thread participant set minus own numbers (group threads: all members).
- `type`: 1 inbox, 2 sent, 3 draft (not emitted), 4 outbox, 5 failed, 6 queued.
- MMS `body` is the first `text/plain` part (SMIL not required); `attachments` lists
  non-text parts in SMIL/`seq` order.

## Media

```
GET /v1/mms/{id}/parts/{part_id}         → raw bytes, Content-Type from part.ct
```

Binary parts are streamed via `adb exec-out content read` (non-root) or the `_data` path
(root); text parts return their `text` column inline in the message object instead.

## Events

```
GET /v1/events?since=<seq>&timeout_ms=25000
→ {"reset": false, "cursor": 1041,
   "events": [{"seq": 1040, "type": "message.new", "message_id": "sms:1:8123",
               "thread_id": 12, "sub_id": 1, "timestamp_ms": 1730000000000,
               "sender": "+15551234567", "is_mms": false, "has_attachment": false,
               "preview": "see you at 8"}]}
```

- `type`: `message.new`, `message.updated` (type/status/read changed), `mms.downloaded`
  (notification row became a downloaded message), `thread.updated` (participants/read).
- If `since` is older than the retained window, the response sets `"reset": true` and the
  client must run a full catch-up via `/v1/threads` + `/v1/threads/{id}/messages`.
- Polling from Matrix side is expected; the agent's internal sweeps produce events within
  the poll interval (default 3 s).

## Send

```
POST /v1/send
{"send_id": "<client uuid>", "thread_id": 12?, "sub_id": 1?, "recipients": ["+15551234567"],
 "text": "hello", "attachments": [{"content_type": "image/jpeg", "file_name": "a.jpg", "base64": "..."}]}
→ {"send_id": "...", "message_id": "sms:1:8130", "thread_id": 12, "parts_sent": 1, "pending": false}
```

- `message_id` may be empty with `"pending": true` when the phone row has not appeared yet
  (bridge correlates later by `send_id` carried in the resulting event).
- Attachments are only accepted when the active backend supports MMS send; otherwise the
  route returns `501` with `{"error": "mms_send_unsupported", "backend": "shell"}`.
- Long SMS: the `shell` backend refuses >160 UTF-16 code units unless
  `allow_sms_split: true`, in which case it splits into `parts_sent` concatenated-in-order
  messages and says so in the response.

## Contacts

```
GET /v1/contacts?q=alice&limit=50
→ [{"address": "+15551234567", "name": "Alice", "avatar_url": "..."}]
```

Best-effort (provider lookup; skipped when the device denies contact reads).

## Error format

Non-2xx always returns `{"error": "<machine_code>", "message": "<human text>"}`.
Codes: `agent_degraded`, `device_offline`, `not_found`, `unsupported`, `send_failed`,
`too_large`, `unauthorized`.
