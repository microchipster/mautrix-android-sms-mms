<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# twilio-pals — scriptable group-chat participants

Testing group MMS needs **2+ real phone numbers**: group messaging is a carrier
feature, and the phone only shows a group thread when a message arrives with
multiple recipients. Two virtual Twilio numbers driven by API give you that
without depending on friends to text you.

## Setup (one time)

1. Create a Twilio account (upgrade to paid — trial accounts can only message
   verified numbers, and your SIM is not verified).
2. Buy **two** US/Canada local numbers with SMS + MMS capability
   (Phone Numbers → Buy a number → filter by "SMS" and "MMS").
3. Store credentials:

   ```sh
   mkdir -p ~/.config/twilio-pals
   cat > ~/.config/twilio-pals/config.json <<'EOF'
   {
     "account_sid": "ACxxxxxxxxxxxxxxxx",
     "auth_token": "xxxxxxxxxxxxxxxx",
     "numbers": {"a": "+15550001111", "b": "+15550002222"},
     "phone": "+15550100100"
   }
   EOF
   chmod 600 ~/.config/twilio-pals/config.json
   ```

   (Env vars `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`, `PAL_A`, `PAL_B`,
   `PHONE` work too.)

Costs: ≈ $1.15/month per number, ≈ $0.02 per MMS, ≈ $0.008 per SMS.

## Usage

```sh
# A texts your phone one-to-one
./pals.py send --from a --to phone --text "hello from A"

# A starts the GROUP thread: one MMS to the phone and B
./pals.py send --from a --to phone,b --text "group hello" --media https://demo.twilio.com/owl.png

# B replies in the group (same recipients incl. the phone)
./pals.py send --from b --to phone,a --text "hi from B" --media https://demo.twilio.com/owl.png

# See everything the numbers sent/received, or watch live
./pals.py recv --number all
./pals.py watch --interval 5
```

Notes:

- Twilio only supports multi-recipient messages as MMS, so group sends need
  `--media` (any public image URL; the URL above is Twilio's demo asset).
- Replies from your phone arrive as **one message per Twilio number** — that is
  normal carrier behaviour for group MMS, not a bug.
- Any alias in `--to` can be an explicit `+E.164` number, so a third
  participant can be added with a spare SIM or another provider.

## Group-chat acceptance matrix

Run through this against the bridge; each item is one automated `send`, one
action on the phone, then a Matrix-side check.

| # | Action | Expected |
|---|---|---|
| 1 | A texts phone 1:1 | One room for the thread; message appears; phone notification shows one thread |
| 2 | A sends group MMS to phone + B | Phone shows the *existing* thread (or one new group thread); bridge has exactly ONE room for that thread id |
| 3 | B replies to phone + A | Same phone thread, same Matrix room — no second room, no fractured thread |
| 4 | A sends a group MMS with an image | Image arrives in Matrix as a normal `m.image`, bytes intact |
| 5 | Phone replies in the group thread | ONE outgoing MMS to A and B (verify both inboxes with `recv`), no duplicates in Matrix |
| 6 | A sends an MMS to phone only (drops B) | Carrier may treat this as a new thread; whatever the phone does, the bridge must mirror it exactly (new room only if the phone made a new thread) |
| 7 | A adds C (a third number) and sends | Membership of the room updates in place; no ghost for the phone's own number; no duplicate room |
| 8 | A sends an Android tapback text (`Loved "..."`) | Bridge converts it into a Matrix reaction on the targeted message |
| 9 | Mark the room read in Matrix | Phone thread shows read (`read=1`, `seen=1` in the provider) |
| 10 | Restart the phone app / bridge | No duplicate rooms, no replayed messages (dedupe + watermarks) |

Record failures with the bridge log time window and the Twilio message SIDs
(`recv` prints them via `twilio-pals` output / Twilio console).

## Alternatives

- **Telnyx** (≈ $1/number/mo) and **Plivo** (≈ $0.80/number/mo) — same idea,
  different API; only `load_config`/`request` in `pals.py` would need tweaking.
- **jmp.chat** — a paid XMPP number that can text from Matrix (you likely
  already run the mautrix-jmpchat bridge); nice for one virtual participant.
- **Old phone + prepaid SIM** — a real second participant, zero code.
