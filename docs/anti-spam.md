<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Anti-spam (local only)

The bridge can score inbound SMS/MMS before they reach Matrix and label,
announce or drop messages that look like spam. Everything runs **locally**:
no URL is fetched, no phone number or message body ever leaves the bridge.
The design rationale and the deferred Phase 2 (opt-in reputation sharing) live
in [anti-spam-research.md](anti-spam-research.md).

The feature is **opt-in** (`anti_spam.enabled: false` by default) and has a
dry-run mode.

## Configuration

All keys live under `anti_spam` in the bridge's network config
(`simple-config.yaml`):

```yaml
network:
  anti_spam:
    # Master switch. The gate does nothing while this is false.
    enabled: true

    # Log and announce decisions without applying labels or dropping
    # messages (default: false).
    dry_run: false

    # What to do with messages from senders the bridge has not seen before:
    #   bridge - bridge the message, label the sender "unknown sender" and
    #            post a management-room notice (default, least destructive)
    #   label  - bridge and label the sender, no notice
    #   hold   - do not bridge the message; label + notice instead
    unknown_sender_policy: bridge

    # Ghost labels that mark a sender as vetted. A sender with any of these
    # labels (case-insensitive) never hits the unknown-sender policy.
    # Contacts that already have a bridged DM/portal are always known.
    allow_labels: [family, service, allow, allowed]

    # Score at which a message is flagged. 0 disables score-based flagging.
    threshold: 10

    # Keyword/heuristic scoring. RE2 expressions, validated at startup.
    # action: flag (default) adds the weight; block drops the message
    # regardless of the score.
    patterns:
      - name: urgency
        regex: '(?i)\b(urgent|act now|immediately|final notice)\b'
        weight: 4
        action: flag
      - name: crypto
        regex: '(?i)\b(bitcoin|usdt|wallet seed)\b'
        weight: 6
        action: block

    # First-contact limiter: an unknown sender may bridge this many messages
    # per window before being flagged. 0 disables the limiter.
    first_contact:
      limit: 5
      window: 10m

    # Offline lexical URL analysis; each signal type adds its weight once per
    # message.
    urls:
      suspicious_tlds: [zip, mov, top, xyz, click, link, tk, ml]
      shorteners: [bit.ly, t.co, tinyurl.com, goo.gl, rb.gy]
      weight: 4

    # Same normalized body hash from at least this many distinct senders
    # inside the window -> flag.
    duplicates:
      window: 1h
      count: 3

    # Management-room notices for labeled/flagged/held/blocked messages.
    notices:
      management: true
```

`patterns: []`, `allow_labels: []`, `urls.suspicious_tlds: []` and
`urls.shorteners: []` explicitly clear the built-in defaults.

### Built-in default patterns

| Name | Signals |
|---|---|
| `urgency` | `urgent`, `act now`, `immediately`, `final notice`, `last warning`, `expires today/soon` |
| `payment` | `wire transfer`, gift/prepaid cards, `money order`, `western union`, `paypal`, `venmo`, `cash app`, `zelle` |
| `crypto` | `bitcoin`, `ethereum`, `crypto`, `usdt`, `wallet address/seed` |
| `prize` | "you've won", `congratulations`, `claim your prize/reward`, "selected as a winner" |
| `credential` | `verify your account`, `confirm your account/identity`, `account suspended`, `unusual activity`, `update your password/payment` |

All default patterns only **flag**; nothing is dropped unless a user-defined
pattern uses `action: block` or `unknown_sender_policy: hold` is set.

## Decision table

The gate runs on every inbound message after the block gate
(`block-user`/`unblock-user`) and before the portal is created or the message
is queued for Matrix. Outgoing (from-me) messages, history backfill and the
phone's own numbers are never evaluated.

| # | Condition | Action | Result |
|---|---|---|---|
| 1 | Any matching pattern has `action: block` | `block` | Message dropped, sender labeled `spam`, notice sent |
| 2 | Unknown sender and `unknown_sender_policy: hold` | `hold` | Message dropped, sender labeled `unknown sender`, notice sent |
| 3 | Score ≥ `threshold` | `flag` | Message bridged, sender labeled `spam`, notice sent |
| 4 | Body hash seen from ≥ `duplicates.count` distinct senders in the window | `flag` | Message bridged, sender labeled `spam`, notice sent |
| 5 | Unknown sender over `first_contact.limit` messages per window | `flag` | Message bridged, sender labeled `spam`, notice sent |
| 6 | Unknown sender, policy `bridge` | `label` | Message bridged, sender labeled `unknown sender`, notice sent |
| 7 | Unknown sender, policy `label` | `label` | Message bridged, sender labeled `unknown sender`, no notice |
| 8 | Known contact or allowlisted label, no signals | `allow` | Message bridged unchanged |

Precedence: 1 → 2 → 3/4/5 → 6/7 → 8. A sender is "known" when a DM/thread
portal already exists for them or their ghost carries an `allow_labels` entry.
Group threads only get content checks (patterns, URLs) — one unknown member
never holds the whole thread.

With `dry_run: true` no label, drop or portal state changes are applied: the
message keeps bridging and the notice is prefixed with `(dry run)`.

## What is recorded

- **Ghost labels** (`spam`, `unknown sender`) via the existing labels metadata,
  managed with the `label`/`labels` commands. Label a sender `family`,
  `service`, `allow` or `allowed` to exempt them from the unknown-sender
  policy.
- **Message metadata**: `spam_action`, `spam_score` and `spam_reasons` (pattern
  names and signal names, never the body) are stored on the bridged message.
- **Logs**: decisions log the action, score, reasons, canonical sender,
  message ID and portal ID — never the body.
- **`spam recent`** keeps the last 200 decisions in memory (IDs only, cleared
  on restart).

## Commands

```
!sms spam status             # effective config + decision counters
!sms spam recent [count]     # last decisions, newest first (default 20)
```

`spam` requires bridge admin permissions and is available in the management
room.

## Caveats

- Keyword lists go stale and are language-specific; false positives are
  expected. Keep `action: block` for patterns you are sure about, and use
  `dry_run` plus `spam recent` to tune before dropping anything.
- Duplicate detection is advisory (`flag` only) and deliberately group-aware:
  normalized bodies from different senders in one group thread are not
  counted.
- First-contact state is in memory: a bridge restart resets the counters.
- Sender classes (short code, toll-free, alphanumeric), own-history
  reputation, per-contact policies, local `report` storage and the Phase 2
  opt-in sharing collector are not implemented yet; see
  [anti-spam-research.md](anti-spam-research.md) §6.
