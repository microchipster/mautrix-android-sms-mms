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
dry-run mode. Decisions, first-contact/duplicate state, per-contact policies,
own-history counters and imported feed entries are stored in small side tables
of the bridge database, so they survive restarts.

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

    # Sender classes: the weight is added for messages from unknown senders
    # of that class. hold_classes puts unknown senders of the class into the
    # hold path; block_classes drops them like a block pattern.
    sender_classes:
      shortcode_weight: 4
      toll_free_weight: 3
      alphanumeric_weight: 6
      international_weight: 3
      hold_classes: []
      block_classes: []
      # E.164 calling code of your home country ("1", "+1", "49"); numbers
      # outside it are classified as international. Empty disables that
      # classification.
      home_country: "1"

    # Own-history reputation: the bridge's recorded history with a sender
    # contributes a score when the sender is not vetted (known/allowlisted).
    reputation:
      weight: 2             # multiplier; 0 disables reputation scoring
      flag_weight: 1        # factor per previously flagged/held message
      block_weight: 2       # factor per previously blocked message
      volume_threshold: 10  # msg/day above this adds to the factor
      max_score: 20         # cap of the reputation contribution

    # Local line-based feed files (domains or sender numbers, one per line).
    # The bridge never downloads anything; fetch the files yourself and point
    # this at them (see "Offline feeds" below).
    feeds:
      paths: []
      weight: 6

    # Management-room notices:
    #   immediate - one notice per decision (default)
    #   digest    - one summary every digest_interval
    #   off       - no notices
    notices:
      mode: immediate
      digest_interval: 1h
```

`patterns: []`, `allow_labels: []`, `urls.suspicious_tlds: []` and
`urls.shorteners: []` explicitly clear the built-in defaults. The legacy
`notices.management: false` is still accepted and maps to `notices.mode: off`.

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

## Sender classes

The classifier works on the canonical sender address and never needs a
network lookup:

| Class | Shape | Default weight |
|---|---|---|
| `shortcode` | 3-6 digits (e.g. `55555`) | 4 |
| `toll_free` | NANP toll-free (`+1` 800/833/844/855/866/877/888) | 3 |
| `alphanumeric` | sender ID such as `BANK` (spoofable) | 6 |
| `international` | E.164 number outside `home_country` | 3 |
| `home` | E.164 number inside `home_country` | 0 |

Class weights are added only for senders that are **not** known, allowlisted
or `spam policy`-allowed. `hold_classes` routes unknown senders of that class
into the hold path; `block_classes` drops them like a block pattern. Both are
applied before scoring, and the class name appears in the decision reasons
(`class:alphanumeric`).

## Own-history reputation

For every gated message the bridge records the sender, first/last-seen time
and counters (total, flagged/held, blocked). The reputation contribution is:

```
factor = flagged * flag_weight + blocked * block_weight
       + max(0, messages_per_day - volume_threshold)
score  = min(factor * weight, max_score)     # max_score 0 = uncapped
```

The first day counts as a full day, so a same-day burst is not diluted. Like
the class weights, reputation only applies to senders that are not vetted.
`spam why <number>` prints the counters, the messages/day value and the score
contribution.

## Offline feeds

`feeds.paths` points at local files with one entry per line: bare domains,
URLs (reduced to their host on import), phone numbers or short codes.
`#`/`;` comment lines and blank lines are ignored, `*.` prefixes are stripped
and URLhaus-style `id,http://…` lines work too. Matching adds `feeds.weight`
once per signal (`feed:sender` for the sender, `feed:url` for any URL host,
including subdomains) and never fetches anything.

Import the configured files at startup, or run a manual import:

```
!sms spam import /var/lib/mautrix-android-sms-mms/feeds/urlhaus.txt
```

Fetching is deliberately manual, e.g. with a cron job or systemd timer that
downloads the list and then asks the bridge to import it. Example systemd
timer setup on the bridge host:

```ini
# /etc/systemd/system/anti-spam-feed.service
[Service]
Type=oneshot
ExecStart=/usr/bin/curl -fsSL -o /var/lib/mautrix-android-sms-mms/feeds/urlhaus.txt \
  https://urlhaus.abuse.ch/downloads/text/
```

```ini
# /etc/systemd/system/anti-spam-feed.timer
[Timer]
OnCalendar=daily
Persistent=true
```

The bridge never performs the download itself; the command/startup import
only reads the local file.

## Decision table

The gate runs on every inbound message after the block gate
(`block-user`/`unblock-user`) and before the portal is created or the message
is queued for Matrix. Outgoing (from-me) messages, history backfill and the
phone's own numbers are never evaluated.

| # | Condition | Action | Result |
|---|---|---|---|
| 1 | Per-contact policy `allow` | `allow` | Message bridged unchanged, no scoring |
| 2 | Block pattern, `sender_classes.block_classes` match, or per-contact policy `block` | `block` | Message dropped, sender labeled `spam`, notice sent |
| 3 | Unknown sender with per-contact `hold`, `hold_classes` match, or `unknown_sender_policy: hold` | `hold` | Message dropped, sender labeled `spam`/`unknown sender`, notice sent |
| 4 | Score ≥ `threshold` (keyword patterns, URL signals, class/reputation/feed weights) | `flag` | Message bridged, sender labeled `spam`, notice sent |
| 5 | Body hash seen from ≥ `duplicates.count` distinct senders in the window | `flag` | Message bridged, sender labeled `spam`, notice sent |
| 6 | Unknown sender over `first_contact.limit` messages per window | `flag` | Message bridged, sender labeled `spam`, notice sent |
| 7 | Unknown sender, policy `bridge` (or no per-contact override) | `label` | Message bridged, sender labeled `unknown sender`, notice sent |
| 8 | Unknown sender, policy `label` | `label` | Message bridged, sender labeled `unknown sender`, no notice |
| 9 | Known contact or allowlisted label, no signals | `allow` | Message bridged unchanged |

Precedence: 1 → 2 → 3 → 4/5/6 → 7/8 → 9. A sender is "known" when a
DM/thread portal already exists for them or their ghost carries an
`allow_labels` entry; per-contact policies win over both the known state and
the global `unknown_sender_policy`. Group threads only get content checks
(patterns, URLs, feeds) — one unknown member never holds the whole thread.

With `dry_run: true` no label, drop or portal state changes are applied: the
message keeps bridging and the notice is prefixed with `(dry run)`. Dry-run
decisions also do not count towards the sender's reputation.

## Notices

`notices.mode` controls the management-room notices:

- `immediate` (default): one notice per flagged/held/blocked message (and for
  new senders when the policy is `bridge`).
- `digest`: no per-message notices; every `digest_interval` the bridge posts
  one summary with the decision counts, the top reasons and the top sender
  ids. Only counts, pattern names and addresses are included, never bodies.
- `off`: no management notices at all.

`spam status` shows the pending digest size; a pending digest is lost on
restart (the underlying decisions are still in the persisted recent log).

## What is recorded

- **Ghost labels** (`spam`, `unknown sender`) via the existing labels metadata,
  managed with the `label`/`labels` commands. Label a sender `family`,
  `service`, `allow` or `allowed` to exempt them from the unknown-sender
  policy.
- **Message metadata**: `spam_action`, `spam_score` and `spam_reasons` (pattern
  names and signal names, never the body) are stored on the bridged message.
- **Logs**: decisions log the action, score, reasons, canonical sender,
  message ID and portal ID — never the body.
- **Side tables** in the bridge database, pruned by age or capped:
  `anti_spam_rate` (first-contact observations), `anti_spam_duplicate`
  (body-hash/sender pairs), `anti_spam_decision` (recent decisions, capped at
  200), `anti_spam_contact` (first/last seen, totals, flagged/blocked and the
  per-contact policy) and `anti_spam_feed` (imported feed entries).

## Commands

```
!sms spam status             # effective config, counters, overrides, feed state
!sms spam recent [count]     # last decisions, newest first (default 20)
!sms spam why <number>       # labels, override, rate/duplicate state, history, verdict
!sms spam policy <number> <allow|bridge|label|hold|block|clear>
!sms spam import <file>      # import a local line-based feed file
```

Per-contact policies are persisted and win over the global policy:

- `allow` — never evaluate the sender (like an allowlist label).
- `bridge` — treat the sender as known: no unknown-sender labels/notices,
  content scoring still applies.
- `label` — force the unknown-sender `label` path even for known senders.
- `hold` — drop messages from the sender (unless a block pattern matches).
- `block` — drop every message from the sender; the sender is labeled `spam`.
- `clear` — remove the stored override.

`spam` requires bridge admin permissions and is available in the management
room.

## Caveats

- Keyword lists go stale and are language-specific; false positives are
  expected. Keep `action: block` for patterns you are sure about, and use
  `dry_run` plus `spam recent`/`spam why` to tune before dropping anything.
- Duplicate detection is advisory (`flag` only) and deliberately group-aware:
  normalized bodies from different senders in one group thread are not
  counted.
- Sender classes and reputation are heuristics, not identities: alphanumeric
  sender IDs are spoofable. `hold_classes`/`block_classes` should be enabled
  deliberately, ideally after a dry-run period.
- Feed entries are only as good as the list you fetch: importing URLhaus text
  does not verify anything, and the bridge stores the list as-is (a plain
  entry per line).
- Local reporting (`spam report`) and the Phase 2 opt-in sharing collector
  are not implemented yet; see
  [anti-spam-research.md](anti-spam-research.md) §6.
