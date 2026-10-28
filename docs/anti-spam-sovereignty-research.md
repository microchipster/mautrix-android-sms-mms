<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Anti-spam sovereignty & expansion research

Status: research note, 2026-10-27. Follow-up to [anti-spam-research.md](anti-spam-research.md),
which answered *"would anti-spam need an external service, and can we stay sovereign?"*
This note answers: *what does enabling anti-spam entail today, how can it be expanded, and
what research can make it more sovereign?* No code changes.

"Phase 1c" here means the remaining local-only hardening items after the Phase 1 gate
([anti-spam.md](anti-spam.md), `!sms spam ...`); "Phase 2" means the opt-in, self-hosted
reputation sharing sketched in [anti-spam-research.md](anti-spam-research.md) §4/§6.

## (a) What enabling entails today, precisely

### Gate order

`anti_spam.enabled` (default `false`) is the only switch. Every inbound message then goes
through this order (bridgev2 connector path, `connector/network_client.go`):

1. `fromMe` echo suppression — outgoing messages are never evaluated.
2. Merge routing decides the *home* portal key (`mergeRoute`, `network_client.go:793`).
3. **Block gate** first: `dropBlockedInbound` (`network_client.go:798`) checks
   `portal.UserBlocked` / the existing DM portal's block state and auto-labels genuinely
   new contacts (`connector/labels.go:44`). A blocked sender is dropped here, before spam.
4. **Anti-spam gate**: `spamGate(ctx, homeKey, msg.Sender, msg.ID, msg.Body, ts, group)`
   (`network_client.go:813`, implementation `connector/spam.go:670`).
   `hold`/`block` return immediately: the message is **never queued to Matrix**.
5. Tapback parsing, portal creation, `QueueRemoteEvent`.
6. For `label`/`flag`, `applySpamMessageMetadata` attaches `spam_action`/`spam_score`/
   `spam_reasons` to the converted message (`network_client.go:864`, `spam.go:971`).

Inside `spamGate`, in order: own-number exclusion → known-contact check (existing portal or
DM portal; group threads are "known" for the *unknown-sender policy* but not for content
checks) → ghost labels → persisted contact stats/policy → "vetted" (known/allow label,
overridden by per-contact policy) → first-contact rate observation (only for non-vetted
senders, capped store) → duplicate-body observation (never for group threads) → sender
class + own-history reputation (non-vetted only) → offline feed match → `evaluateSpam`
(`spam.go:209`), whose precedence is:

```
override allow → block (block pattern / class block / override block)
→ hold (override hold / class hold / unknown_sender_policy=hold)
→ flag (score ≥ threshold, duplicate count, first-contact rate)
→ label (unknown-sender policy bridge: notice; policy label: no notice)
→ allow (known/vetted, no signals)
```

Note the phone is the source of truth for message content: app-side filtering (Fossify)
already runs before the bridge sees anything (see (b), item 11), and a `hold` decision only
stops the Matrix copy — the message is still in the phone's provider.

### What leaves the device

Nothing. The anti-spam code performs **no network request**: no HTTP client, no DNS,
nothing is fetched. URLs in bodies are parsed lexically only (`spam.go:314-401`), feed
files are read from the bridge host's filesystem (`store.importFeedFile`,
`spamstore.go:482`), and `spam import` only reads a local path. The only outbound artifact
is the *management-room notice*, which the bridge's existing bot posts into the user's own
Matrix room (no third party), plus local logs/DB rows on the bridge host.

### What is stored, and where

Persistence is the bridge database (SQLite or Postgres — the store switches one column
type per dialect, `spamstore.go:59-62`). Five side tables, all created lazily on first use:

| Table | Contents | Retention |
|---|---|---|
| `anti_spam_rate` | `(sender, ts)` observation per first-contact message; rate limiter state | pruned by window + bounded at 2048 senders |
| `anti_spam_duplicate` | `(truncated SHA-256 of normalized body, sender, ts)`; 128-bit hex digest of lowercase/whitespace-collapsed body (`spam.go:405`) | pruned by window + bounded at 1024 hashes |
| `anti_spam_decision` | last 200 non-allow decisions: ts, sender, message_id, portal_id, action, score, reasons (JSON), dry_run | capped at 200 (`spamRecentCapacity`) |
| `anti_spam_contact` | per sender: first_seen, last_seen, total, flagged, blocked, per-contact policy | kept, updated in place |
| `anti_spam_feed` | imported list entries (bare host/URL host/number), source file name, imported_at | kept until re-import/cleanup |

Also persisted, outside those tables: ghost labels `spam` / `unknown sender` (reused by the
existing `label`/`labels` commands), and per-message `spam_action`/`spam_score`/
`spam_reasons` metadata on the bridged message itself.

In memory only (lost on restart): the recent-decisions mirror (≤200), lifetime counters
(`spam status` counters reset on restart), the pending notice digest (counts/reasons/
senders), and the feed-entry cache (invalidated on import). There is no body store and no
plain message text anywhere in the anti-spam state.

### What is logged, and what notices contain

Decision logs use ids and signal names only:

```
action, sender, message_id, portal_id, score, reasons (pattern:/url:/class:/…), dry_run
```

`allow` decisions are counted (in memory) but not logged and not stored. Failures log the
sender/message id; feed imports log path/source/added/total counts. Message bodies never
appear in logs.

Management-room notices (`formatSpamNotice`, `spam.go:938`) contain the canonical sender,
action, reasons and score, plus a hint — never the body:

- `Anti-spam: blocked message from +1… (url:shortener, score 12); it was not bridged.`
- `Anti-spam: held message from BANK (class:alphanumeric, score 8); it was not bridged.`
- `Anti-spam: flagged message from +1… (pattern:urgency, score 10). Use `spam recent` for details.`
- `Anti-spam: new sender +1… labeled "unknown sender". Use `label +1… allow` to allowlist.`

`notices.mode` is `immediate` (one per decision), `digest` (one periodic summary: counts
per action, top 5 reasons, top 5 senders, interval, "Use `spam recent` for details"), or
`off`. Dry runs are prefixed `(dry run)` and never enforce a label/drop.

### Commands (admin-only, management room)

`spam status` (effective config, counters, overrides, feed state, pending digest),
`spam recent [n]`, `spam why <number>` (class, override, labels, allowlist, known state,
rate/duplicate windows, history, reputation, feed matches, last 5 decisions, plus the
verdict a new empty message would get), `spam policy <number> <allow|bridge|label|hold|block|clear>`,
`spam import <file>`.

## (b) Expansion ideas, ranked by impact/effort

Impact/effort are relative to a self-hosted single-admin bridge. "S" ≈ days, "M" ≈ 1–2
weeks, "L" ≈ a fork-sized change.

| # | Idea | Impact | Effort | Notes |
|---|---|---|---|---|
| 1 | **Quarantine UX for first contact** — stop *dropping* held messages: keep a small spool or a dedicated quarantine room/space, with `Allow`/`Block` actions; "Allow" re-imports the held message from the phone provider (it is still there — the app inserts before forwarding) | High | M–L | Prior art: Signal message requests, iOS "Screen Unknown Senders". Decide room-vs-spool first: a quarantine room needs no message store but splits the conversation; a spool gives one clean DM but needs replay-by-id via history sync. Current `hold` is a silent drop from Matrix's perspective. |
| 2 | **Per-contact policy UI** — buttons/reactions on notices and a `spam policies` list, instead of typing `spam policy <num> …` | High | S | The policy model, persistence and precedence already exist. Keep admin-only; buttons must map to existing commands so audit trails stay the same. |
| 3 | **Sender-class default policies** — first-class defaults per class (e.g. `alphanumeric → hold`, `shortcode → label`, `international → label`) independent of per-class weights/hooks | High | S–M | `hold_classes`/`block_classes` already exist; this is mostly validation, docs and `spam why` surfacing. Big win: alphanumeric IDs are spoofable (ECC/REC/(25)04), but bank shortcodes are legit — keep allowlist escape hatches. |
| 4 | **Rolling per-number velocity** — sliding-window message counts for *all* senders (not just first contact), flag on bursts | Medium | S | Reuse `anti_spam_rate` (widen the write scope), add `velocity: {limit, window}`; advisory flag only; keep group-aware. |
| 5 | **Link-frequency + lexical hardening** — `anti_spam_url` table (host, sender, ts), flag a host seen from ≥N distinct senders in a window; add IP-literal, punycode and mixed-script host detection (UTS #39) next to the existing shortener/TLD checks | Medium | M | Frequency is the only offline substitute for domain age/reputation. UTS #39 restriction-level skeletons catch homoglyph domains; keep flag-only and test on legit IDNs. |
| 6 | **Language/script signals** — flag when the body's language/script does not match the user's declared languages, or when a single message mixes scripts unnaturally | Medium | M | fastText `lid.176.ftz` is 917 kB, 176 languages (CC BY-SA 3.0) and easy to run on the bridge; Unicode script ranges are a zero-dependency fallback. Language is a weak signal — flag only, tune threshold per user. |
| 7 | **Local reports store** — `spam report <number> [category]`, `spam reports`, TTL, optional label; never sent anywhere | Medium | S | The truthful, consent-based input for Phase 2. Store number/category/timestamp only, no bodies; export format must be stable and previewable. |
| 8 | **`spam why` v2 / JSON output** — per-signal weight contributions, allowed-decision visibility, `--json` for dashboards | Medium | S | Current `why` shows the total score but not each signal's contribution; a JSON mode also makes acceptance tests machine-checkable. |
| 9 | **Digest batching v2** — persist or recompute the pending digest so it survives restarts (currently lost); quiet hours; top-N/severity grouping | Low–Med | S | Recompute from `anti_spam_decision` since the last flush is simpler than a new table; dedupe on restart. |
| 10 | **Offline feed imports v2** — feed metadata/expiry, per-feed weights, local false-positive suppression, hostfile+RPZ import formats, `spam feeds` list/remove | Medium–High | M | Depends on legally clean sources (see (c)): Phishing.Database (MIT), URLhaus (Auth-Key, fair use), OpenPhish community (non-commercial). **Existing doc bug:** the example `https://urlhaus.abuse.ch/downloads/text/` now returns `410 Gone`; URLhaus downloads require an Auth-Key. Never fetch from the bridge; admin cron/systemd-timer fetch stays. |
| 11 | **On-device pre-forward filtering (Fossify fork)** — see below; strongest privacy story but large surface | High | L | The base app already drops keyword/blocked/unknown-number messages *before* insert and notification; the fork can add quarantine, bridge sync and hooks. |
| 12 | **Fossify-side hooks** — forward filter verdicts to the bridge (audit/quarantine mirror), push bridge blocks into the app + Android blocked-number list | Medium–High | M | Two sources of truth need explicit sync rules and an undo path; mirroring into the system block list also blocks calls, which must be opt-in. |

### What the Fossify app can already do before the bridge sees a message

Verified against upstream `FossifyOrg/Messages` (hook points in
[fork-notes.md](fork-notes.md) §2/3):

- `SmsReceiver.onReceive` checks, before inserting anything:
  - `isMessageFilteredOut` — case-insensitive **keyword substring** filter backed by
    `config.blockedKeywords` (`ReceiverUtils.kt:8`, `Config.kt:82`),
  - `isNumberBlocked(address)` — app/system block list,
  - `baseConfig.blockUnknownNumbers` — blunt "only contacts" mode.
  All three `return@ensureBackgroundThread`: the message never reaches the provider, the
  Room DB, a notification, or the bridge hook.
- `MmsReceiver.isAddressBlocked` / `isContentBlocked` call the same helpers through the
  klinker receiver.
- The fork's forward hook sits *after* those checks (SMS after `messagesDB.insertOrUpdate`,
  MMS at the end of `handleMmsMessage`), so the bridge today only ever sees messages the
  app didn't already drop.

Implication: app-side filtering is **destructive and invisible** (no record for the user),
while the bridge gate is explainable and reversible. A fork "quarantine" mode should insert
+ keep the thread but skip notification/forward, then expose allow/block; otherwise bridge
policy and phone behavior will disagree. Two known fork bugs to fix first: `mms.attachment!!`
crashes for text-only MMS, and attachment filtering never inspects the image content.

## (c) Sovereignty and self-hosting angles

### Fully offline ML: feasible, but keep it small and advisory

| Approach | Footprint | Runtime | Training data | Verdict |
|---|---|---|---|---|
| Hashed char/word n-gram logistic regression or gradient boosting | <1 MB weights | pure Go, microseconds/message | train offline (e.g. UCI SMS Spam Collection, 5,574 messages, **CC BY 4.0**) + local decisions | Best first step. Pure Go keeps the bridge CGO-free; weights + training script live in the repo and are auditable. |
| fastText supervised classifier | <1 MB quantized | fastText/go bindings | same | Equivalent; also gives `lid.176.ftz` (917 kB) for language ID. |
| Averaging word embeddings (TFLite Model Maker) | <1 MB | LiteRT | task-specific labels | Fits app-side if a model must ship. |
| MobileBERT / TinyBERT int8 | ~25 MB / ~15 MB | LiteRT (app) or ONNX (bridge) | needs fine-tuning data | Possible but overkill for SMS text; APK bloat and battery cost. |
| BERT-Base / cloud LLMs | 300 MB+ / external | — | — | Not acceptable: API calls leak bodies; local large models are impractical. |

Caveats: the public corpus is 2012-era English (UK/Singapore bias); spam tactics and
languages drift; accuracy is not the metric — false-positive rate is; numbers stay
spoofable, so ML never fixes day-one campaigns. Run any model in `dry_run` first and keep
it flag-only. If the app filters before forwarding, a bridge model never sees that traffic —
models trained on "what the bridge sees" are biased by app policy.

Placement: train/evaluate on the bridge (cheap CPU, easy tuning, one place per admin), keep
the phone app to cheap explainable filters plus, optionally, a frozen model version shipped
with the APK or transferred over the existing paired app channel (never fetched from the
internet by the app). OCR for image-only MMS is feasible offline — Tesseract/Tesseract4Android
(Apache-2.0; ~14 MiB example F-Droid app plus traineddata) — but costs CPU/battery; a
bridge-host tesseract is the pragmatic place if it is ever wanted.

### Refining the opt-in HMAC + k-anonymity design

[anti-spam-research.md](anti-spam-research.md) §4 is the baseline. Concrete refinements:

- **Replace the client-verifiable HMAC with an OPRF/VOPRF** (RFC 9497, Dec 2023): the
  collector never sees the plaintext number and the client cannot brute-force the pepper.
  Alternatively issue tokens per Privacy Pass (RFC 9578, Jun 2024; RFC 9577 for the HTTP
  auth scheme) so submissions are unlinkable and rate-limitable.
- **Anonymous rate limiting instead of account identity** — Privacy Pass issuance gives
  one-time tokens without linking requests; ARM/ARC-style credentials can cap daily
  reports per client. This is the Sybil-resistance primitive; thresholds alone are not.
- **k-anonymity hygiene** — keep the HIBP prefix model (client-side compare, padded
  responses), but measure real bucket occupancy over the NANP/E.164 space; a 5-hex prefix
  may be too narrow once a local collector has few numbers. Never let a lookup return less
  than k contributors' data.
- **Transport** — OHTTP (RFC 9458) through a relay so the collector never learns client
  IPs; batch submissions with random delay; pad request/response sizes.
- **Lifecycle rules** — only surface a number after N distinct contributors in a window;
  decay reports (half-life, e.g. 90–180 days) because numbers get recycled; provide
  unreport/appeal; cap contributions per client; publish aggregate counts, not lists of
  numbers.
- **Self-hosted topology** — 2a: *no server at all* — a group of bridges shares a secret
  pepper and exchanges signed, truncated, k-anonymized bundle files offline (same import
  path as feeds); 2b: one self-hosted collector for a household/community, off by default,
  with the policy from above; 2c (not recommended as default): third-party collectors or
  APIs, per-query consent only. Optional differential privacy (e.g. RAPPOR) applies only if
  aggregates are ever published.
- **Auditability** — per-user switch; "what would be sent" payload preview; local mode
  stays fully functional; collector deletes raw reports after aggregation and logs no IPs;
  every submission is visible in the local report store first.

### DNSBL / feed reality (verified 2026-10-27)

| Source | What you get | Can it be mirrored/self-hosted? |
|---|---|---|
| [Spamhaus DBL](https://www.spamhaus.org/blocklists/domain-blocklist) | malicious **domains** | **No mirror.** Free only for low-volume, non-commercial use from an identifiable network; queries from public resolvers are rejected (`127.255.255.254`); commercial use needs a subscription. DNS lookups also leak to recursive resolvers. If ever used: admin-side bulk import, not live queries. |
| [URLhaus](https://urlhaus.abuse.ch/api/) (abuse.ch) | malware URLs, hostfile, RPZ, plain-text | **Private local copy only.** Downloads now require an abuse.ch Auth-Key; fair use is not-for-profit, commercial may need a paid subscription, no AI training, no redistribution. `https://urlhaus.abuse.ch/downloads/text/` is **410 Gone** — the example in [anti-spam.md](anti-spam.md) is stale. |
| [OpenPhish community feed](https://github.com/openphish/public_feed) | phishing URLs, 12 h updates | **No mirror.** Free but non-commercial, no redistribution/derivatives; a personal import may be acceptable under the terms, but treat it as opt-in and do not rehost it. |
| [Phishing.Database](https://github.com/Phishing-Database/Phishing.Database) | phishing domains/URLs/IPs, active+inactive, validated | **Yes — best candidate.** MIT, bulk `ALL-phishing-*.tar.gz` dumps (single-digit MB) fit the existing line-import path; no per-query service. |
| [PhoneBlock](https://phoneblock.net/phoneblock/) | spam **callers**, community, GPL-3, self-hostable | Good architectural prior art (opt-in reporting, hashed API, self-host), but call-focused and SMS has no equivalent feed. |
| SMTP DNSBLs / RBLs generally | IPs/domains for mail | Wrong identifier space for SMS sender E.164; only domain lists help with links. |

### Why "no third party by default" stands

1. Phone numbers and message bodies are high-sensitivity, low-entropy identifiers; every
   query leaks metadata to someone even when the content is hashed (NDSS 2021; HIBP model
   exists precisely because bare hashes are invertible).
2. Licenses: the best data (URLhaus, OpenPhish) prohibits redistribution and may charge for
   commercial use; a bridge feature must not depend on terms that can change.
3. Query leakage and outages: DNS/API lookups reveal the user's resolver/IP and time, and
   the URLhaus endpoint retirement shows external contracts can disappear overnight.
4. Supply chain/auditability: a local file the admin fetched is inspectable, diffable, and
   does not silently change; a live service does.
5. Function: external reputation cannot be a delivery dependency — SIM farms and number
   rotation defeat it exactly when it matters most, so the local gate must stay complete.

## (d) What cannot be improved without external data

- **Global/cross-user reputation**: first-seen-globally, "this campaign hit 10k numbers
  today", cross-carrier similarity — impossible locally, by definition.
- **Verified sender identity**: SMS has no SHAKEN-style attestation; alphanumeric sender
  IDs are spoofable; RCS verified senders need the Google/GSMA ecosystem (out of scope).
- **Reverse-lookup names** (CNAM/Truecaller-class): no caller-name field in SMS.
- **Number lifecycle**: reassignment/recycling history and porting state; only local decay
  and observation are possible offline.
- **Large-scale URL/domain reputation, campaign clustering, image/OCR-at-scale models,
  multilingual corpora**: bulk lists can be imported, live scoring cannot be reproduced
  offline.
- **Carrier/aggregator verdicts**: intentionally opaque; even CPaaS senders cannot read
  them reliably.
- **Reporting loops (7726/GSMA/regulators)**: one-way in practice; no per-user feed comes
  back, and voluntary industry rules (CTIA/A2P) are context, not queryable data.

## (e) Proposed roadmap: Phase 1c / Phase 2 with test criteria

Assumption: Phase 1c = finish local-only (no network, no new third-party), Phase 2 =
opt-in self-hosted sharing, split into 2a (offline bundles) and 2b (collector). Every item
must keep: body never logged/stored; no fetches from the bridge; admin-only commands; all
network features off by default.

### Phase 1c (local-only)

| # | Item | Test criteria |
|---|---|---|
| C1 | **Quarantine UX v1** — decide room-vs-spool; `hold` first-contact messages surface as an actionable notice ("Allow"/"Block") instead of silence; "Allow" replays the message (history-sync by id) once | Unit: hold decision stores exactly one spool row/marker; allow marks it consumed; replay emits exactly one Matrix event; block never replays. Acceptance (`scripts/validate/t21_spam_local.sh`): new number → no room event until Allow; after Allow exactly one event, same room id; second message flows normally. Privacy: spool contains ids only. |
| C2 | **Policy UI v1** — `spam policies` listing + reaction/button actions on recent notices; mapping to `spam policy` | Unit: action → policy mapping table incl. `clear`; command output stable. Acceptance: act on a notice → `anti_spam_contact.policy` updated; `spam why` reflects source; non-admin action rejected. |
| C3 | **Sender-class defaults** — config defaults + docs for `alphanumeric`/`shortcode`/`international`; validation errors for bad defaults | Unit matrix: class × vetted × override; `hold_classes`/`block_classes` still win; allowlist beats class hold. Acceptance: alphanumeric unknown is held, allowlisted alphanumeric bridges; `spam status` lists defaults. |
| C4 | **Velocity windows** — `velocity: {limit, window}` sliding per-sender count for all senders | Unit: burst flag at limit+1, window expiry resets, group-aware, vetted senders exempt or configured. Acceptance: N rapid messages → only the (limit+1)-th flagged; `spam recent` shows `velocity`. |
| C5 | **URL hardening + frequency** — IP-literal/punycode/mixed-script host flags; `anti_spam_url` host-frequency flag | Unit: ~20-case URL table incl. benign IDN false-positive guard; frequency counts distinct senders, expires by window, capped like duplicate store. No network calls (code audit). |
| C6 | **Language/script flag** — optional language list; flag mismatched or suspiciously mixed script (flag-only) | Unit: language detection fixtures (fastText/script fallback), English/Spanish/mixed; disabled by default; weights configurable. Perf: bounded per-message time; model file optional at startup. |
| C7 | **Local reports** — `spam report <number> [category]`, `spam reports`, TTL, optional export | Unit: rows contain number/category/ts only; TTL pruning; admin-only; export format golden-tested and previewable. Privacy: grep of logs/DB/rooms finds no body. |
| C8 | **`spam why`/status v2** — per-signal contributions and `--json` | Unit: golden JSON output for a fixed decision; status/why text keeps existing fields (no breakage). Acceptance: assert via `--json` instead of screenshots. |
| C9 | **Digest persistence** — pending digest survives restart (recompute from decision log) | Unit: decisions before restart included exactly once after restart; no duplicate notices; quiet-hours honored. |
| C10 | **Feed import v2** — metadata/expiry, per-feed weight, suppression list, hostfile/RPZ parser; fix the stale URLhaus example | Unit: URLhaus text, hostfile, RPZ, plain-host fixtures; subdomain matching; suppression wins; expired feeds excluded and reported in `spam status`. Acceptance (`t22_spam_feeds.sh`): import a fixture, message scores once per signal, re-import is idempotent. |
| C11 | **Fossify pre-forward v1** — quarantine mode (insert without notify/forward) for blocked keywords/numbers, fork hooks emit filter verdicts, bridge→app block sync (opt-in, incl. system block list) | App unit tests for filter decisions; end-to-end: bridge `spam policy block` propagates to the app block list and back; no loops; own numbers/contacts never pushed; unblock removes. Ensure text-only MMS path does not crash (`attachment!!`). |

### Phase 2 (opt-in, self-hosted)

| # | Item | Test criteria |
|---|---|---|
| P1 | **Design freeze** — threat model, payload schema (HMAC/OPRF inputs, coarse classes only), bucketing, thresholds, decay, appeals, key management, collector ops | Review checklist complete; spec reviewed against [anti-spam-research.md](anti-spam-research.md) §4; no code. Explicitly document what a malicious collector/client can learn. |
| P2 | **2a offline bundle exchange** — shared-pepper signed bundles; `spam export --anonymized` / import via the feed path | Integration test with two bridge DBs: same pepper → match after import; different pepper → no match; tampered bundle rejected; expired entries ignored; no plaintext numbers in the bundle (decode test). |
| P3 | **2b collector MVP** — report/lookup endpoints, k-anonymity prefix ranges, thresholds, decay, appeal, admin view; off by default | Go protocol tests (padding, malformed inputs, prefix width); docker-compose integration: 3 simulated contributors, below-threshold lookup returns nothing; threshold reached → match; decay via injected clock; appeal removes; replay rejected. |
| P4 | **2b hardening** — Privacy Pass-style anonymous rate limits, OHTTP relay, differential-privacy aggregates (optional), per-report consent preview | Token replay rejected; tokenless submissions capped; collector access logs contain no client IPs (relay mock); payload preview golden test; feature disabled by default fails closed (no egress). |

Cross-cutting acceptance rule for every item: run `go build ./... && go vet ./... && go test ./...`,
then the validation harness with egress blocked, and grep the bridge DB + logs + management
room for message-body substrings: none may appear. The anti-spam code must remain free of
`net/http` except where a feature is explicitly an opt-in export (Phase 2), and the bridge
must never fetch feeds or links itself.

## Sources (new or re-verified; baseline citations in [anti-spam-research.md](anti-spam-research.md))

- URLhaus: [API docs](https://urlhaus.abuse.ch/api/) (Auth-Key required; RPZ/hostfile/plain
  text; `/downloads/text/` now 410), [FAQ](https://urlhaus.abuse.ch/faq/) (phishing not
  accepted), [abuse.ch Terms of Use](https://abuse.ch/terms-of-use/) (fair use, commercial
  subscriptions, no AI training/redistribution).
- Spamhaus: [DNSBL usage FAQ](https://www.spamhaus.org/faqs/dnsbl-usage),
  [blocklists](https://www.spamhaus.org/blocklists/domain-blocklist) (free = low-volume
  non-commercial, identifiable network; public-resolver queries rejected).
- OpenPhish: [community feed repo](https://github.com/openphish/public_feed) (12 h,
  non-commercial), [terms](https://openphish.com/terms.html).
- Phishing.Database: [repository](https://github.com/Phishing-Database/Phishing.Database)
  (MIT, bulk dumps, active/inactive).
- PhoneBlock: [project](https://phoneblock.net/phoneblock/),
  [integrations](https://github.com/haumacher/phoneblock/blob/master/INTEGRATIONS.md).
- Offline ML: [TFLite Model Maker text classification](https://developers.google.com/edge/litert/libraries/modify/text_classification)
  (model sizes), [fastText language ID](https://fasttext.cc/docs/en/language-identification.html)
  (917 kB, CC BY-SA 3.0), [UCI SMS Spam Collection](https://archive.ics.uci.edu/dataset/228/sms+spam+collection)
  (5,574 messages, CC BY 4.0), [Tesseract4Android OCR example](https://f-droid.org/en/packages/io.github.subhamtyagi.ocr).
- Primitives: [RFC 9497](https://www.rfc-editor.org/info/rfc9497) (OPRF),
  [RFC 9578](https://www.rfc-editor.org/info/rfc9578) and
  [RFC 9577](https://www.rfc-editor.org/info/rfc9577) (Privacy Pass),
  [RFC 9458](https://www.rfc-editor.org/rfc/rfc9458) (OHTTP),
  [HIBP k-anonymity](https://haveibeenpwned.com/API/v3#SearchingPwnedPasswordsByRange),
  [RAPPOR](https://arxiv.org/abs/1407.6981).
- Text/domain signals: [Unicode UTS #39](https://www.unicode.org/reports/tr39)
  (mixed-script/restriction-level detection).
- App-side filtering: `FossifyOrg/Messages` `ReceiverUtils.kt`, `SmsReceiver.kt`,
  `MmsReceiver.kt`, `Config.kt` (blocked keywords, blocked numbers, `blockUnknownNumbers`),
  plus [fork-notes.md](fork-notes.md) hook points.
