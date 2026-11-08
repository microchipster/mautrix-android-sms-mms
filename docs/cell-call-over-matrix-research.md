<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Answering a normal cellular call from a Matrix client (research)

Read-only research note, 2026-10-27. Scope clarified by the user: a **normal (non-JMP) carrier SIM
number in an Android phone**, the caller dials that number, and the user wants to **answer/take the
call from a Matrix client with two-way audio**. Not JMP-specific; not "forward the carrier number to
a SIP number" as the primary design (the user questioned that idea; it stays in the table as the
zero-code fallback). No code was written and **nothing in this document was executed on a device** —
the probes in §6 are designed, not yet run. Claims taken from third-party repositories are marked as
claimed/not independently verified.

Device context in this repo (TODO.md): office phone = LineageOS 21, rooted, US Mobile SIM; plus a
Pixel for comparison; carrier-side plan is a normal US number.

---

## 1. Reality check

1. **Answering and placing calls from an app is a solved, documented problem** (§2.1). A normal
   third-party app can answer a ringing carrier call with `ANSWER_PHONE_CALLS` +
   `TelecomManager.acceptRingingCall()`; a default-dialer app (or a companion app) implementing
   `InCallService` gets full in-call control (answer, reject, hold, mute, DTMF, audio route).
   Root only removes user consent steps and adds `MODIFY_PHONE_STATE`.
2. **Live cellular call audio is the actual wall.** Android has no third-party API that yields the
   uplink/downlink audio of an ongoing *carrier* call:
   - `VOICE_CALL` / `VOICE_DOWNLINK` / `VOICE_UPLINK` capture requires
     `CAPTURE_AUDIO_OUTPUT`, protection level `signature|privileged|role` and documented as
     "reserved for use by system components and is not available to third-party applications".
   - `CALL_AUDIO_INTERCEPTION` is the permission that literally "allows an application to access the
     uplink and downlink audio of an ongoing call" — `signature|privileged|role`, `@SystemApi @hide`,
     "Not for use by third-party applications".
   - `AudioPlaybackCapture` (the Android 10+ supported way to capture other apps' audio) can only
     capture `USAGE_UNKNOWN` / `USAGE_MEDIA` / `USAGE_GAME`; **voice communication is excluded by
     design**.
   - `CallStreamingService` (added API 34) is `@SystemApi @hide`, `signature`-bound, and aimed at
     **VoIP** call streaming — not a third-party route into carrier calls. `CallRedirectionService`
     is outgoing-call redirection only.
3. **Root changes the rules, but device by device.** With root, capture is done by bypassing the
   Android audio policy and reading the vendor audio HAL directly (`/dev/snd`, `tinycap`/`tinymix`)
   or by installing the app as a privileged `priv-app` to hold `CAPTURE_AUDIO_OUTPUT`. Working
   public examples exist (Qualcomm: `VOC_REC_DL`/`VOC_REC_UL` capture + `Incall_Music` injection on a
   Redmi Note 7; Samsung Exynos: ALSA injection into the live uplink on an A51). But many devices
   never expose a capture PCM for the voice path, vendors deliberately disable the `VOC_REC` capture
   controls, and VoLTE/IMS keeps the media path inside the modem DSP. The one-line caution from the
   main open-source rooted recorder is: "MOST PHONES DOES NOT SUPPORT CALL RECORDING."
4. **Even the vendors' own call recording is gated.** Google's Phone app call recording is
   Pixel 6+ / Android 14+, region-limited; LineageOS ships call recording in the privileged dialer
   with `CAPTURE_AUDIO_OUTPUT`, defaults to **microphone** capture, only uses `VOICE_CALL`
   "if supported by device", and gates it per country. This is a platform feature, not an API.
5. **Therefore: the goal as stated is achievable in three tiers**, in descending reliability:
   (A) carrier forwarding to a SIP number that already bridges to MatrixRTC (works today, no phone
   code, ~US$1–2/month for a DID); (B) a no-root local audio tap (Bluetooth HFP hands-free role or a
   USB audio headset gadget) on a stationary phone, with call control from an app; (C) rooted ALSA /
   privileged-app capture, which is a per-device hardware spike that may simply fail.

---

## 2. What the platform gives an app

### 2.1 Call control (answer / place / end)

| Capability | API | Permission | Notes |
|---|---|---|---|
| Answer ringing carrier call | `TelecomManager.acceptRingingCall()` | `ANSWER_PHONE_CALLS` (`dangerous\|runtime`) or `MODIFY_PHONE_STATE` | API 26+; deprecated in API 29 *for wearable companions* but present and functional; self-managed calls need privileged/SystemUI caller |
| End call | `TelecomManager.endCall()` | `ANSWER_PHONE_CALLS` | Same deprecation note |
| Place call | `TelecomManager.placeCall()` / `ACTION_CALL` | `CALL_PHONE` (`dangerous`) | Works from any app; emergency calls always go via the preloaded dialer |
| Full in-call UI + control (answer, reject, hold, mute, DTMF, audio route) | `InCallService` | service must declare `BIND_INCALL_SERVICE` (`signature\|privileged`); system binds it only to the **default dialer** (`RoleManager.ROLE_DIALER`, needs `ACTION_DIAL` handling + full `InCallService`) | `Call.answer()/reject()/disconnect()/setMuted()/setAudioRoute()` in-call; `IN_CALL_SERVICE_RINGING` metadata lets the app own the ringtone |
| In-call control for a **companion app** (no dialer role) | `InCallService` + CompanionDeviceManager association | `MANAGE_ONGOING_CALLS` (`signature\|appop`) | Documented "Access to InCallService for Wearable Devices": declare `MANAGE_ONGOING_CALLS`, associate a companion device, implement `InCallService`. Non-root route to in-call control, but requires a (physical or virtual) companion association |
| Root-privileged variants | `MODIFY_PHONE_STATE` grant via priv-app, `cmd telecom set-default-dialer`, HAL-level routing | `signature\|privileged\|role` / root | What root "adds" for control is mostly convenience: no user-granted runtime permission, silent role assignment, and the ability to change/override audio routing via vendor mixer controls |

Practical answer for this project: **control is easy**. A small app (or a foreground service) can
answer, hang up, and place calls on the office phone today, and the same app can be made the default
dialer on a dedicated phone so that it owns the in-call UI and audio routing
(`adb shell cmd telecom set-default-dialer <pkg>` for lab tests; revert with `set-system-dialer default`).

### 2.2 Live call audio capture

Checked against AOSP `core/res/AndroidManifest.xml` and current API docs (2026-10-27):

| Permission | Protection | Meaning |
|---|---|---|
| `CAPTURE_AUDIO_OUTPUT` | `signature\|privileged\|role` | Needed by `VOICE_CALL`, `VOICE_DOWNLINK`, `VOICE_UPLINK`, `REMOTE_SUBMIX` capture; "reserved for use by system components" |
| `CAPTURE_VOICE_COMMUNICATION_OUTPUT` | `signature\|privileged\|role` | Capture of audio rendered with `USAGE_VOICE_COMMUNICATION` via a playback-capture mix |
| `CALL_AUDIO_INTERCEPTION` | `signature\|privileged\|role`, `@SystemApi @hide` | "Allows an application to access the uplink and downlink audio of an ongoing call. Not for use by third-party applications" |
| `RECORD_AUDIO` | `dangerous` | Microphone only. During a call this is the phone's own mic (the far end is usually inaudible or muted; quality varies by device) |
| `CAPTURE_MEDIA_OUTPUT` | `signature\|privileged\|role` | Media-only subset (not voice) |

What is **fundamentally blocked**, independent of root effort:

- **No supported API surface** for third-party capture of carrier-call audio on any Android version.
  The permissions are platform-provisioned; `pm grant` cannot grant them (signature/privileged only,
  and a priv-app can receive `signature|privileged` only if the ROM's privapp allowlist includes it
  — LineageOS and stock images both grow such allowlists only for their own apps).
- **Where the audio physically lives**: with VoLTE/VoWiFi, media terminates in the modem's IMS
  stack/DSP. Whether the application processor ever sees that PCM is a **vendor audio-HAL decision**.
  If the vendor routes voice through a dedicated "call"/"incall"/"calliope" PCM, an ALSA tap is
  possible; if the DSP keeps it internal, no amount of mixer poking on the AP reveals it.
- **Vendor mixers often have the capture path compiled out / commented out.** The main open-source
  rooted recorder patches the audio XML to re-enable `VOC_REC` because "many manufacturers disable by
  default to prevent two-way call recording".
- **SELinux**: raw `/dev/snd` access from an app needs permissive SELinux or `/dev/snd` chmod (the
  Qualcomm gateway project runs `setenforce 0` and `chmod 666 /dev/snd/*` as root).

What root/primed setup makes possible, with evidence:

- **Qualcomm `VOC_REC_DL` / `VOC_REC_UL` capture + `Incall_Music` injection** (claimed, working on a
  Redmi Note 7 / SDM660 / LineageOS 17.1): a rooted app reads the far-end (downlink) PCM, feeds
  PJSIP, writes the SIP-side audio back into the uplink through an `Incall_Music` mixer path. Mute
  profiles clear the phone's own speaker/mic to prevent echo/leakage; the watchdog re-applies mutes
  every 3 s. This is the closest existing proof of the full "phone as GSM→SIP bridge" architecture.
- **Samsung Exynos ABOX injection** (claimed, Galaxy A51 / Android 13): writing PCM to `pcmC0D8p`
  (RDMA8) injects into the live uplink (`SPUS OUT8 → SIFS5`); during a call the HAL also had
  `pcmC0D104p` (calliope_4, playback) and `pcmC0D113c` (calliope_13, capture) open. That capture PCM
  is exactly the kind of device to probe for downlink audio. The same repo notes that
  `AudioTrack`/`STREAM_VOICE_CALL` "silently fails on both Pixel and Samsung" — the public API path
  is a dead end even for injection.
- **Privileged-app route** (`CAPTURE_AUDIO_OUTPUT` via `priv-app` allowlist) is the cleanest
  self-contained variant: no ALSA, no SELinux dance — but it only works on devices whose HAL routes
  voice PCM to `VOICE_CALL`/`VOICE_DOWNLINK` inputs at all, and even LineageOS defaults its own
  recorder to MIC because `VOICE_CALL` "if supported by device" is frequently false.

### 2.3 What root adds

- Raw `/dev/snd` access (`tinycap`, `tinymix`) for discovery and streaming — requires
  `setenforce 0` (or a permissive domain/policy patch) and `/dev/snd` permissions.
- Vendor mixer control changes that Android's audio policy would otherwise refuse (voice route
  forcing, hardware mute of earpiece/mic, injection paths).
- Privileged-app install to hold `CAPTURE_AUDIO_OUTPUT` / `MODIFY_PHONE_STATE`.
- Silent default-dialer assignment and shell-level test commands.
- It does **not** create a PCM path that the vendor HAL/DSP does not implement.

---

## 3. Options

| # | Option | Audio both ways? | Answer from Matrix? | Root | Effort | Quality / latency | Verdict |
|---|---|---|---|---|---|---|---|
| 1 | **Carrier forwards all calls to a SIP DID**, SIP bridges to MatrixRTC (`mrtc-sip-bridge`, or JMP) | yes, at the SIP DID | yes (incoming call rings Matrix clients) | no | low (config only) | G.711/Opus; one extra carrier hop; ~150–400 ms added one-way worst case | **Works today. Zero phone code. Best value.** |
| 2 | Same, but conditional forward (no-answer) | yes | yes, after the phone rings out | no | low | same | Lets the phone ring first, Matrix as fallback |
| 3 | **Hardware cellular→SIP gateway** (4G/VoLTE SIM box) + SIP→MatrixRTC | yes | yes | n/a | medium (money + wiring) | good (VoLTE), echo canceller included; needs own SIM/plan; 2G-only models useless in the US | Viable if you want dedicated hardware; ~€475+ for 1-port 4G |
| 4 | **Rooted phone as GSM→SIP gateway** (Qualcomm `VOC_REC_DL`/`Incall_Music` + PJSIP) | yes | yes (SIP leg) | yes | high, per-device | 8/16 kHz PCM → Opus; low added latency; no acoustic echo if device mic/speaker hardware-muted | Promising on Qualcomm if `VOC_REC` exists; fragile, lab-only |
| 5 | **Phone app streams Opus to our bridge** (existing app transport, new WS/media channel), bridge joins the LiveKit SFU as the ghost device | yes | yes | yes (same capture problem) | high | same as #4 + bridge work | Same capture wall; more bridge code, no SIP stack; needs SFrame key delivery (jmpchat M2) for encrypted rooms |
| 6 | **Bluetooth HFP tap**: phone (AG) → Linux BlueALSA (hands-free), no root | yes (SCO) | answer still on the phone (app), audio in Linux bridge | no | medium | 8 kHz CVSD or 16 kHz mSBC mono; SCO adapter caveats; extra jitter | Best no-root audio tap; fits a "phone in a drawer" rig |
| 7 | **USB audio headset gadget** (Pi Zero `g_audio` or USB headset loopback), no root | yes (USB audio) | answer on the phone (app) | no | medium | 16 kHz+ clean PCM; tethered | Device-dependent routing; good quality if supported |
| 8 | `CAPTURE_AUDIO_OUTPUT` privileged app reading `VOICE_CALL`/`VOICE_DOWNLINK` | maybe | yes | yes | medium | clean if it works | Per-device; verify before building on it |
| 9 | adb-based lab capture (`su -c tinycap` → USB host) | yes (wired) | via app/shell | yes | low | fine on USB | Lab only; never a product path |
| 10 | Wi-Fi calling / VoLTE APIs | n/a | n/a | n/a | — | — | **No public API exists**; nothing to build |
| 11 | Third-party call recording apps (mic-based) | no (mic only) | no | no | low | poor, one-sided | Not useful for bridging |

---

## 4. Transports, latency, echo

**Phone → bridge.** The project's app transport today is authenticated HTTP/JSON + webhooks/SSE (the
app polls the bridge). Call audio needs a new low-latency bidirectional channel: a WebSocket
(`github.com/coder/websocket` is already in the module graph) or UDP/SRTP. Do not try SSE/HTTP
long-poll: jitter tolerance is the whole game.

**Codec.** Encode Opus on the phone (`MediaCodec` has an `audio/opus` encoder from Android 10;
libopus via JNI if you want DTX/FEC/bitrate control). Voice settings: mono, 16–24 kHz, 16–32 kbps,
20 ms frames. Opus in-band FEC and packet-loss concealment cover the LTE/WS loss spikes. Decode
Opus from MatrixRTC/LiveKit on the phone → PCM → uplink mixer.

**Latency budget (one-way, realistic):**

| Segment | Typical |
|---|---|
| Cellular/VoLTE + carrier + far-end | 100–200 ms (visible in any PSTN call) |
| Phone capture → Opus → WS to bridge | 20–60 ms + buffer |
| Bridge → LiveKit SFU → Matrix client | 40–120 ms incl. client jitter buffer |
| **Tolerable added budget for a bridge** | aim ≤ 100–150 ms one-way beyond the cellular baseline |

A round trip of ~400–700 ms is usable but noticeably worse than a native call. Anything above ~1 s
one-way ruins conversational turn-taking; keep jitter buffers bounded (40–120 ms) and prefer
dropping packets over growing latency.

**Echo.** If the tap is digital (ALSA/`VOICE_DOWNLINK` or SCO/USB audio), there is **no acoustic
path in our pipeline** — as long as the phone's own speaker and microphone are not part of the call
(hardware-muted per the Qualcomm project's mute profiles, or replaced by the BT/USB route). AEC is
then only needed in the Matrix client (it already runs there) and for any local monitoring. If the
only capture available is `MIC`, then AEC between the phone speaker and the phone mic must happen on
the device, and quality/usability drop sharply — treat that as a failure, not a fallback.

**Clock drift.** The phone's audio clock and the bridge's are independent; resample at the bridge
(or run a small adaptive buffer). Do not assume sample rates align.

**Why not SIP directly from the phone?** Because `mrtc-sip-bridge` already proves SIP→MatrixRTC
(inbound, outbound, DTMF, ringing Matrix clients via LiveKit), the phone side then only needs a
GSM↔SIP endpoint. That trades bridge code for an in-app PJSIP stack, which the Qualcomm gateway
project demonstrates is feasible (PJSIP 2.14 in NDK builds). The Trade-off: the SIP leg means
unencrypted rooms in that bridge (documented limitation) until SFrame key delivery (jmpchat
`SFRAME_CALLS_M0_FINDINGS.md` M2) covers bridged media.

**Why adb is lab-only.** `adb shell su -c tinycap …` works for experiments (USB-tethered host, root,
no resilience, no background), but a production bridge cannot depend on USB + a host + root + a
specific vendor mixer. Use it only to answer "does this device expose call PCM at all?" (§6 E2–E3).

---

## 5. Alternative architectures

### 5.1 Hardware cellular→SIP gateways

- 4G/VoLTE: GoIP8-4G (8 channels) US$620; Portech MV-370L (1 SIM, 4G/LTE) €474.81; Yeastar
  TG800LTE-A (8 ch) US$1,752; Dinstar UC2000/DWG2000-1G (1 port, **2G GSM only**) €178.50.
- Gateways do the hard part in hardware (DSP voice path, AEC, SIP/DTMF), but 2G-only models are
  dead in the US; only the 4G/VoLTE models matter. Keep in mind carrier ToS and local rules for
  SIM-box style devices, and that the gateway needs its **own SIM plan or a SIP DID**.
- Because the SIM cannot be in two places, using a gateway means either (a) forwarding the carrier
  number to the gateway's number (same shape as §5.2, with hardware in the middle), or (b) moving
  the SIM out of the phone — which breaks this project's SMS/MMS bridge on that number.

### 5.2 Carrier forwarding to a SIP number (the zero-code fallback)

- Standard GSM forwarding: unconditional `*21*<number>#`, cancel `##21#`, status `*#21#` (3GPP
  TS 22.030/ETSI). US carriers differ: Verizon/US Cellular `*72<number>` / `*73` off;
  AT&T/T-Mobile `*21*<number>#` / `#21#` (T-Mobile `##21#`); `##002#` disables all forwarding.
  Conditional variants (`*61*` no answer, `*67*` busy) let the phone ring first.
- Target: a SIP DID (e.g. Telnyx US$1/number/month incl. voice+SMS; other trunk providers similar;
  US DIDs normally need address/KYC verification). Feed it into `mrtc-sip-bridge`, which rings all
  Matrix clients and bridges the answered call into LiveKit/MatrixRTC.
- Caveats: unconditional forwarding means the phone never rings (callers hear no local ring);
  some carriers/MVNOs restrict or disable forwarding (verify with the actual SIM); forwarding
  consumes carrier minutes/features on some plans; caller ID through the forward is usually
  preserved but verify; two dependencies (carrier + SIP provider) for calls to work.
- This is the only option that a **normal, non-root, degoogled phone** can use. It is also the one
  the user explicitly set aside, so treat it as the known-good fallback, not the design.

### 5.3 Wi-Fi calling / VoLTE / IMS APIs

None. There is no public API to answer, monitor, or stream IMS/Wi-Fi-calling media; the IMS stack is
vendor/carrier-internal. Nothing to prototype here — skip.

### 5.4 Bluetooth HFP hands-free (no root)

- Pair the phone (AG role) with a Linux host running BlueALSA in **HFP-HF** role (`-p hfp-hf`,
  oFono optional). BlueALSA's built-in HFP-HF supports connection establishment, audio, volume and
  battery; call answer/reject/terminate must be done **by the phone** (our app, or the UI) — which is
  exactly our design. The phone (AG) creates the SCO audio connection when the call is up.
- SCO gives both directions: the far end arrives to Linux, and Linux audio goes to the phone's
  uplink (the phone's own mic is not used when the headset route is active).
- Caveats: not every Bluetooth adapter supports SCO on Linux (Broadcom/TI workarounds exist; test
  more than one dongle); 8 kHz CVSD or 16 kHz mSBC mono quality; A2DP must be out of the way; the
  phone must stay paired/connected and route the call to HFP (verify with `setAudioRoute` if we own
  the in-call UI).

### 5.5 USB audio headset gadget (no root)

- Emulate a USB Audio Class headset (e.g. Raspberry Pi Zero with `dwc2` + `g_audio`) and connect it
  to the phone. Android supports USB audio peripherals; in-call audio routing to a USB headset is
  device-dependent and must be verified on the office phone and the Pixel. If it routes, this is the
  cleanest no-root tap: full-duplex 16 kHz+ PCM, no SCO lottery, no acoustic echo (phone speaker/mic
  replaced by the gadget).
- Tethered by design — acceptable for a phone-in-a-drawer bridge, not for a pocket phone.

### 5.6 Not a path (documented so it is not retried)

- `CallStreamingService` (API 34): `@SystemApi @hide`, `signature` bind, VoIP-oriented.
- `CallRedirectionService`: outgoing calls only; `CALL_AUDIO_INTERCEPTION` remains system-only.
- `AudioPlaybackCapture` of a call: voice communication usage is explicitly non-capturable.
- Third-party `CAPTURE_AUDIO_OUTPUT`: not grantable without ROM/privileged install.
- Mic-only recording of a call: one side only (usually the local room), useless for bridging.

---

## 6. Experiment ladder

Ordered by value/effort; each probe has an expected result and a stop condition. Lab resources used:
office phone (LineageOS 21, root, US Mobile), a Pixel, a Linux host, optionally a BT dongle and a
Pi Zero. Keep every probe ≤ half a day; do not sink a week into a device before E4 says no.

**E0 — Control smoke test (no root, ~1 h).**
Proves the easy half.
```sh
adb -s <serial> shell cmd telecom get-default-dialer
adb -s <serial> shell cmd telecom set-default-dialer <pkg>     # lab only, revert later
adb -s <serial> shell am start -a android.intent.action.CALL -d tel:+15550100100
# incoming test: call the phone from another line, then:
adb -s <serial> shell input keyevent KEYCODE_CALL              # may need the app instead
adb -s <serial> shell dumpsys telecom | sed -n '1,80p'
```
Expected: dialer role read/set; outgoing call dials; incoming call answered (KEYCODE_CALL works on
AOSP-style dialers; otherwise the app with `ANSWER_PHONE_CALLS` is the answer). Stop: none — if
this fails, nothing else matters, fix it first.

**E1 — Carrier forwarding fallback (zero code, same day).**
Prove the working product path before chasing hardware:
```sh
adb -s <serial> shell am start -a android.intent.action.CALL -d 'tel:**21*<SIP_DID>#'   # GSM style
adb -s <serial> shell am start -a android.intent.action.CALL -d 'tel:*#21#'             # status
adb -s <serial> shell am start -a android.intent.action.CALL -d 'tel:##21#'             # disable
# Verizon-style: tel:*72<number> / tel:*73
```
Then call the carrier number from a third line; expected: SIP DID rings, `mrtc-sip-bridge` rings
Matrix clients, answer gives two-way audio. Stop: if the carrier/MVNO rejects forwarding, record it
and continue; this option is then closed for that SIM (it may still work on the production SIM).

**E2 — Rooted ALSA inventory + in-call PCM discovery (office phone and Pixel, 1–2 days).**
```sh
adb shell su -c 'ls -l /dev/snd; cat /proc/asound/cards; cat /proc/asound/pcm'
adb shell su -c 'tinymix -D 0 | head -200; tinymix -D 0 | wc -l'    # build tinyalsa if missing
# during an active call:
adb shell su -c 'grep -H "^state:" /proc/asound/card*/pcm*/sub*/status | grep -v CLOSED'
adb shell su -c 'tinymix -D 0 | grep -iE "voc|voice|call|incall|ul|dl|abox|calliope"'
```
Expected: on Qualcomm, `VOC_REC_UL/DL`-style controls and/or a dedicated capture PCM that flips to
`RUNNING` during the call; on Samsung/Tensor ABOX, "calliope"/voice capture PCMs (e.g.
`pcmC0D113c` was open during a call on an Exynos A51). Stop: if **no** capture PCM changes state
during a call on a device (even after mixer forcing), the rooted capture track is dead for that
device — write the finding and move on.

**E3 — Capture probe: is the PCM real audio? (same day as E2).**
```sh
adb shell su -c 'tinycap /data/local/tmp/cap.wav -D 0 -d <N> -c 2 -r 16000 -b 16'   # while talking
adb pull /data/local/tmp/cap.wav
sox cap.wav -n stat 2>&1 | grep -Ei "rms|maximum"    # or: ffmpeg -i cap.wav -af volumedetect -f null -
```
Expected: RMS>0, speech-shaped while the far end talks. Stop: three candidate PCMs all digital
silence (max ≈ 0) during active speech → rooted capture closed on that device. (Also check whether
downlink exists but uplink does not, or vice versa; one direction alone is not enough.)

**E4 — Vendor mixer/mixer-path hunt (after E2/E3).**
```sh
adb shell su -c 'grep -iE "voc_rec|voice|incall" /vendor/etc/*mixer*.xml 2>/dev/null | head -80'
adb shell su -c 'tinymix -D 0 "VOC_REC_DL" 1'   # example; only if the control exists
```
Expected: `VOC_REC_UL/DL` present but disabled on many Qualcomm ROMs → enabling may revive capture;
if the mixer XML has no voice-capture path at all, that is a vendor decision, not a config mistake.
Stop: do not patch/overlay ROM audio XML for production; if a device needs it, it is a lab device.

**E5 — Privileged-app `CAPTURE_AUDIO_OUTPUT` probe (root; alternative to ALSA).**
```sh
# Magisk/priv-app install + /system/etc/permissions/privapp-permissions-app.xml with CAPTURE_AUDIO_OUTPUT; reboot
adb shell su -c 'grep -r CAPTURE_AUDIO_OUTPUT /system/etc/permissions/ | head'
adb shell dumpsys package <pkg> | grep -i capture_audio_output
# app: MediaRecorder/AudioRecord with VOICE_CALL then VOICE_DOWNLINK; 10 s during speech
```
Expected: permission granted; audio works on some devices, throws or is silent on others. Stop:
permission confirmed granted but silent → the HAL lacks the path; no amount of privilege helps.

**E6 — Bluetooth HFP tap (no root; after E0, in parallel with E2).**
```sh
# host: stop PulseAudio, start BlueALSA with HFP-HF, pair/trust the phone, then during a call:
bluealsactl list        # (v5) list PCMs / find the phone's HF PCM
arecord -D bluealsa:DEV=<mac>,PROFILE=hfp-hf -f S16_LE -r 16000 -c 1 /tmp/cap.wav
aplay  -D bluealsa:DEV=<mac>,PROFILE=hfp-hf /tmp/from-matrix.wav
```
Expected: SCO link forms, far-end audio lands in the capture file, audio played reaches the caller.
Stop: SCO fails with two different adapters → close the BT track.

**E7 — USB audio gadget tap (no root; alternative to E6).**
Configure a Pi Zero as a UAC1 headset (`dwc2` + `g_audio`), connect to the phone, then during a call
check `adb shell dumpsys audio | grep -i usb` and capture on the Pi. Expected: the phone shows the
USB headset and routes call audio to it (Pixel likely; verify office phone). Stop: call audio stays
on earpiece/speaker with the gadget attached → close for that device.

**E8 — End-to-end prototype (only after E3/E5/E6/E7 yields audio).**
Pick the cheapest working tap (E6/E7 strongly preferred: no root, no per-device mixer). Build:
1. Answer control in a small app (`ANSWER_PHONE_CALLS`, or default dialer on the dedicated phone).
2. Opus 16 kHz mono → WS/HTTPS channel to the bridge → LiveKit SFU as the ghost participant
   (reuse `mrtc-sip-bridge` if the tap is on SIP-friendly ground, or the LiveKit Go SDK path from
   `SFRAME_CALLS_M0_FINDINGS.md`).
3. Measure: one-way latency (send a click, measure), jitter-buffer adaptation, echo behavior,
   CPU/battery on the phone, recovery after LTE handover.
Stop: if measured added one-way latency > 150 ms consistently or calls are not intelligible with
Matrix playback (echo/double-talk), do not ship; keep native calls.

### Ladder summary

| Step | Root | Expected | Stop condition |
|---|---|---|---|
| E0 control | no | works | cannot answer/place at all |
| E1 forwarding | no | works (verify per SIM) | carrier rejects forwarding |
| E2 PCM discovery | yes | device-dependent; Qualcomm/Exynos hopeful | no in-call capture PCM on a device |
| E3 capture probe | yes | speech on the right PCM | all candidates digital silence |
| E4 mixer hunt | yes | VOC_REC exists on Qualcomm | no voice capture path in vendor mixer |
| E5 priv-app capture | yes | works where HAL routes | granted yet silent |
| E6 BT HFP tap | no | likely with a good dongle | SCO won't link |
| E7 USB tap | no | device-dependent | no call routing to USB |
| E8 prototype | maybe | after any audio works | latency > 150 ms added or echo |

---

## 7. Verdict

- **Answering from Matrix is not the hard part**; streaming the carrier call's audio is, and it is
  not possible through supported APIs. Any design that promises "answer from Matrix" for a normal
  SIM must either move the audio path off the phone (forwarding/gateway) or bypass the audio policy
  locally (rooted ALSA, privileged app) or bypass the phone's own call audio entirely (Bluetooth/USB
  headset route).
- **Recommended path, in order:**
  1. Keep the carrier-forward-to-SIP fallback as the product-grade answer for normal phones
     (E1 + `mrtc-sip-bridge`); it works today, needs no phone code, and is the only option that
     survives on non-root degoogled devices. Costs ~US$1–2/month for a DID; the phone stops ringing
     (or rings first with conditional forwarding).
  2. For the office phone, treat the no-root headset tap (E6 BT HFP, E7 USB audio) as the
     self-hosted alternative: the phone stays the SIM/voice endpoint, a Linux host becomes the
     bridge, and only an app for call control is needed on the phone. This is the best
     quality/effort/robustness compromise we found for "two-way audio into Matrix".
  3. Run E2–E5 as a bounded spike on the office phone and the Pixel only to answer "does this
     device expose call PCM". If yes, the Qualcomm `VOC_REC_DL` + `Incall_Music` pattern (or the
     Exynos `calliope` PCM) gives the cleanest, lowest-latency bridge; if no, stop — do not chase
     ROM mixer patches for a product.
- **Honest expectation for E2–E5**: maybe 40–60 % on a rooted Qualcomm LineageOS device that
  exposes `VOC_REC`; lower on Pixel/Tensor (proprietary ABOX, no public precedent found); the
  no-root taps (E6/E7) are more likely to succeed than rooted capture.
- Legal/consent: bridging someone else's call so another party joins/hears it has wiretap/consent
  implications in many jurisdictions; gate the feature and document it. Carrier ToS apply to
  gateways and forwarding.

**Bottom line for the user:** "answer from Matrix" is achievable today only by making the carrier
deliver the call somewhere else (forwarding/SIP). Keeping the SIM in the phone and answering its
audio from Matrix requires a local audio tap; the practical forms are Bluetooth/USB (no root, lab
quality) or rooted ALSA/privileged capture (device-specific, potentially impossible). Nothing in
Android's public API allows capturing a carrier call's uplink+downlink in a third-party app, on any
version.

---

## Sources (checked 2026-10-27)

- AOSP `core/res/AndroidManifest.xml` (main): `CAPTURE_AUDIO_OUTPUT` and
  `CAPTURE_VOICE_COMMUNICATION_OUTPUT` `signature|privileged|role`; `CALL_AUDIO_INTERCEPTION`
  `@SystemApi @hide`, "access the uplink and downlink audio of an ongoing call. Not for use by
  third-party applications"; `ANSWER_PHONE_CALLS` `dangerous|runtime`; `MODIFY_PHONE_STATE`
  `signature|privileged|role`; `BIND_INCALL_SERVICE`; `MANAGE_ONGOING_CALLS` `signature|appop`;
  `BIND_CALL_STREAMING_SERVICE` `signature`.
  <https://android.googlesource.com/platform/frameworks/base/+/refs/heads/main/core/res/AndroidManifest.xml>
- Android API docs, `MediaRecorder.AudioSource`: `VOICE_CALL`/`VOICE_DOWNLINK`/`VOICE_UPLINK`
  require `CAPTURE_AUDIO_OUTPUT`, "reserved for use by system components".
  <https://developer.android.com/reference/android/media/MediaRecorder.AudioSource>
- Android API docs, `TelecomManager.acceptRingingCall()`: requires `ANSWER_PHONE_CALLS` or
  `MODIFY_PHONE_STATE`; deprecation and self-managed caveats.
  <https://developer.android.com/reference/android/telecom/TelecomManager>
- Android API docs, `InCallService`: default dialer via `RoleManager.ROLE_DIALER`, `ACTION_DIAL`,
  `BIND_INCALL_SERVICE`, wearable companion path with `MANAGE_ONGOING_CALLS`.
  <https://developer.android.com/reference/android/telecom/InCallService>
- AOSP `telecomm/java/android/telecom/CallStreamingService.java`: `@SystemApi @hide`, voip call
  streaming, `BIND_CALL_STREAMING_SERVICE` `signature`.
  <https://android.googlesource.com/platform/frameworks/base/+/refs/heads/main/telecomm/java/android/telecom/CallStreamingService.java>
- Android API docs, `AudioPlaybackCaptureConfiguration`: only `USAGE_UNKNOWN`/`USAGE_GAME`/
  `USAGE_MEDIA` are capturable; voice communication is not.
  <https://developer.android.com/reference/android/media/AudioPlaybackCaptureConfiguration>
- AOSP `AudioService.java` (main): voice-communication capture mixes need
  `CAPTURE_VOICE_COMMUNICATION_OUTPUT`.
  <https://android.googlesource.com/platform/frameworks/base/+/refs/heads/main/services/core/java/com/android/server/audio/AudioService.java>
- Android docs, supported media formats: Opus **encoder** from Android 10.
  <https://developer.android.com/media/platform/supported-formats>
- LineageOS Dialer (lineage-22.2): `privileged: true`, `CAPTURE_AUDIO_OUTPUT` in the manifest;
  `call_recording_audio_source` defaults to 1 (MIC), "4 (VOICE_CALL) if supported by device";
  region-gated `call_record_states.xml`.
  <https://github.com/LineageOS/android_packages_apps_Dialer>
- Google Phone app help: call recording requires Pixel 6+/Android 14+, region-limited.
  <https://support.google.com/phoneapp/answer/9803950>
- AOSP `TelecomShellCommand.java`: `cmd telecom` subcommands (`set-default-dialer`,
  `get-default-dialer`, `set-system-dialer`, …).
  <https://android.googlesource.com/platform/packages/services/Telecomm/+/refs/heads/main/src/com/android/server/telecom/TelecomShellCommand.java>
- `s-xander/Android-sip-gateway` (claimed): rooted Qualcomm phone as GSM↔SIP gateway using
  `VOC_REC_DL`/`VOC_REC_UL`/`Incall_Music`, PJSIP, SELinux permissive, `/dev/snd` chmod 666;
  tested on Redmi Note 7 (SDM660, LineageOS 17.1). Not independently verified.
  <https://github.com/s-xander/Android-sip-gateway>
- `krantiutils/ring-gateway` (claimed): rooted Samsung Exynos ALSA injection into a live call
  (`tinyplay` → `pcmC0D8p`/RDMA8 → SIFS5), in-call PCMs `pcmC0D104p`/`pcmC0D113c` (calliope_4/13),
  `AudioTrack`/`STREAM_VOICE_CALL` fails on Pixel and Samsung, programmatic hangup needs
  `InCallService`. Not independently verified.
  <https://github.com/krantiutils/ring-gateway>
- `althafvly/android-call-recorder`: README "MOST PHONES DOES NOT SUPPORT CALL RECORDING";
  MixerPaths patching re-enables the `VOC_REC` control that manufacturers disable.
  <https://github.com/althafvly/android-call-recorder>
- tinyalsa (`tinycap`, `tinymix`, `tinyplay`), usage/build.
  <https://github.com/tinyalsa/tinyalsa>
- BlueALSA wiki, "Using BlueALSA with HFP and HSP Devices": HFP-HF role, AG creates the SCO audio
  connection, BlueALSA does not do call management, SCO adapter caveats.
  <https://github.com/Arkq/bluez-alsa/wiki/Using-BlueALSA-with-HFP-and-HSP-Devices>
- `williamkray/mrtc-sip-bridge`: working SIP↔MatrixRTC (LiveKit) bridge, echoes the jmpchat stack;
  rooms must be unencrypted (documented).
  <https://github.com/williamkray/mrtc-sip-bridge>
- Internal: `mautrix-jmpchat/docs/SFRAME_CALLS_M0_FINDINGS.md` (LiveKit Go SDK SFrame, key
  delivery M2), `mautrix-android-sms-mms/docs/PLAN.md` (app transport), repo `TODO.md`
  (device/carrier facts).
- Gateway pricing/background: GoIP8-4G US$620 <https://goantifraud.com/goip-equipment/48-goip8-4g>;
  Portech MV-370L €474.81
  <https://voip.world/Portech-MV-370-4G_VoIP-LTE-Gateway>; Dinstar UC2000-VA-1G €178.50 (2G)
  <https://voip.world/Dinstar-DWG2000-1G-1-channel-VoIP-GSM-Gateway_1>; Yeastar TG800LTE-A
  US$1,752 <https://flyteccomputers.com/voip-systems/voice-gateway/gsm-cdma-4g-5g-lte-gateway/>;
  2G-vs-4G GoIP guide <https://meni.ge/en/docs/sip-gateway-goip>.
- Call forwarding: GSM/ETSI codes <https://en.wikipedia.org/wiki/Call_forwarding>; US carrier
  codes <https://bubblyphone.com/hub/call-forwarding-codes> (Verizon `*72`/`*73`, AT&T/T-Mobile
  `*21*number#`, `##002#` disable-all); Ting help page for the `*72`/`*73` flow
  <https://help.ting.com/mobile-articles/using-call-forwarding>.
- DID pricing: Telnyx US$1.00/number/month (voice+SMS) <https://telnyx.com/pricing/numbers>.
