<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Anti-spam research for bridged SMS/MMS

Status: research note, 2026-10-27. No code changes. Question answered: *"would anti-spam
need an external service, and can we stay sovereign/private/anonymous?"*

Hard constraint used throughout: **local-first, self-hosted, no third-party dependency by
default; every network feature opt-in and auditable**. "Sovereign" here means: no data
leaves the user's phone/bridge unless the user explicitly turns a feature on and can see
exactly what is sent.

Short answer: **most useful anti-spam work needs no external service at all.** The
industry protections (carrier filtering, STIR/SHAKEN, 10DLC, RCS/Google, 7726) are either
voice-only, app-specific, business-facing, or invisible to third-party apps - the bridge
can inherit none of them. What is left is a solid local layer (quarantine, scoring,
link analysis, sender history) and an *optional* self-hosted reputation collector. External
reputation services are possible but should stay off by default.

---

## 1. Industry reality: what exists, and what a self-hosted bridge can get

| Layer | What it is | Source | Can the bridge use it? |
|---|---|---|---|
| Voice caller auth | STIR/SHAKEN: originating carriers sign caller ID on IP voice calls, terminating carriers verify | [FCC, "Combating Spoofed Robocalls"](https://www.fcc.gov/call-authentication) | **No.** Voice-only framework. No SHAKEN-style attestation is attached to SMS/MMS PDUs, and the public Android telephony API has no authentication or reputation column ([`Telephony.ThreadsColumns`](https://developer.android.com/reference/android/provider/Telephony.ThreadsColumns)) |
| SMS carrier filtering | Carriers filter inbound SMS with ML scoring on per-number volume, message similarity across the network, and content resemblance to known spam | [Twilio, "SMS Carrier Filtering in the US and Canada"](https://support.twilio.com/hc/en-us/articles/360022449893-SMS-Carrier-Filtering-in-the-United-States-and-Canada) | **Only its effect is visible.** Verizon: "blocks more than a billion text messages every month ... without examining the content of the messages" ([Verizon](https://www.verizon.com/about/account-security/smishing-and-spam-text-messages)). T-Mobile: "Our network automatically blocks spam texts" ([T-Mobile](https://www.t-mobile.com/customers/scam-shield)) |
| Carrier filter verdicts | Verdicts are deliberately opaque: "It is not always possible to know for certain when a message has been filtered by a carrier. In rare cases, carriers may falsely report filtered messages as 'delivered' to prevent spammers from reverse engineering filtering systems" | [Twilio, "How Does Message Filtering Work?"](https://help.twilio.com/articles/223181848-How-Does-Carrier-Filtering-Work-) | **No.** Even CPaaS senders cannot reliably read carrier verdicts; a self-hosted receiver definitely cannot |
| Consumer reporting | `7726` ("SPAM") forwards spam SMS to the GSMA Spam Reporting Service, operated by Cloudmark on behalf of operators; France also uses `33700`; reports are aggregated and can lead to sender blocking | [GSMA press release](https://www.gsma.com/newsroom/press-release/gsma-to-address-spam-and-fraudulent-messaging-threats-for-consumers); [Ofcom 7726 page](https://www.ofcom.org.uk/phones-and-broadband/scam-calls-and-messages/7726-reporting-scam-texts-and-calls) | **No per-user feed.** Reports go to carriers/vendor, not back to the reporter. T-Mobile's "Security Center is a global system, run by a vendor on our behalf ... The information reported to the Security Center may be shared with government agencies" ([T-Mobile support](https://www.t-mobile.com/support/plans-features/help-with-scams-spam-and-fraud)) |
| A2P / 10DLC | US long-code application-to-person traffic must register (Brand + Campaign via The Campaign Registry); unregistered A2P traffic is filtered | [Twilio A2P 10DLC FAQ](https://help.twilio.com/articles/1260800720410-What-is-A2P-10DLC-); [The Campaign Registry](https://www.campaignregistry.com/) | **Not directly applicable to the bridge.** US/CA carriers treat all platform-routed traffic as A2P ([Twilio P2P vs A2P](https://support.twilio.com/hc/en-us/articles/223133807-What-is-P2P-and-A2P-messaging-)); a bridge sending from a real handset is P2P with no registration path of its own. A2P rules matter only as context for *inbound* sender classes |
| A2P conduct rules | Voluntary industry rules: consent/opt-in (including confirm opt-in for recurring messages), opt-out, content restrictions, spoofing section, unwanted-traffic containment | [CTIA Messaging Principles and Best Practices (May 2023)](https://api.ctia.org/wp-content/uploads/2023/05/230523-CTIA-Messaging-Principles-and-Best-Practices-FINAL.pdf) | **Indirectly.** Useful as a naming/policy reference for "service" senders, and as prior art for the local "reply YES to allow" pattern |
| Short codes | Common Short Codes are 5- or 6-digit numbers, registered and monitored under the CTIA handbook | [CTIA Short Code Monitoring Handbook v1.9](https://api.ctia.org/wp-content/uploads/2024/01/CTIA-Short-Code-Monitoring-Handbook-v1.9-FINAL.pdf) | **As a local signal only.** "5-6 digit sender" is a class hint; the registry itself is not queried |
| RCS / Google Messages | Google Messages runs spam/scam detection: standard = on-device ML for unknown senders; enhanced = cloud "Private AI Compute". It uploads URLs to Google for safety checks, checks non-contacts against Google's spammer database, and "Report spam" sends Google the sender's number plus the last 10 inbound messages (used to train AI) | [Google Messages spam protection](https://support.google.com/messages/answer/9327903); [Report spam](https://support.google.com/messages/answer/9061432) | **No, by design.** This project is DeGoogled, no Google account, RCS out of scope ([PLAN.md](PLAN.md) §2.9) |
| RCS business messaging | RBM is processed through Google servers; "Google also has access to RBM message content to apply spam and abuse protections", and keys are used to scan for phishing/malware URLs; ITU-T has RCS-specific spam guidance | [Google RBM data security](https://developers.google.com/business-communications/rcs-business-messaging/terms-and-policies/data-security); [ITU-T X.1238 (12/2025)](https://www.itu.int/rec/T-REC-X.1238/en) | **No.** Requires the Google/GSMA RBM ecosystem |
| Alphanumeric sender IDs | Widely spoofable. EU/UK regulators require operators to corroborate business Sender IDs and keep protected-sender lists | [ECC/REC/(25)04 (16 Oct 2025)](https://docdb.cept.org/document/28656); [Ofcom statement](https://www.ofcom.org.uk/siteassets/resources/documents/consultations/category-3-4-weeks/consultation-combatting-mobile-messaging-scams/main-document/statement-combatting-mobile-messaging-scams.pdf?v=423155) | **As a local signal.** "Alphanumeric sender" should get stricter defaults than an E.164 number |
| Law | FCC rules ban autodialed texts without consent, allow revocation "in any reasonable manner", and (since the 2023 order) require carriers to block texts from numbers on a reasonable Do-Not-Originate list; rules apply even without the National Do Not Call Registry listing | [FCC consumer guide](https://www.fcc.gov/consumers/guides/stop-unwanted-robocalls-and-texts); [FCC 2023 scam-texting order](https://www.fcc.gov/document/fcc-adopts-its-first-rules-focused-scam-texting-0); [Federal Register, Second R&O](https://www.federalregister.gov/documents/2024/01/26/2023-28832/targeting-and-eliminating-unlawful-text-messages-implementation-of-the-telephone-consumer-protection) | **Complaint paths only.** No data feed |
| Android platform | The platform's blocked-number provider discards calls and messages from listed numbers; AOSP's internal SMS filter chain (`InboundSmsHandler`) is scoped to the carrier package, system package and visual-voicemail filter - a normal messaging app cannot register a global SMS filter | [BlockedNumberContract](https://developer.android.com/reference/android/provider/BlockedNumberContract); [AOSP block numbers](https://source.android.com/docs/core/connect/block-numbers); [AOSP InboundSmsHandler](https://android.googlesource.com/platform/frameworks/opt/telephony/+/master/src/java/com/android/internal/telephony/InboundSmsHandler.java); [CarrierServicesSmsFilter](https://android.googlesource.com/platform/frameworks/opt/telephony/+/abc773f97376ce62199678104a9ef6e639c24a17/src/java/com/android/internal/telephony/CarrierServicesSmsFilter.java) | **Yes, the block-list part.** The app can maintain the system block list; no spam *classification* API exists |

**Conclusion for (a):** a self-hosted bridge gets the *post-filter residue* and nothing else:
no carrier verdicts, no SHAKEN attestation, no 7726 feed, no RCS verification. Industry
data cannot be consumed; it can only be complemented.

---

## 2. What this bridge already has

| Capability | State | Reference |
|---|---|---|
| Contact labels (`!sms label <number> <label>`, `!sms labels`, `--clear`) stored on the ghost, never sent to the phone | Done | [caller-id-and-labels.md](caller-id-and-labels.md); `connector/labels.go` |
| Auto `new contact` label on first inbound message that creates a portal; optional management-room notice with the number only, never the body (`auto_label_new_contacts`, `new_contact_notice`) | Done | [caller-id-and-labels.md](caller-id-and-labels.md); `labelNewContact` in `connector/labels.go` |
| Own-number exclusion from labeling/ghosts | Done | `labelNewContact` in `connector/labels.go` |
| Block/unblock through the standard bridgev2 interface (`!sms block-user`, `!sms unblock-user`), stored as `portal.user_blocked` | Done | `connector/labels.go` (`UserBlockingNetworkAPI`); upstream added the interface in [mautrix/go CHANGELOG](https://github.com/mautrix/go/blob/main/CHANGELOG.md) |
| Inbound gate drops blocked senders before Matrix (DM and group portals) | Done | `dropBlockedInbound` in `connector/labels.go`; called at `connector/network_client.go:798` |
| Body pattern engine with `otp`, `credit-card`, `ssn` presets and custom RE2 patterns (currently for TTL redaction, not classification) | Done, reusable | [history-ttl.md](history-ttl.md); `connector/ttlconfig.go` (`patterns`, `custom_patterns`) |
| Per-carrier message stats (`!sms stats`) | Done | [carrier-scoped-stats.md](carrier-scoped-stats.md) |
| CNAM/caller-name for SMS | Not possible; no caller-name field exists in SMS/MMS | [caller-id-and-labels.md](caller-id-and-labels.md) |

Missing (all local): sender scoring, quarantine/hold, rate limits, URL analysis,
sender-class detection, local reporting, per-contact policies, and any reputation
sharing. The current "anti-spam" surface is essentially *label + block*.

---

## 3. Local-first options (no external service)

Ranked roughly by value/effort. None of these requires a network call.

| # | Signal / control | How it works locally | Caveats |
|---|---|---|---|
| 1 | **First-contact quarantine** | Unknown sender's first message (and any MMS attachment) is held in a quarantine room/folder; user taps "Allow". Prior art: Signal message requests (block/delete/accept) ([Signal support](https://support.signal.org/hc/en-us/articles/360007459591-Signal-Profiles-and-Message-Requests)); iOS "Screen Unknown Senders" filters unknown senders into separate lists on-device ([Apple support](https://support.apple.com/guide/iphone/screen-and-filter-texts-iph203ab0be4/ios)) | False positives for one-off human texts; needs a per-contact "always allow" memory and a grace/hold timeout |
| 2 | **Allowlist-first mode** | Only senders with labels like `family`/`service`/explicit "allowed" reach the main room; everything else quarantines. Complements FCC advice not to engage with unknown senders ([FCC guide](https://www.fcc.gov/consumers/guides/stop-unwanted-robocalls-and-texts)) | Requires onboarding UX for the first weeks; group threads with one unknown member need policy (hold member, not the thread) |
| 3 | **Keyword/regex scoring** | Reuse the existing pattern engine (`patterns` + `custom_patterns`, RE2). Score features: urgency words, money/payment, "verify account", gift-card asks, crypto. Tune against a public corpus such as the [UCI SMS Spam Collection](https://archive.ics.uci.edu/dataset/228/sms+spam+collection) (5,574 labeled messages) | Keyword lists go stale and are language-specific; keep scoring explainable ("why" output) and user-tunable |
| 4 | **URL / link analysis** | Extract URLs; flag shorteners, IP-literal hosts, punycode/homoglyph domains, rare TLDs, mismatched link text. SMS phishers commonly use shorteners and cloaking ([Nahapetyan et al., "On SMS Phishing Tactics and Infrastructure"](https://www.cise.ufl.edu/~k.childs/Papers/SMS_Phishing.pdf), IEEE S&P 2024). Optional offline feeds: [URLhaus](https://urlhaus.abuse.ch/api/), [OpenPhish free feed](https://openphish.com/phishing_feeds.html), [Spamhaus DBL](https://www.spamhaus.org/blocklists/domain-blocklist) for domain reputation. A middle option is the [Safe Browsing v4 Update API](https://developers.google.com/safe-browsing/v4), which keeps a local hash-prefix database and only talks to Google on a local prefix match - still external, so opt-in. Never auto-fetch links from the bridge (SSRF and beaconing) | Local feeds need periodic refresh (a network action - keep opt-in); image-only spam (OCR inside MMS) is not feasible locally without ML |
| 5 | **Duplicate/repetition detection** | Same or near-identical body/URL sent to many recipients; repeated sends from the same number in a short window. Carriers themselves score on "how many similar messages have transited the carrier's network" ([Twilio](https://support.twilio.com/hc/en-us/articles/360022449893-SMS-Carrier-Filtering-in-the-United-States-and-Canada)); phishing campaigns are near-identical with URL/phone/OTC fields varied ([SMS phishing paper](https://www.cise.ufl.edu/~k.childs/Papers/SMS_Phishing.pdf)) | Legitimate group MMS and broadcast-ish human messages (party invites) can trip this; make it advisory and group-aware |
| 6 | **Sender class** | Classify by shape: short code (5-6 digits, registered/monitored per [CTIA](https://api.ctia.org/wp-content/uploads/2024/01/CTIA-Short-Code-Monitoring-Handbook-v1.9-FINAL.pdf)), toll-free (8xx), international number, alphanumeric sender ID (spoofable - [ECC/REC/(25)04](https://docdb.cept.org/document/28656)), or plain E.164 | Class is a hint, not a verdict; short codes can be legitimate ("your bank's code") |
| 7 | **Reputation from the user's own history** | Reply ratio, first-seen date, message counts, prior block/unblock, TTL/OTP matches, and user labels stored by the bridge | Cold start per number; the user's own history is tiny compared to global spam |
| 8 | **Per-contact policies** | Per-label defaults (`family` = never hold, `service` = allow but cap volume, unknown = quarantine), per-contact rate caps, quiet hours. Store as bridge config + ghost metadata | Needs a policy model and a management-room UI (`!sms policy ...`) |
| 9 | **On-device filtering (Fossify fork)** | The base app already has blocking, word/phrase filters, and block-list import/export ([FossifyOrg/Messages](https://github.com/FossifyOrg/Messages)). The fork can filter/quarantine before forwarding to the bridge, and mirror bridge blocks into the system block list - the platform then discards calls and messages from that number entirely ([BlockedNumberContract](https://developer.android.com/reference/android/provider/BlockedNumberContract)) | Two sources of truth (app + bridge) need sync; AOSP's system SMS filter hook is not available to normal apps ([InboundSmsHandler](https://android.googlesource.com/platform/frameworks/opt/telephony/+/master/src/java/com/android/internal/telephony/InboundSmsHandler.java)) |
| 10 | **Local reporting only** | `!sms report <number> [category]` writes a local row (number, category, timestamp) and optionally applies a label; `!sms reports` lists them. This is the local version of 7726/GSMA-style reporting ([GSMA SRS](https://www.gsma.com/newsroom/press-release/gsma-to-address-spam-and-fraudulent-messaging-threats-for-consumers)) without telling anyone else | Without sharing, reports only help one user - but they become the truthful, consent-based input for Phase 2 |

Challenge/response ("reply YES to allow") notes:

- Prior art exists on all sides: Signal message requests ([Signal](https://support.signal.org/hc/en-us/articles/360007459591-Signal-Profiles-and-Message-Requests)), iOS unknown-sender screening ([Apple](https://support.apple.com/guide/iphone/screen-and-filter-texts-iph203ab0be4/ios)), and A2P double opt-in confirmation ([CTIA principles](https://api.ctia.org/wp-content/uploads/2023/05/230523-CTIA-Messaging-Principles-and-Best-Practices-FINAL.pdf), §5.1.2).
- Automated SMS challenges must **never auto-reply to short codes or premium-rate numbers** (cost/abuse differences are country-specific; ECC documents premium-rate SMS as a separate category: [ECC/REC/(25)04](https://docdb.cept.org/download/4812)). Keep the challenge manual-tap ("Allow" in Matrix) first; an actual SMS "reply YES" is a second step for real E.164 senders.
- One-time-code senders will never reply but are not necessarily spam; treat OTP-shaped messages as "hold, do not auto-allow" (the existing `otp` preset is a good detector, [history-ttl.md](history-ttl.md)).

---

## 4. Reputation sharing without losing anonymity (design sketch)

### Why plain hashing is not enough

Phone numbers have tiny entropy. The NDSS 2021 study "All the Numbers are US" reversed
hashed phone numbers from WhatsApp/Signal/Telegram contact discovery at scale (10% of US
mobile numbers queried on WhatsApp, 100% on Signal, with "very few resources"), and built
rainbow tables over the non-uniform phone-number space; it concludes "currently deployed
hashing-based contact discovery protocols are severely broken"
([NDSS 2021](https://www.ndss-symposium.org/wp-content/uploads/ndss2021_1C-3_23159_paper.pdf)).
Signal itself says "the hash of a user identifier can almost always be inverted" and uses
a hardware-isolated service instead ([Signal contact discovery](https://signal.org/blog/private-contact-discovery/)).

So: an unsalted or truncated hash of a phone number is **not** privacy. Any design must
assume the whole E.164/NANP space is enumerable offline.

### Practical design (Phase 2, opt-in)

- **Signals only**: per-number coarse class (`spam` / `not-spam` / `unknown`) derived from
  explicit user actions (block, report, unblock), plus an optional category. No message
  bodies, no timestamps finer than a day, no sender/recipient pairs. Reports are never
  automatic.
- **Keyed hash**: `HMAC-SHA-256(number, pepper)` instead of a bare hash. Be honest about
  the limit: any client-verifiable scheme can be brute-forced by a determined client, so
  the pepper only raises the bar. The principled fix is an oblivious protocol (PSI/OPRF,
  as in Signal's contact discovery service and the mitigations in the NDSS paper).
- **k-anonymity lookup**: submit/query by truncated HMAC prefix (the HIBP Pwned Passwords
  model: first 5 hex chars = 1,048,576 buckets, ~800 suffixes returned per query, client
  compares locally; HIBP also pads responses) -
  [HIBP k-anonymity](https://haveibeenpwned.com/API/v3#SearchingPwnedPasswordsByRange),
  [HIBP Passwords page](https://haveibeenpwned.com/Passwords). For the full ~10^10 NANP
  space a 5-hex prefix leaves a large average bucket, but buckets are concentrated on
  allocated numbers - measure the real bucket sizes on your data before trusting k.
- **Transport privacy**: Oblivious HTTP (RFC 9458) through a relay so the collector never
  sees client IPs ([RFC 9458](https://www.rfc-editor.org/rfc/rfc9458)); batch submissions
  with random delay; pad requests/responses.
- **Aggregation rules**: only surface a number after N distinct contributors report it in
  a window (Sybil resistance); decay old reports (numbers get recycled); provide
  unreport/appeal; cap contributions per user; publish counts, not identities.
- **Optional differential privacy**: if you ever publish aggregate statistics, use a
  randomized-response mechanism such as RAPPOR ([arXiv:1407.6981](https://arxiv.org/abs/1407.6981)).
- **Opt-in, auditable**: per-user switch, visible "what we would send" preview, local-only
  mode remains fully functional. Nothing is sent while the switch is off.

### DNSBL/RBL reality check for phone numbers

| Thing | Exists? | Reality |
|---|---|---|
| [PhoneBlock](https://phoneblock.net/phoneblock/) | Yes, call-focused, community-run, GPL-3, self-hostable ([repo](https://github.com/haumacher/phoneblock)) | Lookup is by **unsalted SHA-1** of the number, with `prefix10`/`prefix100` for range blocking ([INTEGRATIONS.md](https://github.com/haumacher/phoneblock/blob/master/INTEGRATIONS.md)). Good model to learn from (opt-in community reporting, hashed API), but the hash is reversible by the server (NDSS) and it is not SMS-specific |
| SMTP-style DNSBLs ([Spamhaus DBL](https://www.spamhaus.org/blocklists/domain-blocklist), [VoIPBL](https://www.voipbl.org/)) | Yes, but wrong identifier space | DBL is domains (useful for link scoring), VoIPBL is IPs (SIP abuse). Neither answers "is +1... a spammer" |
| Do Not Call registries ([donotcall.gov](https://www.donotcall.gov/)) | Yes | Suppression lists for *callers* to scrub against, not a query service for receivers |
| FCC Consumer Complaints | Yes, bulk public data | "Unwanted Calls" dataset includes a "Caller ID Number" field and is published nightly with "no personal information" (names/contact info of complainants); unverified by definition ([FCC data center](https://www.fcc.gov/consumer-complaints-center-data), [dataset](https://opendata.fcc.gov/Consumer-and-Government-Affairs/Consumer-Complaints-Data-Unwanted-Calls/vakf-fz8e)). US-only, complaint bias, negative-only |
| Standard phone-number RBL for SMS | **No** | There is no equivalent of Spamhaus for E.164 numbers that a self-hoster can query |

Privacy tradeoff to note: DNS-based lookups leak queries to recursive resolvers; if a
phone-number RBL ever is used, prefer an HTTPS API over DNS, and never send more than the
k-anonymity prefix.

---

## 5. External services inventory (privacy/sovereignty analysis)

Default answer for every row below: **no**. "Ever acceptable" column describes the
maximum I would consider, and only behind an explicit, per-query opt-in.

| Service | What it gives | What leaves the device | Sovereign/self-hostable | Ever acceptable? |
|---|---|---|---|---|
| **Truecaller** | Reverse lookup / caller ID from a crowdsourced global directory | If installed: address-book access, call/message metadata and keywords, IP/device identifiers ([privacy policy](https://www.truecaller.com/privacy-policy)); policy makes the user confirm others' consent | No | No. Even non-users' numbers appear in the directory. EU-mode claims (no phonebook storage/sharing, no reverse lookup of EU contacts) ([GDPR FAQ](https://www.truecaller.com/privacy/gdpr)) only narrow the practice; criticism of phonebook upload persists ([TNW](https://thenextweb.com/news/truecaller-disturbing-privacy-flaw-leaks-friends-numbers)); company says contacts stay on-device ([myth page](https://www.truecaller.com/busting-myths-about-truecaller)) |
| **Hiya** | Carrier/OEM-grade spam analytics; network-level screening ("AI inside your network. No apps") ([operators](https://www.hiya.com/solutions/operators)) | In carrier deals: call/message metadata inside the operator network; app/website data per [Hiya legal](https://www.hiya.com/legal/privacy) | No | No. Enterprise/carrier-only; requires trusting the carrier-plus-vendor chain |
| **Google Messages spam protection** | On-device standard detection, cloud-enhanced mode, sender reputation, URL checks | URLs of messages from unknown senders; phone numbers of non-contact senders temporarily; spam reports include the number and last 10 messages and train AI ([support](https://support.google.com/messages/answer/9327903), [report spam](https://support.google.com/messages/answer/9061432)) | No (needs Google ecosystem; RCS out of scope here) | No for this project |
| **US carrier services (T-Mobile Scam Shield, AT&T ActiveArmor, Verizon)** | Network-level call/text blocking; 7726 reporting | Report content and numbers to the carrier's vendor ("Security Center"), possibly shared with government agencies ([T-Mobile](https://www.t-mobile.com/support/plans-features/help-with-scams-spam-and-fraud)); 7726 forwarding ([AT&T](https://www.att.com/support/pages/report-call-or-texts/)) | No | Complementary, user-level only. Enable it if you want (free), but the bridge reads nothing from it |
| **Twilio Lookup v2** | Line type intelligence (mobile/landline/VoIP/toll-free) and a proprietary "SMS Pumping Risk" score ([docs](https://www.twilio.com/docs/lookup/v2-api), [pumping risk](https://www.twilio.com/docs/lookup/v2-api/sms-pumping-risk)) | Every queried number leaves the device | No | Per-query, explicit user confirmation, high-risk numbers only - and even then it tells you about fraud campaigns, not SMS spam |
| **IPQS-style phone reputation** | Proprietary spam/abuse score ("numbers ... recently involved with abusive behavior online") ([docs](https://www.ipqualityscore.com/documentation/proxy-detection-api/features-and-use-cases/phone-reputation)) | Every queried number | No | Same as above; default off |
| **Cloud content-classification APIs** (AWS, Google, Azure) | General ML text/image moderation | Message bodies/attachments leave the device | No | No. Providers may promise "not used to train without permission" ([AWS Bedrock privacy](https://aws.amazon.com/bedrock/amazon-models/privacy), [Google Cloud gen-AI privacy](https://services.google.com/fh/files/misc/genai_privacy_google_cloud_202308.pdf)), but the content has still been transmitted; unacceptable for SMS/MMS under this constraint |
| **FCC Consumer Complaints (bulk data)** | Public dataset of complained-about numbers (US) | Nothing per query - it is a downloadable dataset | Public data, can be stored locally | The only "external" source I would allow near Phase 2/3: bulk download, US-only, unverified, never a live query |
| **PhoneBlock** | Community spam-call blocklist with hashed lookups | The hashed number being checked | Self-hostable, GPL-3 | Acceptable as prior art / optional external feed; not SMS-focused |

---

## 6. Recommendation ladder

### Phase 1 - local-only (do this first; no external data)

Implemented in the bridge (see [anti-spam.md](anti-spam.md)):

1. **Done** - unknown-sender handling with explicit allow: `unknown_sender_policy`
   (`bridge`/`label`/`hold`), allowlist labels, per-contact `spam policy` overrides.
   The separate quarantine room/folder UI is still open (see 10).
2. **Partially done** - per-number first-contact rate limit with a management-room
   notice and persisted state; the per-hour *global* burst cap is not implemented.
3. **Done** - explainable scoring engine (keyword patterns + urgency/payment
   heuristics), `!sms spam why <number>` and dry-run mode.
4. **Partially done** - offline lexical URL analysis (shorteners, suspicious TLDs) and
   offline feed import (`feeds.paths` + `spam import`, never fetching). IP-literal and
   punycode checks are still open.
5. **Partially done** - cross-sender exact-body duplicate detection; near-duplicate
   (fuzzy) matching is not implemented.
6. **Done** - sender-class weights and `hold_classes`/`block_classes` hooks (short
   code / toll-free / alphanumeric / international / home).
7. **Done** - own-history reputation counters and persisted per-contact policies,
   surfaced in `spam why` and `spam status`.
8. **Deferred** - on-device filtering in the Fossify fork (pre-forward quarantine, word
   filters, mirror bridge blocks into the platform blocked-number provider).
9. **Deferred** - local reporting (`!sms report`) that stores data only, as the
   Phase 2 opt-in source.
10. **Partially done** - "why" explanations and policy persistence exist; the policy
    editor, quarantine view and per-label defaults are still open.

### Phase 2 - self-hosted, opt-in sharing

1. Build the collector to the §4 design (HMAC pepper + k-anonymity + OHTTP + aggregation
   rules + decay + appeals).
2. Ship it **off by default**; per-report and per-lookup consent; visible payload preview.
3. Reuse the local report store as the only data source; never auto-submit.
4. Keep an offline-only mode: import a coarse public list (e.g., FCC complaints for US)
   instead of querying anyone.

### Phase 3 - external, default off, explicit consent

1. Per-number reputation APIs (Twilio/IPQS/Truecaller-class) behind a per-query
   confirmation dialog that shows exactly what will be sent; never bulk-upload contacts.
2. Carrier/OS features as documentation only ("enable Scam Shield/ActiveArmor on your
   line if you want"); the bridge must not depend on them.
3. Cloud content classification: **not recommended**, even with "no training" promises.

### What cannot be done without external data

- Global/cross-user reputation and first-seen-global scoring: impossible locally by
  definition.
- Verified sender identity: no STIR/SHAKEN for SMS; RCS verified senders need the
  Google/GSMA RBM ecosystem; alphanumeric sender IDs are spoofable
  ([FCC](https://www.fcc.gov/call-authentication), [ECC/REC/(25)04](https://docdb.cept.org/document/28656)).
- Reverse-lookup names (CNAM/Truecaller-class): SMS carries no caller name
  ([caller-id-and-labels.md](caller-id-and-labels.md)).
- Large-scale URL/domain reputation and ML content models (language coverage, image/OCR).
- Carrier/aggregator filter verdicts: intentionally opaque, no API
  ([Twilio](https://help.twilio.com/articles/223181848-How-Does-Carrier-Filtering-Work-)).

And even with external data, number spoofing, SIM farms and number rotation defeat
reputation for the newest campaigns: UK data shows scammers exploiting gaps "to send P2P
messages at scale from SIM farm devices" (ban in progress) and regulators now require
Sender-ID corroboration and know-your-traffic checks
([Ofcom](https://www.ofcom.org.uk/phones-and-broadband/scam-calls-and-messages/new-rules-to-thwart-text-message-scammers-and-protect-consumers-and-businesses),
[Ofcom statement](https://www.ofcom.org.uk/siteassets/resources/documents/consultations/category-3-4-weeks/consultation-combatting-mobile-messaging-scams/main-document/statement-combatting-mobile-messaging-scams.pdf?v=423155)).
First-contact behaviour and content heuristics remain the only defenses that work on
day-one numbers.

---

## Recommended next implementation items (ranked)

Done (see [anti-spam.md](anti-spam.md)): first-contact rate limiting (per number),
scoring engine v1 with dry-run and `spam why`, sender classes/hooks, offline feed import,
duplicate detection, per-contact policies and own-history reputation.

Still open:

1. **Quarantine UX for first contact** (hold first message per new number; "Allow"/"Block"
   in the management room; reuse the `new contact` label as state).
2. **Global burst detector** (bridge-wide message rate cap per hour, alert only).
3. **Fuzzy near-duplicate detection** (normalized bodies plus simple token/character
   distance, advisory score only).
4. **More lexical URL checks** (IP hosts, punycode, mixed-script hostnames) on top of the
   shortener/TLD/feed analysis.
5. **Per-label default policies** (e.g. hold everything labeled `unknown sender`) and a
   policy editor view in the management room.
6. **Local reports** (`!sms report`, `!sms reports`) with TTL and no message bodies - the
   future opt-in input for Phase 2.
7. **Fossify fork hooks** (pre-forward word-filter/quarantine toggle; mirror bridge blocks
   into the Android blocked-number provider so the phone also stops them).
8. **Phase 2 collector spec + prototype** only after 1-7 land: HMAC-pepper + k-anonymity +
   OHTTP + aggregation/decay/appeals, off by default, self-hostable.
