<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Carrier-scoped statistics (`!sms stats`)

The bridge reports message traffic by direction, message type and
carrier/SIM subscription from the management room:

```
!sms stats [24h|7d|30d|all]
```

Example output:

```
Message statistics (7d): 126 total (in 57 / out 69), SMS 101 / MMS 25

By carrier/SIM:
- sub 1 — ExampleMobile — +15550100100: 100 (in 45 / out 55), SMS 80 / MMS 20
- sub 2 — SecondCarrier — +15550100200: 23 (in 11 / out 12), SMS 18 / MMS 5
- unknown SIM: 3 (in 1 / out 2), SMS 3 / MMS 0
```

## Where the numbers come from

Everything is derived from tables the bridge already has; no extra database or
counter table is maintained:

- **Direction and type** — `message` rows of the login's portals. The network
  message ID encodes the type and SIM slot (`sms:<sub_id>:<row_id>` /
  `mms:<sub_id>:<row_id>`); outgoing messages are those whose `sender_id` is
  one of the login's own-number ghosts (or whose `sender_mxid` is the Matrix
  user, for messages sent from Matrix).
- **Carrier and own number** — `user_login.metadata.subscriptions`, filled from
  the paired app heartbeat (`subscriptions[].carrier`, `.number`,
  `.display_name`) or the ADB agent's health payload. The bridge refreshes it
  whenever the phone reports new SIM state.
- **Portal scope** — only portals linked to the login (`user_portal`) are
  counted, so other logins' traffic never leaks into your stats. The command
  reports the requesting user's default login; use `list-logins` to see it.

## "Database per carrier": aggregation, not a second database

There is deliberately **no separate database file per carrier**. A second
database would split the message history, break portal identity (one room per
thread), and complicates backups for no benefit. Instead, carrier scope is an
aggregation dimension over the existing bridge tables:

- `message.id` carries the `sub_id` prefix.
- `user_login.metadata.subscriptions` maps `sub_id` → carrier + own number.
- `portal.metadata.sub_id` maps a room to its SIM slot for per-room views.

The `stats` command is the built-in view of that aggregation. For ad-hoc SQL,
the same aggregation works directly against the bridge database. Example
SQLite view counting messages per SIM slot and type over the last 30 days:

```sql
CREATE VIEW carrier_usage AS
SELECT
  CASE WHEN id LIKE 'mms:%' THEN 'mms' ELSE 'sms' END AS kind,
  CAST(substr(substr(id, 5), 1, instr(substr(id, 5), ':') - 1) AS INTEGER) AS sub_id,
  COUNT(*) AS messages
FROM message
WHERE (id LIKE 'sms:%' OR id LIKE 'mms:%')
  AND timestamp > (strftime('%s', 'now') - 30 * 86400) * 1000000000
GROUP BY kind, sub_id
ORDER BY sub_id, kind;
```

(`timestamp` is nanoseconds since the epoch; `substr(id, 5, ...)` skips the
`sms:`/`mms:` prefix that both ID shapes share.) The equivalent PostgreSQL
extraction uses `split_part(id, ':', 2)`. Per-room views can join
`portal.metadata->>'sub_id'` on `portal.mxid = <room>`; per-contact views use
`portal.other_user_id` / `portal.metadata->'recipients'`.

Direction (in/out) needs the login's own-number ghost IDs, which only the
bridge knows; use `!sms stats` instead of hand-rolling that join. The
authoritative aggregation lives in `connector/stats.go`
(`aggregateStats`), and its unit tests double as the executable spec for
these queries.

## Limitations

- The carrier name depends on what the phone reports in its heartbeat. A
  freshly paired phone that has not sent a heartbeat yet shows `unknown
  carrier`, and messages whose `sub_id` cannot be resolved show as
  `unknown SIM`.
- SMS/MMS carries no caller name (CNAM) — see
  [`caller-id-and-labels.md`](caller-id-and-labels.md). The carrier label here
  is the user's own SIM carrier, not the sender's.
- The `all` window scans the login's full bridged history; on very large
  databases prefer `30d` or `7d`.
