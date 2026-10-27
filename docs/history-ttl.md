<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Matrix history retention (TTL)

The bridge can periodically redact bridged messages from Matrix rooms and
delete the matching bridge database rows once they are older than a configured
retention period. The phone's own database is never touched: this only limits
how much history lives in the Matrix rooms / bridge database.

The feature is **opt-in** (`history_ttl.enabled: false` by default) and has a
dry-run mode.

## Configuration

All keys live under `history_ttl` in the bridge's network config
(`simple-config.yaml`):

```yaml
network:
  history_ttl:
    # Master switch. The cleanup loop does nothing while this is false.
    enabled: true

    # Global retention period. Accepts "12h", "30d", "365d" or "0"/empty for
    # "keep forever". Empty means the cleanup has nothing to do by default.
    global: 365d

    # Per-room overrides, keyed by exact Matrix room ID.
    per_room:
      "!abcdef:example.com": 30d

    # Per-contact overrides, keyed by phone number / sender ID (canonicalized
    # on load). An explicit 0 keeps that contact's history forever even when a
    # global or room period is configured.
    per_contact:
      "+15550100100": 7d
      "+15550100200": 0

    # Named redaction patterns, redacted regardless of the retention period.
    patterns: [otp, credit-card]

    # Extension map: name -> Go regular expression (RE2). Names are free-form.
    custom_patterns:
      ticket-number: '(?i)\bticket\s*#?\d{3,}\b'

    # Include messages that carry MMS attachments in the time-based cleanup
    # (default: true). Patterns still redact matching messages either way.
    delete_media: true

    # Only report what a run would do (default: false).
    dry_run: false

    # Cleanup cadence (default: 6h) and per-run scan bounds.
    interval: 6h
    scan_window: 30d   # recent-message window for pattern matching (default: 30d)
    scan_limit: 2000   # max messages inspected per portal and run (default: 2000)
```

### Override precedence

For each room the bridge resolves the retention in this order:

1. `per_contact` entry for the DM's contact (canonical phone number),
2. `per_room` entry for the Matrix room ID,
3. `global`.

An explicit `0` at any level means "keep forever" and stops the lookup. Group
rooms have no single contact, so they use `per_room` then `global`.

### Redaction patterns

Patterns match against the message body stored in the bridge database
(`message.metadata.body`). Matching messages are redacted on the next cleanup
run **regardless of age** and even when their media would otherwise be kept.

Presets:

| Preset | What it matches |
|---|---|
| `otp` | One-time / verification / login codes: a 4–8 digit number near a code keyword, or a number followed by `code`/`OTP`/`PIN`/`passcode`. |
| `credit-card` | 13–16 digit card-like numbers, optionally separated by spaces or dashes. |
| `ssn` | US Social Security numbers (`123-45-6789`). |

The `custom_patterns` extension map adds your own RE2 expressions. Preset
names and custom regexes are validated at startup: a typo or an invalid
expression stops the bridge instead of silently disabling redaction.

## How a cleanup run works

Every `interval` (plus a 5 minute grace period after startup) the bridge walks
all Matrix rooms with bridged history and, per room:

1. Fetches the oldest messages older than the resolved retention period
   (bounded by `scan_limit`; deleted rows free up the next run).
2. Fetches the newest messages inside the `scan_window` and checks them
   against the redaction patterns.
3. For each match: sends an `m.room.redaction` for the Matrix event and
   deletes the message row from the bridge database. If redaction fails, the
   row is kept so the next run retries it. Rows with fake event IDs
   (`~fake:`) are only deleted from the database.

Failure in one room does not stop the rest of the run. Only message IDs, room
IDs and counts are logged — never bodies.

### Why both redact and delete

Redacting hides the event content on the homeserver and in clients; deleting
the bridge row stops the message from being re-imported or found again (for
example by tapback lookups or history dedupe). Note that Matrix redaction does
not necessarily delete media from the homeserver's media repository — it
removes the event that references it.

## Commands

```
!sms ttl status                       # configured policy + last run summary
!sms ttl run                          # run now, honouring history_ttl.dry_run
!sms ttl run --dry-run                # report what would be redacted now
!sms ttl run --apply                  # run now even when dry_run: true
```

`ttl` requires bridge admin permissions and is available in the management
room. `ttl run --apply` refuses to run while `enabled: false`.

## Caveats

- Messages older than `scan_window` are no longer checked against the patterns;
  they are still subject to the time-based retention. Increase `scan_window`
  if you need pattern coverage further back.
- `per_room` keys are exact room IDs, not aliases. A rebound room (see
  `rebind-room`) keeps its room ID, so overrides survive rebinding.
- The cleanup deletes bridge-side state. Matrix clients keep their local
  copies of events until they sync the redaction, and the phone keeps its own
  messages.
- `history_ttl` is global to the bridge, not per login.
