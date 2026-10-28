#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t21_call_capture.sh — acceptance step: place one real phone-SIM call between
# two rooted devices (A calls B), answer it on B and capture the in-call audio
# on B through the staged rooted tinyalsa tooling in $E3_DIR (see
# docs/cell-call-over-matrix-research.md §6 E2/E3).
#
# Stages:
#   1. precheck  both devices online, root available, $E3_DIR tooling present,
#                capture mixer route resolvable, no active call on either.
#   2. dial      A: default calling account selected (cmd telecom, when the
#                build supports it), ACTION_CALL to B, Pixel-style "Choose SIM"
#                account picker handled via uiautomator tap.
#   3. active    B answers (uiautomator/keyevent); poll mCallState/telecom until
#                ACTIVE within CALL_ACTIVE_TIMEOUT. On setup failure, collect the
#                exact carrier/IMS evidence (telecom ImsReasonInfo, radio
#                reason codes) and the callee-never-rang verdict, then FAIL.
#   4. capture   ACTIVE only: flip B's VOC_REC_DL mixer On, tinycap
#                CAPTURE_SECONDS, verify size + non-silence (sox, else python3),
#                restore the mixer Off, hang up both.
#   5. cleanup   always: mixers Off, end any calls, remove temp files.
#
# Environment contract
# --------------------
#   PHONE_A_SERIAL    adb serial of the caller (required).
#   PHONE_A_SSH       run adb for A on that ssh host (empty: local adb).
#   PHONE_A_NUMBER    A's own E.164 number; matches the SIM row of a "Choose
#                     SIM"/account picker (optional, improves picker handling).
#   PHONE_B_SERIAL    adb serial of the callee/capture device (required).
#   PHONE_B_SSH       run adb for B on that ssh host (empty: local adb).
#   PHONE_B_NUMBER    number to dial on B (required, E.164).
#   PHONE_B_MIXER_DL  mixer control that routes the in-call downlink into the
#                     capture PCM (default: per-device map below, then a single
#                     `MultiMedia* Mixer VOC_REC_DL` candidate on the device).
#   PHONE_B_MIXER_UL  companion UL control; only read back/restored (optional).
#   PHONE_B_PCM_DEVICE  ALSA capture PCM for tinycap (default from the map or
#                     derived from the control name via /proc/asound/pcm).
#   PHONE_B_PCM_CARD  ALSA card for tinycap (default 0).
#   E3_DIR            on-device staging dir (default /data/local/tmp/e3).
#   CALL_ACTIVE_TIMEOUT  seconds to wait for the call to become ACTIVE (45).
#   CAPTURE_SECONDS   tinycap capture length in seconds (10).
#   PICKER_WAIT       seconds to wait for/pick the calling-account picker (15).
#   KEEP_TMP          1 = keep the host temp dir and the device WAV after the run.
#
# Known lab devices (ro.product.device -> route): OnePlus9Pro (OnePlus 9 Pro,
#   LineageOS 23) = `MultiMedia9 Mixer VOC_REC_DL/UL`, PCM 23; taimen (Pixel 2
#   XL) = `MultiMedia1 Mixer VOC_REC_DL/UL`, PCM 0.
#
# Evidence rule: ids, sizes, timestamps and level/rms numbers only — audio
# content is never logged. The captured WAV stays on the device; a temporary
# copy is pulled only for the host-side sox/python stats and is removed again
# unless KEEP_TMP=1.
#
# Unreachable devices, missing root/staging or a pre-existing call SKIP; a call
# that dies during setup FAILs with the carrier/IMS cause; a call that connects
# must produce a non-silent WAV.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

PHONE_A_SERIAL="${PHONE_A_SERIAL:-}"
PHONE_A_SSH="${PHONE_A_SSH:-}"
PHONE_A_NUMBER="${PHONE_A_NUMBER:-}"
PHONE_B_SERIAL="${PHONE_B_SERIAL:-}"
PHONE_B_SSH="${PHONE_B_SSH:-}"
PHONE_B_NUMBER="${PHONE_B_NUMBER:-}"
PHONE_B_MIXER_DL="${PHONE_B_MIXER_DL:-}"
PHONE_B_MIXER_UL="${PHONE_B_MIXER_UL:-}"
PHONE_B_PCM_DEVICE="${PHONE_B_PCM_DEVICE:-}"
PHONE_B_PCM_CARD="${PHONE_B_PCM_CARD:-0}"
E3_DIR="${E3_DIR:-/data/local/tmp/e3}"
CALL_ACTIVE_TIMEOUT="${CALL_ACTIVE_TIMEOUT:-45}"
CAPTURE_SECONDS="${CAPTURE_SECONDS:-10}"
PICKER_WAIT="${PICKER_WAIT:-15}"
KEEP_TMP="${KEEP_TMP:-0}"

require_env PHONE_A_SERIAL PHONE_B_SERIAL PHONE_B_NUMBER
require_int "CALL_ACTIVE_TIMEOUT" "$CALL_ACTIVE_TIMEOUT"
require_int "CAPTURE_SECONDS" "$CAPTURE_SECONDS"
require_int "PICKER_WAIT" "$PICKER_WAIT"

FAILURES=0
SKIPS=0
STAGE_LINES=()

skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }
stage_line() { STAGE_LINES+=("$1"); }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t21.XXXXXX")"
# Host-side UI dump parser (used when python3 is available; the script falls
# back to a sed match otherwise).
cat >"$WORK/ui_tap.py" <<'PY'
#!/usr/bin/env python3
"""Find the tap target for a uiautomator XML dump.

Usage: ui_tap.py XML (number DIGITS | regex PATTERN)
Prints "x y" for the first match; taps the smallest clickable node containing
the match centre. Exit 1 = no match, 2 = unparsable XML.
"""
import re
import sys
import xml.etree.ElementTree as ET

path, mode, value = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    root = ET.parse(path).getroot()
except Exception as exc:  # noqa: BLE001
    print(f"parse error: {exc}", file=sys.stderr)
    sys.exit(2)


def bounds(node):
    match = re.findall(r"\d+", node.get("bounds") or "")
    return tuple(map(int, match)) if len(match) == 4 else None


nodes = list(root.iter("node"))
candidates = []
for node in nodes:
    text = "|".join([node.get("text") or "", node.get("content-desc") or ""])
    if mode == "number":
        digits = re.sub(r"\D", "", text)
        if digits and digits.endswith(value):
            candidates.append(node)
    elif re.search(value, text, re.I):
        candidates.append(node)
if not candidates:
    sys.exit(1)

candidate = bounds(candidates[0])
if not candidate:
    sys.exit(1)
cx = (candidate[0] + candidate[2]) // 2
cy = (candidate[1] + candidate[3]) // 2

enclosing = []
for node in nodes:
    if node.get("clickable") != "true":
        continue
    box = bounds(node)
    if box and box[0] <= cx <= box[2] and box[1] <= cy <= box[3]:
        enclosing.append(((box[2] - box[0]) * (box[3] - box[1]), box))
if enclosing:
    _, box = min(enclosing)
    cx = (box[0] + box[2]) // 2
    cy = (box[1] + box[3]) // 2
print(f"{cx} {cy}")
PY
DEV_TMP_FILES=()
CLEANUP_DONE=0
CALL_ANSWER_TRIED=0
ROOT_MODE_A=""
ROOT_MODE_B=""
ACCOUNT_RESTORE=""
CAPTURE_REMOTE=""
CAPTURE_STARTED_MS=""

log "t21_call_capture: A=$PHONE_A_SERIAL${PHONE_A_SSH:+ (via $PHONE_A_SSH)} B=$PHONE_B_SERIAL${PHONE_B_SSH:+ (via $PHONE_B_SSH)} E3_DIR=$E3_DIR"
[ -n "$PHONE_B_NUMBER" ] && log "t21_call_capture: dialing B ($PHONE_B_NUMBER from A)"

# ---------------------------------------------------------------------------
# Device transport (per-device ssh host, like lib.sh's ADB_SSH but for two)
# ---------------------------------------------------------------------------

t21_dev_serial() {
	case "$1" in
	A) printf '%s' "$PHONE_A_SERIAL" ;;
	B) printf '%s' "$PHONE_B_SERIAL" ;;
	*) die "t21_dev_serial: unknown device '$1'" ;;
	esac
}

t21_dev_ssh() {
	case "$1" in
	A) printf '%s' "$PHONE_A_SSH" ;;
	B) printf '%s' "$PHONE_B_SSH" ;;
	*) die "t21_dev_ssh: unknown device '$1'" ;;
	esac
}

# t21_dev_cmd DEV ARGS... — run one adb command for DEV, locally or via ssh.
# Every argument is quoted for the remote shell; with a local adb the args are
# passed through verbatim.
t21_dev_cmd() {
	local dev="$1"
	shift
	local serial ssh_host remote arg
	serial="$(t21_dev_serial "$dev")"
	ssh_host="$(t21_dev_ssh "$dev")"
	if [ -n "$ssh_host" ]; then
		remote="adb -s $(shell_quote "$serial")"
		for arg in "$@"; do
			remote="$remote $(shell_quote "$arg")"
		done
		ssh -n "$ssh_host" "$remote"
	else
		adb -s "$serial" "$@"
	fi
}

# t21_dev_shell DEV CMD — run CMD in the device shell (one argument, so CMD
# keeps its own quoting through adb and the optional ssh layer).
t21_dev_shell() {
	t21_dev_cmd "$1" shell "$2"
}

# t21_dev_exec_out DEV ARGS... — binary-safe `exec-out` for DEV.
t21_dev_exec_out() {
	local dev="$1"
	shift
	t21_dev_cmd "$dev" exec-out "$@"
}

# t21_root_shell DEV CMD — run CMD as root (direct when adbd is root, else
# `su -c 'CMD'` for Magisk-style su).
t21_root_shell() {
	local dev="$1" cmd="$2" mode
	case "$dev" in
	A) mode="$ROOT_MODE_A" ;;
	B) mode="$ROOT_MODE_B" ;;
	esac
	if [ "$mode" = "su" ]; then
		t21_dev_shell "$dev" "su -c $(shell_quote "$cmd")"
	else
		t21_dev_shell "$dev" "$cmd"
	fi
}

# t21_root_mode DEV — print direct|su|none and verify uid=0.
t21_root_mode() {
	local dev="$1" out rc=0
	out="$(t21_dev_shell "$dev" 'id' 2>&1)" || rc=$?
	if [ "$rc" -eq 0 ]; then
		case "$out" in
		*uid=0*) printf 'direct'; return 0 ;;
		esac
	fi
	rc=0
	out="$(t21_dev_shell "$dev" "su -c 'id'" 2>&1)" || rc=$?
	if [ "$rc" -eq 0 ]; then
		case "$out" in
		*uid=0*) printf 'su'; return 0 ;;
		esac
	fi
	printf 'none'
}

# ---------------------------------------------------------------------------
# Device state probes
# ---------------------------------------------------------------------------

# t21_mcallstate DEV — highest TelephonyManager call state on DEV:
# 0 idle, 1 ringing, 2 offhook (dialing/answered). Empty when unreadable.
t21_mcallstate() {
	local out
	out="$(t21_dev_shell "$1" "dumpsys telephony.registry | grep -oE 'mCallState=[0-9]+' | cut -d= -f2 | sort -n | tail -1" 2>/dev/null | tr -d '\r')" || true
	printf '%s' "${out:-}"
}

# t21_audio_state DEV — "In ring or call: true|false" from dumpsys audio.
t21_audio_state() {
	local out
	out="$(t21_dev_shell "$1" "dumpsys audio | grep -m1 -oE 'In ring or call: (true|false)'" 2>/dev/null | tr -d '\r')" || true
	printf '%s' "${out:-}"
}

# t21_dev_idle DEV — 0 when DEV has no call in any state.
t21_dev_idle() {
	local state audio
	state="$(t21_mcallstate "$1")"
	audio="$(t21_audio_state "$1")"
	[ "${state:-0}" = "0" ] && [ "$audio" != "In ring or call: true" ]
}

# t21_telecom_dump DEV OUTFILE — save `dumpsys telecom` of DEV.
t21_telecom_dump() {
	t21_dev_shell "$1" 'dumpsys telecom' >"$2" 2>/dev/null
	[ -s "$2" ]
}

# t21_terminated_since FILE SINCE_MS — "yes" when FILE (a telecom dump)
# contains a call record that ended at/after SINCE_MS and carries a
# callTerminationReason. Used to recognise a setup failure even when the
# DIALING->idle transition was missed by the poll loop.
t21_terminated_since() {
	awk -v t="$2" '
		/Call TC@/ { inb=1; end=""; term=0 }
		inb && $1 == "endTime:" { end=$2 }
		inb && /callTerminationReason:/ { term=1 }
		inb && /^[[:space:]]*}[[:space:]]*$/ {
			if (term && end + 0 >= t) hit=1
			inb=0
		}
		END { print (hit ? "yes" : "no") }' "$1" 2>/dev/null | tail -1
}

# t21_radio_window DEV SINCE_S UNTIL_S REGEX — radio-buffer lines of DEV in the
# [SINCE,UNTIL] epoch-second window matching REGEX (case-insensitive), tail 12.
t21_radio_window() {
	local dev="$1" since="$2" until="$3" regex="$4"
	t21_dev_shell "$dev" "logcat -b radio -d -v epoch | awk -v a=$since -v b=$until '\$1+0>=a-2 && \$1+0<=b+2' | grep -iE $(shell_quote "$regex") | tail -n 12" 2>/dev/null | tr -d '\r' || true
}

# ---------------------------------------------------------------------------
# UI automation (uiautomator dump + tap)
# ---------------------------------------------------------------------------

# t21_q — single-quote VALUE for one device-shell layer.
t21_q() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# t21_ui_dump DEV OUT — dump the current UI hierarchy of DEV into OUT (stdout).
# A unique on-device path is tracked for cleanup.
t21_ui_dump() {
	local dev="$1" out="$2" remote rc=0
	remote="/data/local/tmp/t21_ui_$$_${#DEV_TMP_FILES[@]}.xml"
	DEV_TMP_FILES+=("$dev|$remote")
	t21_dev_shell "$dev" "uiautomator dump $(t21_q "$remote") >/dev/null 2>&1; cat $(t21_q "$remote") 2>/dev/null" >"$out" 2>/dev/null || rc=$?
	[ "$rc" -eq 0 ] && [ -s "$out" ]
}

# t21_ui_find_tap XML MODE VALUE — print "x y" of the tap target. MODE=number
# matches a node whose non-digit text ends with VALUE (e.g. the SIM row of a
# "Choose SIM" picker carrying the caller's number); MODE=regex matches
# text/content-desc. When the match itself is not clickable, the smallest
# clickable node containing its centre is tapped. Empty output when absent.
t21_ui_find_tap() {
	local xml="$1" mode="$2" value="$3" rc=0
	if command -v python3 >/dev/null 2>&1; then
		python3 "$WORK/ui_tap.py" "$xml" "$mode" "$value" 2>/dev/null || rc=$?
		# rc 1 = no match, 2 = unparsable XML; both mean "no coordinates".
		[ "$rc" -eq 0 ] || return 0
	else
		t21_ui_find_tap_sed "$xml" "$mode" "$value"
	fi
}

# t21_ui_find_tap_sed — sed fallback used when python3 is unavailable. Matches
# the first node whose text matches, then taps the centre of that node.
t21_ui_find_tap_sed() {
	local xml="$1" mode="$2" value="$3" regex line bounds
	if [ "$mode" = "number" ]; then
		# Insert [^0-9]* between the digits so "1 555-010-0201" matches too.
		regex="$(printf '%s' "$value" | sed 's/./[^0-9]*&/g')"
	else
		regex="$value"
	fi
	line="$(sed 's/></>\n</g' "$xml" 2>/dev/null | grep -iE "$regex" | head -n 1)"
	[ -n "$line" ] || return 0
	bounds="$(printf '%s' "$line" | grep -oE 'bounds="\[[0-9]+,[0-9]+\]\[[0-9]+,[0-9]+\]"' | head -n 1 | grep -oE '[0-9]+' | tr '\n' ' ')"
	# shellcheck disable=SC2086
	set -- $bounds
	[ "$#" -eq 4 ] || return 0
	printf '%s %s\n' "$((($1 + $3) / 2))" "$((($2 + $4) / 2))"
}

# t21_ui_tap_tap DEV COORDS — tap "x y" on DEV.
t21_ui_tap() {
	# shellcheck disable=SC2086
	t21_dev_shell "$1" "input tap $2" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Precheck
# ---------------------------------------------------------------------------

# t21_resolve_route — resolve $PHONE_B_MIXER_DL/UL and $PHONE_B_PCM_DEVICE for
# the capture side. Explicit env wins; the two known lab serials have defaults;
# any other device must expose exactly one `MultiMedia* Mixer VOC_REC_DL`.
t21_resolve_route() {
	local serial="$1" controls count pcm device
	if [ -z "$PHONE_B_MIXER_DL" ]; then
		device="$(t21_root_shell B "getprop ro.product.device" 2>/dev/null | tr -d '\r' | tr 'A-Z' 'a-z')" || true
		case "$device" in
		oneplus9pro)
			PHONE_B_MIXER_DL="MultiMedia9 Mixer VOC_REC_DL"
			PHONE_B_MIXER_UL="${PHONE_B_MIXER_UL:-MultiMedia9 Mixer VOC_REC_UL}"
			PHONE_B_PCM_DEVICE="${PHONE_B_PCM_DEVICE:-23}"
			;;
		taimen)
			PHONE_B_MIXER_DL="MultiMedia1 Mixer VOC_REC_DL"
			PHONE_B_MIXER_UL="${PHONE_B_MIXER_UL:-MultiMedia1 Mixer VOC_REC_UL}"
			PHONE_B_PCM_DEVICE="${PHONE_B_PCM_DEVICE:-0}"
			;;
		*)
			controls="$(t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD controls | grep -oE 'MultiMedia[0-9]+ Mixer VOC_REC_DL' | sort -u" 2>/dev/null | tr -d '\r')" || true
			count="$(printf '%s\n' "$controls" | sed '/^$/d' | wc -l | tr -d ' ')"
			if [ "$count" = "1" ]; then
				PHONE_B_MIXER_DL="$controls"
				PHONE_B_MIXER_UL="$(printf '%s' "$controls" | sed 's/VOC_REC_DL$/VOC_REC_UL/')"
			fi
			;;
		esac
	fi
	if [ -z "$PHONE_B_PCM_DEVICE" ] && [ -n "$PHONE_B_MIXER_DL" ]; then
		# Derive the PCM device from the MultiMedia<N> prefix of the control.
		pcm="$(t21_dev_shell B "grep -E '^$(printf '%02d' "$PHONE_B_PCM_CARD")-[0-9]+: $(printf '%s' "$PHONE_B_MIXER_DL" | sed -E 's/ Mixer.*//')( |$)' /proc/asound/pcm | sed -E 's/^[0-9]+-([0-9]+):.*/\1/' | head -n 1" 2>/dev/null | tr -d '\r')" || true
		[ -n "$pcm" ] && PHONE_B_PCM_DEVICE="$pcm"
	fi
}

t21_precheck_dev() {
	local dev="$1" label="$2" state="" rc=0
	label="${label:-$dev}"
	state="$(t21_dev_cmd "$dev" get-state 2>&1 | tr -d '\r')" || rc=$?
	if [ "$rc" -ne 0 ] || [ "$state" != "device" ]; then
		skip_check "$label: adb state '${state:-no output}' (want device)"
		printf 'no'
		return 0
	fi
	log "precheck: $label online (serial $(t21_dev_serial "$dev"))"

	local mode
	mode="$(t21_root_mode "$dev")"
	case "$dev" in A) ROOT_MODE_A="$mode" ;; B) ROOT_MODE_B="$mode" ;; esac
	if [ "$mode" = "none" ]; then
		skip_check "$label: no root (direct adbd and su both refused)"
		printf 'no'
		return 0
	fi
	log "precheck: $label root available ($mode)"

	if ! t21_root_shell "$dev" "test -x $(t21_q "$E3_DIR/tinycap") && test -x $(t21_q "$E3_DIR/tinymix")"; then
		skip_check "$label: staged tooling missing in $E3_DIR (tinycap/tinymix)"
		printf 'no'
		return 0
	fi
	log "precheck: $label staging present in $E3_DIR"

	if ! t21_dev_idle "$dev"; then
		skip_check "$label: a call is already active/ringing (state '$(t21_mcallstate "$dev")', '$(t21_audio_state "$dev")')"
		printf 'no'
		return 0
	fi
	printf 'yes'
}

# ---------------------------------------------------------------------------
# Dial stage (A)
# ---------------------------------------------------------------------------

# t21_wake DEV — wake the screen and dismiss a non-secure keyguard (best
# effort; a secure keyguard is fine, the call UI shows above it).
t21_wake() {
	t21_dev_shell "$1" "input keyevent 224; wm dismiss-keyguard >/dev/null 2>&1 || true" >/dev/null 2>&1 || true
}

# t21_select_calling_account — when A's build implements `cmd telecom
# set-user-selected-outgoing-phone-account`, make the SIM account the default
# outgoing account so ACTION_CALL does not open an account picker.
t21_select_calling_account() {
	local help="" dump="$WORK/a-telecom-pre.txt" current="" acc=""
	# `cmd telecom help` exits non-zero after printing the usage; keep the text.
	help="$(t21_dev_shell A 'cmd telecom help' 2>&1)" || true
	case "$help" in
	*set-user-selected-outgoing-phone-account*) ;;
	*)
		log "dial: A does not implement cmd telecom set-user-selected-outgoing-phone-account (Pixel-style build); account picker will be handled via UI"
		return 0
		;;
	esac
	t21_telecom_dump A "$dump" || return 0
	current="$(sed -n 's/^  outgoingPhoneAccountForTelScheme: //p' "$dump" | head -n 1)"
	acc="$(sed -n 's/.*PhoneAccount: ComponentInfo{\([^}]*TelephonyConnectionService\)}, \([0-9]*\), UserHandle{\([0-9]*\)}.*/\1|\2|\3/p' "$dump" | head -n 1)"
	if [ -z "$acc" ]; then
		log "dial: no telephony PhoneAccount found on A; leaving the outgoing account alone"
		return 0
	fi
	local comp id user want
	comp="${acc%%|*}"
	acc="${acc#*|}"
	id="${acc%%|*}"
	user="${acc#*|}"
	want="ComponentInfo{$comp}, $id, UserHandle{$user}"
	if [ "$current" = "$want" ]; then
		log "dial: A outgoing calling account already $want"
		return 0
	fi
	log "dial: setting A outgoing calling account to $want (was '${current:-unset}')"
	if t21_dev_shell A "cmd telecom set-user-selected-outgoing-phone-account $comp $id $user" >/dev/null 2>&1; then
		[ -n "$current" ] && case "$current" in ComponentInfo*) ACCOUNT_RESTORE="$comp $id $user" ;; esac
		log "dial: default calling account set"
	else
		log "dial: cmd telecom set-user-selected-outgoing-phone-account failed; account picker will be handled via UI"
	fi
}

# t21_handle_picker A — when the "Choose SIM"/account picker is showing, tap
# the SIM row (matched by A's own number, then by SIM-ish labels).
t21_handle_picker() {
	local dev="$1" ui="$WORK/picker-ui.xml" digits="" coords=""
	t21_ui_dump "$dev" "$ui" || return 1
	grep -qiE 'Choose SIM|Select.*account|Choose an account|Call using|account for this call' "$ui" || return 1
	digits="$(printf '%s' "$PHONE_A_NUMBER" | tr -cd '0-9')"
	if [ -n "$digits" ]; then
		digits="${digits: -10}"
		coords="$(t21_ui_find_tap "$ui" number "$digits" || true)"
	fi
	if [ -z "$coords" ]; then
		coords="$(t21_ui_find_tap "$ui" regex 'SIM|mobile|phone account|cellular' || true)"
	fi
	if [ -z "$coords" ]; then
		log "dial: account picker present but no SIM row matched (digits '${digits:-<none>}'); tapping the picker's first dialog row is unsafe, continuing to the call poll"
		return 1
	fi
	t21_ui_tap "$dev" "$coords"
	log "dial: account picker handled — tapped SIM row at ($coords)"
	sleep 2
	return 0
}

# t21_dial — start the call on A and clear the account picker while no call
# state has appeared yet.
t21_dial() {
	local out="" rc=0 deadline
	t21_wake A
	t21_select_calling_account
	out="$(t21_dev_shell A "am start -a android.intent.action.CALL -d $(t21_q "tel:$PHONE_B_NUMBER")" 2>&1)" || rc=$?
	if [ "$rc" -ne 0 ] || printf '%s' "$out" | grep -qiE 'Error|Exception|Permission Denial|does not exist'; then
		fail "dial: ACTION_CALL to $PHONE_B_NUMBER failed on A: $(printf '%s' "$out" | tr '\n' ' ' | head -c 300)"
		return 1
	fi
	log "dial: ACTION_CALL tel:$PHONE_B_NUMBER started on A"

	deadline=$((SECONDS + PICKER_WAIT))
	while [ "$SECONDS" -lt "$deadline" ]; do
		local state
		state="$(t21_mcallstate A)"
		if [ "${state:-0}" -ge 1 ]; then
			log "dial: call state ${state} on A (no account picker pending)"
			return 0
		fi
		t21_handle_picker A || true
		sleep 1
	done
	log "dial: no call state after ${PICKER_WAIT}s wait for the picker; continuing to the state poll"
	return 0
}

# ---------------------------------------------------------------------------
# ACTIVE poll (B answers)
# ---------------------------------------------------------------------------

# t21_answer_attempt DEV — tap the Answer control from a UI dump, else send
# KEYCODE_CALL/HEADSETHOOK.
t21_answer_attempt() {
	local dev="$1" ui="$WORK/answer-ui.xml" coords=""
	if t21_ui_dump "$dev" "$ui"; then
		coords="$(t21_ui_find_tap "$ui" regex 'answer|accept' || true)"
	fi
	if [ -n "$coords" ]; then
		t21_ui_tap "$dev" "$coords"
		log "active: answered on B by tapping ($coords)"
	else
		log "active: no Answer node found on B; sending KEYCODE_CALL (keyevent 5)"
	fi
	t21_dev_shell "$dev" 'input keyevent 5' >/dev/null 2>&1 || true
	sleep 1
	t21_dev_shell "$dev" 'input keyevent 79' >/dev/null 2>&1 || true
}

# t21_wait_active — 0 = ACTIVE, 1 = terminated before ACTIVE, 2 = timeout.
t21_wait_active() {
	local deadline=$((SECONDS + CALL_ACTIVE_TIMEOUT))
	local saw_dial=0 polls=0 a b a2 b2 term=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		polls=$((polls + 1))
		a="$(t21_mcallstate A)"
		b="$(t21_mcallstate B)"
		[ "${a:-0}" -ge 1 ] && saw_dial=1
		if [ "${b:-0}" -eq 1 ] && [ "$CALL_ANSWER_TRIED" -eq 0 ]; then
			CALL_ANSWER_TRIED=1
			t21_answer_attempt B || true
		fi
		if [ "${b:-0}" -eq 2 ] && [ "${a:-0}" -ge 1 ]; then
			log "active: call is ACTIVE (A state ${a}, B state ${b})"
			return 0
		fi
		if [ "${a:-0}" -eq 0 ] && [ "${b:-0}" -eq 0 ]; then
			# Both idle again: confirm the setup failure (either we saw the
			# offhook transition, or a fresh termination record appeared) so a
			# fast failure does not sit out the whole timeout.
			sleep 1
			a2="$(t21_mcallstate A)"
			b2="$(t21_mcallstate B)"
			if [ "${a2:-0}" -eq 0 ] && [ "${b2:-0}" -eq 0 ]; then
				term=""
				if [ "$polls" -ge 2 ]; then
					t21_telecom_dump A "$WORK/a-telecom-poll.txt" || true
					term="$(t21_terminated_since "$WORK/a-telecom-poll.txt" "$ATTEMPT_START_MS" 2>/dev/null || true)"
				fi
				if [ "$saw_dial" -eq 1 ] || [ "$term" = "yes" ]; then
					log "active: call left the offhook state without ever becoming ACTIVE"
					return 1
				fi
			fi
		fi
		sleep 1
	done
	return 2
}

# ---------------------------------------------------------------------------
# Failure evidence (step 5)
# ---------------------------------------------------------------------------

# t21_parse_ims_reason LINE — print `cause|name|sip|message` for an
# `ImsReasonInfo :: {339 : CODE_SIP_REQUEST_CANCELLED, 487, ocs ...}` line.
t21_parse_ims_reason() {
	local line="$1" inner cause name rest sip msg
	inner="${line#*\{}"
	inner="${inner%%\}*}"
	cause="${inner%% *}"
	name="$(printf '%s' "$inner" | sed -nE 's/^[0-9]+ : ([A-Za-z_]+).*/\1/p')"
	rest="${inner#*, }"
	if [ "$rest" = "$inner" ]; then
		printf '%s|%s|||\n' "$cause" "$name"
		return 0
	fi
	sip="${rest%%,*}"
	msg="${rest#*,}"
	[ "$msg" = "$rest" ] && msg=""
	msg="${msg# }"
	printf '%s|%s|%s|%s\n' "$cause" "$name" "$sip" "$msg"
}

# t21_collect_failure_evidence — fill EV_* globals from A's telecom/radio and
# B's logs; never audio content.
t21_collect_failure_evidence() {
	local since_s="$1" until_s="$2"
	EV_A_REASON_LINE=""
	EV_A_RADIO=""
	EV_B_MT=""
	EV_B_RANG="unknown"

	t21_telecom_dump A "$WORK/a-telecom.txt" || true
	if [ -s "$WORK/a-telecom.txt" ]; then
		# Prefer the call record (it carries direction/endTime context); fall
		# back to any ImsReasonInfo line (e.g. a state-machine history line).
		EV_A_REASON_LINE="$(grep -E 'callTerminationReason: DisconnectCause' "$WORK/a-telecom.txt" | tail -n 1)"
		[ -n "$EV_A_REASON_LINE" ] || EV_A_REASON_LINE="$(grep -E 'ImsReasonInfo :: \{' "$WORK/a-telecom.txt" | tail -n 1)"
	fi
	EV_A_RADIO="$(t21_radio_window A "$since_s" "$until_s" 'onCallTerminated|maybeRemapReasonCode|CallAnalytics|disconnect_extra_code|disconnect_reason_code|disconnect_extra_message|ocs terminated|ImsReasonInfo|CallFailCause' | grep -E 'reasonCode|Remap|CallAnalytics|disconnect_|ocs terminated|CallFailCause|ImsReasonInfo' | head -n 6)"

	EV_B_MT="$(t21_radio_window B "$since_s" "$until_s" 'incoming|ringing|invite|onCallSessionReceived')"
	local b_inc="no"
	t21_telecom_dump B "$WORK/b-telecom.txt" || true
	if [ -s "$WORK/b-telecom.txt" ]; then
		b_inc="$(awk -v t="$((since_s * 1000))" '
			/Call TC@/ { inb=1; dir=""; end="" }
			inb && $1 == "direction:" { dir=$2 }
			inb && $1 == "endTime:" { end=$2 }
			inb && /^[[:space:]]*}[[:space:]]*$/ {
				if (dir == "INCOMING" && end + 0 >= t) hit=1
				inb=0
			}
			END { print (hit ? "yes" : "no") }' "$WORK/b-telecom.txt" | tail -1)"
	fi
	if [ -z "$EV_B_MT" ] && [ "$b_inc" = "no" ]; then
		EV_B_RANG="never rang"
	elif [ "$b_inc" = "yes" ]; then
		EV_B_RANG="rang (incoming call record present)"
	elif [ -n "$EV_B_MT" ]; then
		EV_B_RANG="radio shows MT/ring activity"
	fi
	log "evidence: B 'callee never rang' verdict: $EV_B_RANG"
	[ -n "$EV_B_MT" ] && log "evidence: B radio: $(printf '%s' "$EV_B_MT" | head -n 1 | cut -c1-200)"
	[ -n "$EV_A_RADIO" ] && log "evidence: A radio: $(printf '%s' "$EV_A_RADIO" | head -n 1 | cut -c1-200)"
	[ -n "$EV_A_REASON_LINE" ] && log "evidence: A telecom: $(printf '%s' "$EV_A_REASON_LINE" | cut -c1-300)"
}

# ---------------------------------------------------------------------------
# Capture stage (B)
# ---------------------------------------------------------------------------

# t21_capture — flip the DL route On, tinycap, verify, restore.
t21_capture() {
	local wav_size="" host_wav="$WORK/capture.wav" stats="" peak="" rms=""
	CAPTURE_REMOTE="$E3_DIR/t21_cap_$$_$(date +%s).wav"
	if ! t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD set $(t21_q "$PHONE_B_MIXER_DL") 1" >/dev/null 2>&1; then
		fail "capture: cannot set ${PHONE_B_MIXER_DL}=1 on B"
		return 1
	fi
	local rb
	rb="$(t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD get $(t21_q "$PHONE_B_MIXER_DL")" 2>/dev/null | tr -d '\r')" || true
	case "$rb" in
	*On*) log "capture: ${PHONE_B_MIXER_DL}=On (read-back '${rb}')" ;;
	*)
		fail "capture: ${PHONE_B_MIXER_DL} did not read back On ('${rb}')"
		return 1
		;;
	esac

	CAPTURE_STARTED_MS="$(now_ms)"
	local out="" rc=0
	out="$(t21_root_shell B "$(t21_q "$E3_DIR/tinycap") $(t21_q "$CAPTURE_REMOTE") -D $PHONE_B_PCM_CARD -d $PHONE_B_PCM_DEVICE -c 1 -r 48000 -b 16 -t $CAPTURE_SECONDS" 2>&1)" || rc=$?
	log "capture: tinycap rc=$rc (started ${CAPTURE_STARTED_MS}ms), $(printf '%s' "$out" | tr '\n' ' ' | head -c 200)"
	# Restore the route immediately, regardless of tinycap's verdict.
	t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD set $(t21_q "$PHONE_B_MIXER_DL") 0" >/dev/null 2>&1 || true
	rb="$(t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD get $(t21_q "$PHONE_B_MIXER_DL")" 2>/dev/null | tr -d '\r')" || true
	case "$rb" in
	*On*) fail "capture: ${PHONE_B_MIXER_DL} still On after restore ('${rb}')" ;;
	*) log "capture: ${PHONE_B_MIXER_DL} restored to Off (read-back '${rb}')" ;;
	esac
	if [ "$rc" -ne 0 ]; then
		fail "capture: tinycap exited $rc on B"
		return 1
	fi

	wav_size="$(t21_dev_shell B "ls -l $(t21_q "$CAPTURE_REMOTE") 2>/dev/null | awk '{print \$5}'" 2>/dev/null | tr -d '\r')" || true
	if [ -z "$wav_size" ] || [ "${wav_size:-0}" -le 44 ]; then
		fail "capture: WAV $CAPTURE_REMOTE missing or has no PCM data (size '${wav_size:-0}')"
		return 1
	fi
	log "capture: WAV $CAPTURE_REMOTE size ${wav_size} bytes"

	if ! t21_dev_exec_out B cat "$CAPTURE_REMOTE" >"$host_wav" 2>/dev/null; then
		fail "capture: cannot read $CAPTURE_REMOTE off B for verification"
		return 1
	fi
	stats="$(t21_wav_stats "$host_wav" || true)"
	if [ -z "$stats" ]; then
		skip_check "capture: no sox/python3 on the host; non-silence check skipped (size ${wav_size} bytes)"
		return 0
	fi
	log "capture: stats $stats"
	peak="$(printf '%s' "$stats" | sed -nE 's/.*peak=([0-9]+).*/\1/p')"
	rms="$(printf '%s' "$stats" | sed -nE 's/.*rms=([0-9.]+).*/\1/p')"
	if [ -n "$peak" ] && [ "$peak" -le 8 ]; then
		fail "capture: WAV is digital silence (peak=${peak} rms=${rms:-0}) — mixer route toggled but no FL/UL audio reached the PCM"
		return 1
	fi
	log "capture: WAV is non-silent (peak=${peak:-?} rms=${rms:-?})"
	return 0
}

# t21_wav_stats FILE — "size=<bytes> rate=<hz> channels=<n> frames=<n>
# peak=<abs> rms=<x.y>" via sox stat, else a python3 (stdlib wave) RMS helper.
t21_wav_stats() {
	local file="$1"
	if command -v sox >/dev/null 2>&1; then
		local out peak rms peak16
		out="$(sox "$file" -n stat 2>&1)" || return 1
		peak="$(printf '%s' "$out" | sed -nE 's/^Maximum amplitude:[[:space:]]+([0-9.eE+-]+).*/\1/p' | head -1)"
		rms="$(printf '%s' "$out" | sed -nE 's/^RMS[[:space:]]+amplitude:[[:space:]]+([0-9.eE+-]+).*/\1/p' | head -1)"
		[ -n "$peak" ] || return 1
		peak16="$(awk -v p="$peak" 'BEGIN { printf "%.0f", p * 32768 }')"
		printf 'size=%s peak=%s rms=%s\n' "$(wc -c <"$file" | tr -d ' ')" "$peak16" "${rms:-?}"
		return 0
	fi
	command -v python3 >/dev/null 2>&1 || return 1
	python3 - "$file" <<'PY'
import sys, wave
try:
    w = wave.open(sys.argv[1], "rb")
    n = w.getnframes()
    frames = w.readframes(n)
except Exception:
    sys.exit(1)
import array
a = array.array("h")
a.frombytes(frames[: len(frames) - (len(frames) % 2)])
peak = max((abs(v) for v in a), default=0)
rms = (sum(v * v for v in a) / len(a)) ** 0.5 if len(a) else 0
print(f"size={len(frames) + 44} rate={w.getframerate()} channels={w.getnchannels()} frames={n} peak={peak} rms={rms:.1f}")
PY
}

# ---------------------------------------------------------------------------
# Hang-up / cleanup
# ---------------------------------------------------------------------------

# t21_hangup DEV — end any call on DEV (ENDCALL keyevent, then a UI tap of an
# End call control) and wait for idle.
t21_hangup() {
	local dev="$1" coords="" ui="$WORK/endcall-ui.xml"
	t21_dev_shell "$dev" 'input keyevent 6' >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5; do
		t21_dev_idle "$dev" && return 0
		sleep 1
	done
	if t21_ui_dump "$dev" "$ui"; then
		coords="$(t21_ui_find_tap "$ui" regex 'end call|hang up|disconnect' || true)"
	fi
	if [ -n "$coords" ]; then
		t21_ui_tap "$dev" "$coords"
		log "cleanup: tapped End call on $dev at ($coords)"
		for _ in 1 2 3 4 5; do
			t21_dev_idle "$dev" && return 0
			sleep 1
		done
	fi
	t21_dev_idle "$dev"
}

# t21_cleanup — end calls, mixers Off, remove temp files. Prints the kept
# locations when KEEP_TMP=1. Returns the number of issues found.
t21_cleanup() {
	[ "$CLEANUP_DONE" -eq 1 ] && return 0
	CLEANUP_DONE=1
	local issues=0 entry dev path

	if [ -n "$ROOT_MODE_A" ] && [ "$ROOT_MODE_A" != "none" ]; then
		t21_hangup A || { issues=$((issues + 1)); printf '[validate] cleanup: A still has an active call\n' >&2; }
	fi
	if [ -n "$ROOT_MODE_B" ] && [ "$ROOT_MODE_B" != "none" ]; then
		t21_hangup B || { issues=$((issues + 1)); printf '[validate] cleanup: B still has an active call\n' >&2; }
	fi

	if [ -n "$PHONE_B_MIXER_DL" ] && [ -n "$ROOT_MODE_B" ] && [ "$ROOT_MODE_B" != "none" ]; then
		local rb
		t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD set $(t21_q "$PHONE_B_MIXER_DL") 0" >/dev/null 2>&1 || true
		[ -n "$PHONE_B_MIXER_UL" ] && t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD set $(t21_q "$PHONE_B_MIXER_UL") 0" >/dev/null 2>&1 || true
		rb="$(t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD get $(t21_q "$PHONE_B_MIXER_DL")" 2>/dev/null | tr -d '\r')" || true
		case "$rb" in
		*On*) issues=$((issues + 1)); printf '[validate] cleanup: %s still On on B (%s)\n' "$PHONE_B_MIXER_DL" "$rb" >&2 ;;
		*) log "cleanup: B mixers Off (${PHONE_B_MIXER_DL} read-back '${rb}')" ;;
		esac
	fi

	local keep_dev=0
	[ "$KEEP_TMP" = "1" ] && keep_dev=1
	for entry in ${DEV_TMP_FILES[@]+"${DEV_TMP_FILES[@]}"}; do
		dev="${entry%%|*}"
		path="${entry#*|}"
		if [ "$dev" = "B" ] && [ "$path" = "$CAPTURE_REMOTE" ] && [ "$keep_dev" = "1" ]; then
			log "cleanup: kept device capture $path on B (KEEP_TMP=1)"
			continue
		fi
		t21_dev_shell "$dev" "rm -f $(t21_q "$path")" >/dev/null 2>&1 || true
	done
	if [ -n "$CAPTURE_REMOTE" ] && [ "$keep_dev" = "0" ]; then
		t21_dev_shell B "rm -f $(t21_q "$CAPTURE_REMOTE")" >/dev/null 2>&1 || true
	fi

	if [ -n "$ACCOUNT_RESTORE" ]; then
		# shellcheck disable=SC2086
		t21_dev_shell A "cmd telecom set-user-selected-outgoing-phone-account $ACCOUNT_RESTORE" >/dev/null 2>&1 || true
	fi

	if [ "$KEEP_TMP" = "1" ]; then
		log "cleanup: kept host work dir $WORK (KEEP_TMP=1)"
	else
		rm -rf "$WORK"
	fi
	return "$issues"
}

trap 't21_cleanup >/dev/null 2>&1 || true' EXIT

# ---------------------------------------------------------------------------
# 1. precheck
# ---------------------------------------------------------------------------

A_READY="$(t21_precheck_dev A "A ($PHONE_A_SERIAL)")"
B_READY="$(t21_precheck_dev B "B ($PHONE_B_SERIAL)")"

# t21_finish_skip REASON — print the stage summary for a skipped run and exit 0.
t21_finish_skip() {
	stage_line "precheck: SKIP ($1)"
	stage_line "dial: SKIP"
	stage_line "active: SKIP"
	stage_line "capture: SKIP"
	stage_line "evidence: SKIP"
	stage_line "cleanup: SKIP"
	log "t21_call_capture: summary"
	for line in "${STAGE_LINES[@]}"; do printf '[validate]   %s\n' "$line" >&2; done
	log "t21_call_capture: SKIP (skips: $SKIPS)"
	exit 0
}

if [ "$A_READY" != "yes" ] || [ "$B_READY" != "yes" ]; then
	t21_finish_skip "A=$A_READY B=$B_READY"
fi
t21_resolve_route "$PHONE_B_SERIAL"
if [ -z "$PHONE_B_MIXER_DL" ] || [ -z "$PHONE_B_PCM_DEVICE" ]; then
	skip_check "capture route unresolved for B ($PHONE_B_SERIAL): pass PHONE_B_MIXER_DL and PHONE_B_PCM_DEVICE"
	t21_finish_skip "capture route unresolved"
fi
if ! t21_root_shell B "$(t21_q "$E3_DIR/tinymix") -D $PHONE_B_PCM_CARD get $(t21_q "$PHONE_B_MIXER_DL")" >/dev/null 2>&1; then
	skip_check "B does not expose control '${PHONE_B_MIXER_DL}' on card $PHONE_B_PCM_CARD"
	t21_finish_skip "mixer control missing"
fi
if ! t21_dev_shell B "grep -qE '^$(printf '%02d' "$PHONE_B_PCM_CARD")-$(printf '%02d' "$PHONE_B_PCM_DEVICE"): .*capture' /proc/asound/pcm"; then
	skip_check "B exposes no capture PCM ${PHONE_B_PCM_CARD}-${PHONE_B_PCM_DEVICE} in /proc/asound/pcm"
	t21_finish_skip "capture PCM missing"
fi
ROUTE_UL_NOTE=""
[ -n "$PHONE_B_MIXER_UL" ] && ROUTE_UL_NOTE=" + '${PHONE_B_MIXER_UL}'"
log "precheck: B capture route '${PHONE_B_MIXER_DL}' (PCM ${PHONE_B_PCM_CARD}-${PHONE_B_PCM_DEVICE})${ROUTE_UL_NOTE}"
stage_line "precheck: PASS"

# ---------------------------------------------------------------------------
# 2. dial
# ---------------------------------------------------------------------------

ATTEMPT_START_MS="$(now_ms)"
ATTEMPT_START_S=$((ATTEMPT_START_MS / 1000))
if t21_dial; then
	stage_line "dial: PASS"
else
	stage_line "dial: FAIL"
fi

# ---------------------------------------------------------------------------
# 3. ACTIVE poll / 5. setup-failure evidence
# ---------------------------------------------------------------------------

ACTIVE=0
POLL_RC=2
if [ "$FAILURES" -eq 0 ]; then
	POLL_RC=0
	t21_wait_active || POLL_RC=$?
fi
if [ "$POLL_RC" -eq 0 ]; then
	ACTIVE=1
	stage_line "active: PASS"
	stage_line "evidence: SKIP (call connected, no setup failure)"
	log "active: PASS"
elif [ "$FAILURES" -ne 0 ]; then
	stage_line "active: SKIP"
	stage_line "evidence: SKIP (dial failed before the call poll)"
else
	stage_line "active: FAIL"
	until_s="$(date +%s)"
	t21_collect_failure_evidence "$ATTEMPT_START_S" "$until_s"
	if [ -n "$EV_A_REASON_LINE" ]; then
		parsed="$(t21_parse_ims_reason "$EV_A_REASON_LINE")"
		cause="${parsed%%|*}"
		rest="${parsed#*|}"
		code_name="${rest%%|*}"
		rest="${rest#*|}"
		sip="${rest%%|*}"
		msg="${rest#*|}"
		fail "call setup failed before ACTIVE: ImsReasonInfo {${cause} ${code_name}${sip:+, SIP ${sip}}${msg:+, \"${msg}\"}} — callee ${EV_B_RANG}"
	else
		fail "call A→B did not become ACTIVE within ${CALL_ACTIVE_TIMEOUT}s and no ImsReasonInfo was logged on A — callee ${EV_B_RANG}"
	fi
	stage_line "evidence: FAIL (carrier cause above)"
fi

# ---------------------------------------------------------------------------
# 4. capture
# ---------------------------------------------------------------------------

if [ "$ACTIVE" -eq 1 ]; then
	if t21_capture; then
		stage_line "capture: PASS"
		log "capture: PASS"
	else
		stage_line "capture: FAIL"
	fi
	t21_hangup A >/dev/null 2>&1 || true
	t21_hangup B >/dev/null 2>&1 || true
else
	if [ "$FAILURES" -ne 0 ]; then
		stage_line "capture: SKIP (call never became ACTIVE)"
	else
		stage_line "capture: SKIP"
	fi
fi

# ---------------------------------------------------------------------------
# 5. cleanup
# ---------------------------------------------------------------------------

CLEANUP_ISSUES=0
t21_cleanup || CLEANUP_ISSUES=$?
if [ "$CLEANUP_ISSUES" -eq 0 ]; then
	stage_line "cleanup: PASS"
	log "cleanup: PASS (no calls, mixers Off, temp files removed)"
else
	stage_line "cleanup: FAIL ($CLEANUP_ISSUES issue(s))"
	fail "cleanup: $CLEANUP_ISSUES issue(s) left behind (calls/mixers/temp files)"
fi
[ "$KEEP_TMP" = "1" ] && log "t21_call_capture: kept host work dir $WORK (KEEP_TMP=1)"

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

log "t21_call_capture: summary"
for line in "${STAGE_LINES[@]}"; do printf '[validate]   %s\n' "$line" >&2; done
if [ "$FAILURES" -eq 0 ]; then
	log "t21_call_capture: PASS (skips: $SKIPS)"
	exit 0
fi
log "t21_call_capture: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
