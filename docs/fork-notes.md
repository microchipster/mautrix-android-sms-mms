# Fossify fork notes (recon at fossify-messages 7fd846a / fossify-mmslib ebfbb30)

Blueprint for the bridge fork. Line numbers refer to those commits.

## 1. Manifest (`app/src/main/AndroidManifest.xml`)

- Perms L6-23: READ_SMS, WRITE_SMS, SEND_SMS, RECEIVE_SMS, RECEIVE_MMS, WAKE_LOCK,
  SCHEDULE_EXACT_ALARM, RECEIVE_BOOT_COMPLETED, READ_PHONE_STATE, POST_NOTIFICATIONS,
  READ_SYNC_SETTINGS, CALL_PHONE, READ_CONTACTS. **Missing: INTERNET, FOREGROUND_SERVICE
  (+FOREGROUND_SERVICE_DATA_SYNC for API 34+) — add for BridgeService.**
- Services: `HeadlessSmsSendService` L163-176 (exported, RESPOND_VIA_MESSAGE);
  `com.android.mms.transaction.TransactionService` L178.
- Receivers: SmsStatusSent/Delivered; `SmsReceiver` L184-191 (SMS_DELIVER, exported,
  BROADCAST_SMS); `PushReceiver` L193-203 (WAP_PUSH_DELIVER + wap mms mime); MmsSent;
  `MmsReceiver` L210-213; MarkAsRead/DirectReply/DeleteSms/ScheduledMessage (internal);
  RescheduleAlarmsReceiver L234-244.
- applicationId from `gradle.properties` APP_ID (L8) → app/build.gradle.kts:35 + namespace
  L118; debug suffix `.debug` L72. Authorities use `${applicationId}`. taskAffinity values
  are package-independent (matched via `BroadcastUtils.addClassName`). No hardcoded pkg in
  app code except `BuildConfig.APPLICATION_ID` uses. → side-by-side id is safe.
- FileProvider authority `${applicationId}.provider` L246-254.

## 2/3. Receive hooks

- `receivers/SmsReceiver.kt`: `onReceive` L32-75 → `handleMessageSync(...)` L77-155;
  provider insert `context.insertNewSMS` L92-101 (Context.kt:821-849, returns `_id`),
  Room `context.messagesDB.insertOrUpdate(message)` L138, then notifications L140-154.
  **Hook after L138.**
- `receivers/MmsReceiver.kt`: extends klinker `MmsReceivedReceiver` (library `onReceive`
  final; persists via `DownloadRequest.persist` then calls `onMessageReceived` L45-57 —
  provider row complete before callback). `handleMmsMessage` L63-102; text-only MMS throws
  inside L69-78 (`attachment!!.attachments.first()`) but is caught. **Hook at end of
  `handleMmsMessage` (after L96 addConversation/refresh).**
- `DownloadRequest.persist` hardcodes `groupMmsEnabled=true`
  (`service_alt/DownloadRequest.java:183-189`) → receive-side threads already use FROM+TO+CC.

## 4. Config (`helpers/Config.kt`)

- `sendGroupMessageMMS` L43-46 default **false** (key SEND_GROUP_MESSAGE_MMS Constants.kt:25);
  `isGroupMessageMmsPreferenceResolved` L48-49; `sendLongMessageMMS` L38-41; delivery
  reports L33-36; `mmsFileSizeLimit` L56-58 (default 200KB, UI-only enforcement).
- No auto-retrieve app key: library reads `auto_download_mms` (default true,
  NotificationTransaction.java:146-152) and `group_message` (default true, PushReceiver.java).
- `getSendMessageSettings()` Messaging.kt:22-30 builds klinker `Settings` with
  `group=config.sendGroupMessageMMS`.

## 5. Send path

- `extensions/Context.kt` → `sendMessageCompat(text, addresses, subId, attachments, messageId)`
  Messaging.kt:39-108. MMS decision L52-53 (`attachments.isNotEmpty() || long || addresses>1 && group`).
  **BUG to fix: attachment loop L56-66 sends one Transaction per attachment** (last one
  carries text) — must be one `Message` with `addMedia` per attachment.
- Multi-recipient SMS branch L70-78 → `MessagingUtils.sendSmsMessage`:
  **inserts merged dummy `a|b` address row L91-101** (`ADDRESS_SEPARATOR="|"` L205) before
  per-address sends L103-119. Bridge must filter the merged row.
- `sendMmsMessage` L157-196: `Transaction(context, settings)` + `Message(text, recipients)`
  L164-165, single `addMedia` L178, sends L192; library supports multi-media
  (`Message.addMedia` L397) and builds multipart (`Transaction.sendMmsMessage` L413-481).
- Group decision UI: ThreadActivity.isMmsMessage L1907-1911, confirm dialog
  `confirmGroupMessageSendIfNeeded` L1605-1625, GroupMessageSendDialog.kt.

## 6. History API (`extensions/Context.kt`, not helpers/)

`getMessages(threadId, dateFrom=-1, includeScheduled=true, limit)` L114-216 (sms + mms merge);
`getMMS(threadId?, sortOrder?, dateFrom, uri)` L219-300; `getMMSSender` L302-321;
`getUnreadCountsByThread` L323-349; `getConversations(threadId?, privateContacts)` L351-462
(`isGroupConversation = phoneNumbers.size>1` L427); `getConversationIds` L482-493;
`getMmsAttachment(id)` L497-561; `getLatestMMS` L563-566; `getMMS(uri)` L568-570;
`getMessageRecipientAddress` L599-619; `getThreadParticipants(threadId, contactsMap)` L621-666;
`markMessageRead(id,isMMS)` L996-1006; `markThreadMessagesRead(threadId)` L1008-1029.
**No `getMessage(id)`** — add or query provider directly for the API.

## 7. IDs

`Message.getStableId()` models/Message.kt:44-49 → `generateStableId(type,key)` Constants.kt:119-122
(bits L63-66). Room `Message.id` == provider `_id` (SMS via insertNewSMS return, MMS via
Context.kt:252); `m_id` unused. Scheduled messages use random ids (Constants.kt:113-117).

## 8. Services/notifications

No foreground service exists; only `services/HeadlessSmsSendService.kt` (plain). Long-running
= AlarmManager exact alarms. Notification channel helper `NotificationHelper.createChannel`
(private L232-247), channel id `fossify_messages` (Constants.kt:18). `App` extends FossifyApp
(App.kt:15-44).

## 9. Build

- app/build.gradle.kts: buildTypes L70-85, flavors foss/gplay L87-91, deps L142-156
  (no network libs!), namespace L118. libs.versions.toml: mmslib 1.0.1 L21/L48, compileSdk 36
  / target 36 / min 26. Root settings.gradle.kts includes only `:app` L17.
- **Local mmslib**: add `include(":mmslib")` + `project(":mmslib").projectDir =
  file("../fossify-mmslib/library")`, swap `implementation(libs.mmslib)` →
  `implementation(project(":mmslib"))`. mmslib library module: group org.fossify, namespace
  com.klinker.android.send_message, compileSdk 34, minSdk 26, deps logger 1.0.3 + okhttp 2.5.0
  (+urlconnection). No LICENSE file in repo (headers Apache-2.0) → add LICENSE/NOTICE.
- No tests anywhere in app (no src/test, no src/androidTest); gradle wrapper 9.8.1.

## 10. mmslib plumbing

- `Transaction.java`: `sendNewMessage` L141-196 — `if (!settings.getGroup())` sends one MMS
  per address L167-173, else single MMS with all addresses L174-178; SMS merged dummy row
  L187-194; multipart `sendMmsMessage` L413-481 (images L426-435, extra parts loop L439-452,
  text L454-461); system sending `sendMmsThroughSystem` L608-693 with
  `MMS_CONFIG_GROUP_MMS_ENABLED = settings.getGroup()` L673; `getBytes` recipients
  `addTo` L490-496, PduBody L514-539, SMIL L542-548; `checkMMS` L929-935.
- `Settings.java`: group L40/177/318, deliveryReports, sendLongAsMms, sendLongAsMmsAfter,
  subscriptionId, DEFAULT_SUBSCRIPTION_ID=-1.
- `Message.java`: `parts` L74, `addMedia` L397-399 (also 375/386), `getParts` L511-513.
- `PduPersister.java`: ADDRESS_FIELDS L101-106; `persist` L1287-1512; thread creation loads
  FROM L1393 + TO/CC when groupMmsEnabled L1401-1408 (SEND_REQ: TO only L1410-1411);
  `persistAddress` L687-700 writes all addr types L1503-1509; `loadRecipients` L1522-1546
  excludes own line1.
- `PushReceiver.java`: group read L131-140/176-188, persist NotificationInd L184-188.

## 12. No HTTP/webhook/adb/su code anywhere (app or lib) — bridge service is new code.
