<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Caller ID, labels and anti-spam

## What caller information SMS/MMS actually gives you

- **Carrier (CNAM) lookup: not available.** SMS and MMS transport the sender
  address (`+1555…`, a short code, or an alphanumeric sender ID) and nothing
  else. There is no caller-name field in the PDU that the bridge or the phone
  could read — CNAM is a landline/SS7 database feature that mobile carriers do
  not deliver with SMS/MMS. Any "caller ID" beyond the number can only come
  from your own contacts on your own devices.
- **Contact names: not available.** Neither SMS/MMS itself nor the pairing
  API exposes the phone's address book, so ghost users and rooms are named
  with the canonical number. A name would have to come from Matrix-side
  provisioning, and only the number is authoritative.
- **Own-number / carrier info comes from the SIM.** The paired app's heartbeat
  (or the ADB agent's health payload) reports each subscription's carrier,
  number and display name, which the bridge stores for `!sms stats` (see
  [`carrier-scoped-stats.md`](carrier-scoped-stats.md)). This is the phone's
  *own* carrier, not a lookup of the sender's carrier.

## Labels

Labels are free-form tags stored in the ghost metadata of a phone number or
sender ID. They are never sent to the phone and do not change room names.

Manage them from the management room:

```
!sms label +15550100100 family            # add a label
!sms label +15550100100 very important    # labels may contain spaces
!sms label +15550100100 --clear           # remove all labels
!sms labels                               # list every labeled contact
```

### Automatic "new contact" labeling

When the first inbound message of a sender creates a new portal and the number
has no DM portal yet, the bridge tags the ghost with the `new contact` label.
Two network-config keys control the behavior:

```yaml
network:
  # Tag first-time senders (default: true). Metadata only.
  auto_label_new_contacts: true
  # Also post a management-room notice with the number (default: false).
  # The notice never contains the message body.
  new_contact_notice: false
```

Manually added labels are preserved when the automatic label would be added.
Clearing labels with `--clear` clears every label for that contact — automatic
and manual ones alike. Both config keys are read at startup; changing them
requires a bridge restart.

## Anti-spam

### Blocking a sender

The bridge implements the standard `block-user` command, so it works on DM
portals:

```
!sms block-user            # run inside the DM room with the sender
!sms unblock-user          # or: !sms block-user --unblock
```

Because SMS/MMS has no remote block list the bridge can write to, blocking is
**bridge-side**: inbound messages from a blocked contact are dropped before
they reach Matrix (in DMs and in groups that contain the blocked number), and
`portal.user_blocked` is stored with the room state. To stop the messages on
the phone itself as well, block the number in the Android messages app.

### What the bridge does not do

- It does not send delivery receipts, spam reports, or carrier complaints on
  your behalf.
- It does not scan message bodies for spam (the optional pattern set in
  [`history-ttl.md`](history-ttl.md) is for redacting sensitive messages, not
  for classifying senders).
