# App-base research (condensed)

State 2026-10-22. All repos cloned and inspected at HEAD; issue/release data via GitHub API.
Question: which open-source Android SMS/MMS app is the best base for a "bridge mode"
(server reads/writes full-fidelity SMS/MMS incl. group recipients + attachments, sends
incl. group MMS, syncs read state)?

## Verdict

**Fork Fossify Messages (`FossifyOrg/Messages`) + its `mmslib`.** Only candidate that is a
real default-SMS app *with* full MMS pipeline, group recipient sets, provider read/write
(incl. read state), stable IDs, active upstream, GPL-3, F-Droid presence, and a
maintainer precedent (Traccar) for adding a foreground-service HTTP server to this exact
codebase.

`capcom6/android-sms-gateway` is the best **zero-fork interim** gateway if we accept: no
group-recipient list on receive, no read-state writeback, Firebase dep, not F-Droid.
DekuSMS's lib is MIT but its data model is structurally lossy — reject as base.

## Comparison (condensed)

| | Fossify Messages | DekuSMS | capcom6 gateway | KDE Connect | Traccar SMS GW | SmsForwarder | Silence/QKSMS/Simple |
|---|---|---|---|---|---|---|---|
| License | GPL-3 | app GPL-3 / lib MIT | Apache-2.0 | GPL-2/3 | GPL-3 | BSD-2 | GPL-3 |
| Maintained | ✅ v1.10.1 2026-10 | ✅ v0.78.0 2026-10 | ✅ v1.77.4 2026-10, 5.9k★ | ✅ | ✅ v7.0.4 | ✅ 28.5k★ | ❌ dead 2021–2024 |
| Real default-SMS app | ✅ | ✅ | ❌ companion | ❌ | ✅ (Fossify fork) | ❌ | — |
| MMS engine | `org.fossify:mmslib` 1.0.1 (klinker fork) | klinker + lossy Room model | own AOSP PduComposer port | klinker + native PDU | mmslib | none real | old |
| Group MMS read | ✅ thread participants (all addrs) | ❌ drops first addr row, 1 address field | ❌ only type=137 FROM | ✅ to+cc dedupe | ❌ SMS only | ❌ broken | old |
| Group MMS send | ✅ multi-To, but `sendGroupMessageMMS` default false | ❌ `group=false` hardcoded, single recipient | ✅ `phoneNumbers[]` multi-To | ✅ default true | ❌ | ❌ | old |
| Multi-attachment send | ⚠️ sends each attachment as separate MMS | ❌ one part only | ✅ all in one PDU | ✅ | ❌ | ❌ | — |
| Read state writeback | ✅ READ+SEEN sms+mms (Context.kt:996-1044) | ❌ room-local only | ❌ none | provider read only | inherits | ❌ | — |
| Stable IDs | ✅ `Message.getStableId()` | ⚠️ own PK | ✅ per-row messageId | — | inherits | — | — |
| Full PDU export | ✅ `MessagesReader`/`MmsBackup`/`MessagesWriter` | ❌ `TODO()` stubs | 💾 parts files + API | base64 | ❌ | ❌ | — |
| FGS + HTTP API | ❌ (Traccar proves fit; upstream #893 proposal) | FGS but no HTTP (RMQ/SMTP/FTP) | ✅ Ktor + Swagger + webhooks | ❌ | ✅ Jetty | ✅ AndServer | — |
| F-Droid | ✅ | ✅ | ❌ | ✅ | ❌ | ❌ | ✅ (dead) |

## Fossify Messages — top pick, details

- Repo/build: `FossifyOrg/Messages`, GPL-3, `libs.versions.toml`: commons 7.1.3, **mmslib
  1.0.1**, room 2.8.5, AGP 9.4.1, minSdk 26/target 36; Room DB v11; XML views + Kotlin.
- SMS RX: `receivers/SmsReceiver.kt` on `SMS_DELIVER`, concatenates segments, inserts into
  provider then Room.
- MMS RX: `receivers/MmsReceiver.kt` extends klinker `MmsReceivedReceiver`; WAP push via
  `com.android.mms.transaction.PushReceiver`; retrieval via
  `SmsManager.downloadMultimediaMessage` (mmslib `transaction/DownloadManager.java:97`).
- Fidelity: `Context.getMMS()` = msg + `getThreadParticipants()` (all recipients) +
  `getMmsAttachment()` → attachments `content://mms/part/{id}` with SMIL filenames; Room
  `messages.participants` = full list.
- Send: `messaging/Messaging.kt::sendMessageCompat` → `MessagingUtils.sendSmsMessage` or
  `sendMmsMessage` (klinker `Transaction` + `Message(text, addresses[])`); group flag =
  `config.sendGroupMessageMMS` (Config.kt:43 default **false**); system path sets
  `MMS_CONFIG_GROUP_MMS_ENABLED` (mmslib `Transaction.java:673`). Warts to fix in fork:
  group flag default false; N attachments → N MMS messages.
- Read state: `markThreadMessagesRead`/`markMessageRead` write provider READ+SEEN + Room
  (Context.kt:996-1044).
- Byte-exact export already exists: `helpers/MessagesReader.kt` + `models/MmsBackup.kt`
  (base64 parts, typed addresses) + `MessagesWriter.kt`.
- Stable IDs: `Message.getStableId()` (Constants.kt:119) = type<<61 | (id<<1|isMMS).
- Upstream stance: desktop/web integration rejected as out of scope (#665); companion-API
  request **#893** (2026-09-28, AIDL + companion app) open, no comments → fork.
- Group MMS risk list to encode in tests: #744 quick reply → last participant only; #709
  group from Contacts → first member only; #595 reply to unrelated recipient; #167
  text-only group MMS never arrive; #796 duplicate outgoing provider rows; #863 incoming
  MMS spawns new conversation (thread-key mismatch).
- Multi-recipient SMS with group=false inserts a merged `a|b` address row *plus*
  per-recipient rows (`MessagingUtils.kt`) — bridge must filter/merge these.

## DekuSMS — reject as base

Bugs confirmed: `MmsParser.getMmsAddr()` skips first addr row (moveToFirst+while moveToNext)
and concatenates into one string; `MmsParser.getSendMessageSettings()` hardcodes
`settings.group=false` (line 158); `sendMms(Message(text, address))` single recipient
(`SmsMms.kt:399`); mark-read room-local only (`SmsMmsActionsImpl.kt`); export parsers are
`TODO()` stubs. Reusable only: FGS (`dataSync`) and CI signing patterns
(`ci/docker_build.sh`, reproducible-build workflow with `apksigner sign`/`verify`).

## capcom6/android-sms-gateway — interim option

- Apache-2, active. Ktor local server FGS (`modules/localserver/WebService.kt`,
  `foregroundServiceType=connectedDevice`), Swagger, JWT+basic auth w/ scopes, webhooks
  `sms:received`/`mms:received`/`mms:downloaded`, no Firebase needed for local mode
  (verify cold start without GMS).
- Send: `POST /message {phoneNumbers: string[], mmsMessage:{subject,text,attachments[]}}` →
  `MmsSender.buildSendReq` multi-To, `SmsManager.sendMultimediaMessage` with URI perms to
  `com.android.phone`/`com.android.mms.service`; PDU files in `files/mms-out`.
- Receive: `MMSParser` + `MmsReceiver` (WAP push), `MmsContentObserver` download,
  attachments in `files/mms-in/<id>/`, `GET /inbox`,
  `GET /inbox/{id}/attachments/{partId}`, `POST /inbox/refresh` (provider backfill).
- Gaps: receive has single sender/recipient (137 only); no read-state/delete sync; own
  `incoming_messages` DB (not provider truth); Firebase dep; not F-Droid.
- Its `docs/MMS.md` is the best single writeup of Android MMS constraints.

## Other candidates

- **KDE Connect** `plugins/sms/SmsMmsUtils.kt`: reference group handling — `getMmsTo()` =
  RetrieveConf.to+cc dedupe, group send default true, native PDU builder. GPL-compatible
  to steal from; transport is KDE packets, no HTTP.
- **Traccar SMS Gateway**: Fossify fork + Jetty FGS (`app/src/traccar/kotlin/org/traccar/
  gateway/{GatewayServer,GatewayService}.kt`, `POST /` + Authorization key,
  `FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING`) — SMS-only API; use as service skeleton.
- **SmsForwarder**: active, AndServer HTTP + web UI, but MMS code is dead reflection.
- **Silence/QKSMS/Simple SMS**: dead (2021/2023/2024). Existing Matrix bridges
  (`tijder/SmsMatrix` 2023, `benkuly/matrix-sms-bridge`) are SMS-text-only → no
  MMS-capable OSS Matrix bridge exists. F-Droid "sms gateway" search → 0 apps.

## Group MMS platform details

- Threads are keyed by recipient set (`Threads.getOrCreateThreadId`); recipient_ids in
  `content://mms-sms/conversations?simple=true`; any mismatch creates a new thread (cause
  of Fossify #863/#595/#709). Bridge rule: Matrix room ⇄ `thread_id`, never per-sender.
- Recipient set on receive: RetrieveConf To/Cc; platform persists all addr types
  (`PduPersister.persistAddress`, mmslib `PduPersister.java:687-695`). Full fidelity only
  if the app reads beyond `type=137` (Fossify does; capcom6 doesn't).
- Retrieval: WAP push → default app only; mmslib `PushReceiver`/`TransactionService` +
  AOSP MmsService (MMS APN, retries via `RetryScheduler`/`NotificationTransaction`).
- MMS send: multi-To single PDU with `MMS_CONFIG_GROUP_MMS_ENABLED`; platform allows
  non-default send but outbox persistence needs default role; some carriers drop
  non-default sends. Keep fork as default app.
- Multipart SMS: broadcast delivers all segments; both apps concatenate (UDH dropped —
  acceptable). MMS read *reports* (`ReadRecTransaction`) are separate, off by default.
- GrapheneOS/LineageOS: AOSP `com.android.mms.service`; needs `mms` APN + mobile data;
  VPN ("block connections without VPN", always-on VPN) break MMS; FGS needs
  battery-optimization exemption; `INTERNET` permission only in bridge service/flavor.
- Role recap: SMS_DELIVER + WAP_PUSH_DELIVER + `RESPOND_VIA_MESSAGE` service =
  default role. Non-default companion with READ_SMS/SEND_SMS can read history + send but
  cannot mark read/delete/download MMS.

## Signing & distribution

- F-Droid signs with its own key unless reproducible builds +
  `Binaries`/`AllowedAPKSigningKeys` metadata. A private fork must self-sign/self-distribute.
- One-time keystore: `keytool -genkeypair -v -keystore release.jks -alias messages
  -keyalg RSA -keysize 4096 -validity 10000`; back up offline (loss = no updates).
- CI patterns: capcom6 `build-apk.yml` (base64 keystore secret + alias/passwords);
  DekuSMS reproducible-build workflow (`apksigner sign` + `verify`); Fossify accepts both
  `keystore.properties` and `SIGNING_*` env (app/build.gradle.kts:45-63). Verify with
  `apksigner verify --print-certs`.
- Replacing install: same applicationId + key → `adb install -r`. Different signature
  (e.g. F-Droid build → fork) → `INSTALL_FAILED_UPDATE_INCOMPATIBLE`, uninstall first.
  SMS/MMS history survives uninstall (lives in telephony provider).
- Distribution: own F-Droid repo (fdroidserver), Obtainium, or plain `adb install`; ship
  universal APK; keep versionCode monotonic.

## Fork plan (Fossify Messages + mmslib)

Fork both repos. mmslib has **no repo-level LICENSE** (file headers Apache-2.0
klinker/AOSP) → vendor as a local Gradle module from the fork, add LICENSE/NOTICE, pin the
commit (replaces the `org.fossify:mmslib` JitPack dep).

1. **Transport**: new `BridgeService` FGS (`foregroundServiceType="connectedDevice"`,
   POST_NOTIFICATIONS) with embedded HTTP server (Traccar Jetty skeleton or Ktor like
   capcom6), bearer token, optional SSE/WebSocket events. Sends/receives stay inside
   Fossify (notifications, blocked numbers, recycle bin still apply).
2. **Event push**: hook `SmsReceiver.handleMessageSync()` and
   `MmsReceiver.handleMmsMessage()` → emit `{type, sms|mms, providerId, threadId, sender,
   participants[], parts[]}`.
3. **History read**: expose provider via existing `Context.getConversations()`,
   `getMessages()`/`getMMS()` (dateFrom pagination, `MESSAGES_LIMIT=50`),
   `getThreadParticipants()`, `getMmsAttachment()`; byte-exact via `MessagesReader`/
   `MmsBackup` (no Room dependency; provider is truth).
4. **Send API**: `POST /send {addresses[], text, attachments[base64|partUri], group:true}`
   → `sendMessageCompat` with group forced true; fix N attachments → one MMS (pass whole
   list into one klinker `Message.addMedia` loop; buildPdu supports N parts + SMIL).
5. **Read-state sync**: `POST /thread/{id}/read`, `/message/{id}/read` →
   `markThreadMessagesRead`/`markMessageRead` (provider READ+SEEN). Differentiator.
6. **Stable IDs**: `Message.getStableId()` + `thread_id` (`sms:<stableId>`,
   `mms:<stableId>`, room ⇄ `thread:<threadId>`).
7. **Tests/caveats**: filter/merge `|`-joined dummy group SMS rows; group MMS needs
   `settings.group=true` + carrier acceptance; verify group reply paths (#744 etc.);
   MMS download needs default role + mms APN; document Wi-Fi/VPN caveats.

**Fast path**: run capcom6 in parallel as v1 (MMS send multi-recipient + attachments,
webhooks, inbox, `/inbox/refresh` backfill) while the Fossify fork becomes v2 for full
fidelity (receive recipients, read state).
