#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t20_cdn.sh — bridge-hosted encrypted file service ("CDN") contract matrix.
# See the project's docs/cdn-file-service.md for the API and the app contract.
#
# Everything runs against CDN_BASE_URL with CDN_TOKEN (a paired device's
# bearer token). Tokens are never printed. Without CDN_BASE_URL and CDN_TOKEN
# the whole live matrix SKIPs cleanly and the script exits 0.
#
# Matrix:
#   a. client-side AES-256-GCM encrypt -> authenticated upload -> anonymous
#      fetch -> decrypt, byte-exact, for a text file, a PNG and a PDF (the
#      python3 helper replicates the app contract; Node's WebCrypto re-checks
#      decryptability and type sniffing when node is available);
#   b. a wrong key must fail to decrypt the fetched ciphertext;
#   c. fragment-less wrapper URL: 200 + CSP (sandbox/nosniff), no plaintext,
#      no key, the id appears only as the data literal;
#   d. missing resource: 404 on wrapper and raw;
#   e. unauthenticated upload/list/revoke: 401 (plus a bad token);
#   f. expiry: override accepted, zero/negative/invalid/above-max rejected,
#      and a short-lived link 404s on read once expired (the hourly GC that
#      frees the bytes cannot be triggered over HTTP);
#   g. oversized upload: 413 when CDN_MAX_FILE_BYTES names a probeable cap;
#   h. per-user cap FIFO pruning when CDN_PER_USER_CAP_BYTES names a small cap;
#   i. revoke own file: 204, then 404 on wrapper/raw, second revoke 404;
#   j. cross-user isolation with CDN_TOKEN_2 (or a documented SKIP);
#   k. bridge discovery advertises cdn_base_url matching CDN_BASE_URL (SKIP
#      when the bridge info endpoint is not reachable).
#
# Env contract (see docs/validation.md):
#   CDN_BASE_URL             live base URL, e.g. https://cdn.example.com
#   CDN_TOKEN                uploader's paired-device bearer token (never logged)
#   CDN_TOKEN_2              second user's token for the isolation check
#   CDN_EXPIRES              expiry for the round-trip uploads (default 12h;
#                            must not exceed the bridge's cdn.max_expiry)
#   CDN_SHORT_EXPIRES        short expiry proven end-to-end (default 60s)
#   CDN_EXPIRY_WAIT          extra seconds to wait past CDN_SHORT_EXPIRES (default 5)
#   CDN_MAX_FILE_BYTES       configured cdn.max_file_bytes; enables the 413 probe
#   CDN_PER_USER_CAP_BYTES   configured cdn.per_user_cap_bytes; enables FIFO
#   CDN_MAX_UPLOAD_BYTES     probe guard (default 8388608); larger caps SKIP
#   CDN_CHECK_DISCOVERY      1 (default) checks cdn_base_url in bridge info
#   CDN_INSECURE             1 = curl -k for a self-signed staging deployment
#   CDN_NODE_TLS_INSECURE    1 = Node accepts self-signed certs too (staging)
#   CDN_PYTHON / CDN_NODE    interpreter overrides (default python3 / node)
#
# Evidence rule: ids, sizes, timestamps and hashes only — never tokens, keys
# or plaintext.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t20.XXXXXX")"
CDN_HEADERS="$WORK/last.headers"
CDN_BODY="$WORK/last.body"
: >"$WORK/uploads.txt"

cdn_curl() { # METHOD PATH [TOKEN] [DATA_FILE] [CONTENT_TYPE]
	local method="$1" path="$2" token="${3:-}" data_file="${4:-}" content_type="${5:-}"
	local url rc=0
	url="$CDN_BASE_URL$path"
	: >"$CDN_HEADERS"
	: >"$CDN_BODY"
	local -a args=(
		--silent --show-error --max-time "$CURL_TIMEOUT"
		--request "$method" --dump-header "$CDN_HEADERS" --output "$CDN_BODY" --write-out '%{http_code}'
	)
	args+=(${CURL_EXTRA[@]+"${CURL_EXTRA[@]}"})
	if [ -n "$token" ]; then
		args+=(-H "Authorization: Bearer $token")
	fi
	if [ -n "$content_type" ]; then
		args+=(-H "Content-Type: $content_type")
	fi
	if [ -n "$data_file" ]; then
		args+=(--data-binary "@$data_file")
	fi
	CDN_STATUS="$(curl "${args[@]}" "$url")" || rc=$?
	if [ "$rc" -ne 0 ]; then
		return "$RC_UNREACHABLE"
	fi
	return 0
}

cdn_header() { # NAME — response header value (case-insensitive, last match)
	local name="$1" line
	line="$(grep -i -m1 "^${name}:" "$CDN_HEADERS" 2>/dev/null | tr -d '\r' || true)"
	printf '%s\n' "${line#*: }"
}

# cdn_cleanup revokes every file this run uploaded (best effort) so a live
# matrix run leaves no test ciphertext behind.
cdn_cleanup() {
	local id
	if [ -n "${CDN_TOKEN:-}" ] && [ -s "$WORK/uploads.txt" ]; then
		while IFS= read -r id; do
			[ -n "$id" ] || continue
			cdn_curl DELETE "/f/$id" "$CDN_TOKEN" >/dev/null 2>&1 || true
		done <"$WORK/uploads.txt"
	fi
	rm -rf "$WORK"
}
trap cdn_cleanup EXIT

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CDN_BASE_URL="${CDN_BASE_URL:-}"
while [ "${CDN_BASE_URL%/}" != "$CDN_BASE_URL" ]; do
	CDN_BASE_URL="${CDN_BASE_URL%/}"
done
CDN_TOKEN="${CDN_TOKEN:-}"
CDN_TOKEN_2="${CDN_TOKEN_2:-}"
CDN_EXPIRES="${CDN_EXPIRES:-12h}"
CDN_SHORT_EXPIRES="${CDN_SHORT_EXPIRES:-60s}"
CDN_EXPIRY_WAIT="${CDN_EXPIRY_WAIT:-5}"
CDN_MAX_FILE_BYTES="${CDN_MAX_FILE_BYTES:-}"
CDN_PER_USER_CAP_BYTES="${CDN_PER_USER_CAP_BYTES:-}"
CDN_MAX_UPLOAD_BYTES="${CDN_MAX_UPLOAD_BYTES:-8388608}"
CDN_CHECK_DISCOVERY="${CDN_CHECK_DISCOVERY:-1}"
CDN_INSECURE="${CDN_INSECURE:-0}"
CDN_NODE_TLS_INSECURE="${CDN_NODE_TLS_INSECURE:-0}"
CDN_PYTHON="${CDN_PYTHON:-python3}"
CDN_NODE="${CDN_NODE:-node}"
CDN_CRYPT_HELPER="$SCRIPT_DIR/cdn_crypt.py"
CDN_NODE_HELPER="$SCRIPT_DIR/cdn_node_check.mjs"

CURL_EXTRA=()
if [ "$CDN_INSECURE" = "1" ]; then
	CURL_EXTRA=(-k)
fi

CDN_LAST_ID=""
CDN_LAST_KEY=""
CDN_LAST_SIZE=""
CDN_LAST_EXPIRES_MS=""
NODE_OK=0

log "t20_cdn: base=${CDN_BASE_URL:-<unset>} token=$([ -n "$CDN_TOKEN" ] && echo set || echo unset) second_token=$([ -n "$CDN_TOKEN_2" ] && echo set || echo unset) expires=$CDN_EXPIRES short=$CDN_SHORT_EXPIRES"

# ---------------------------------------------------------------------------
# 0. fixtures (local; always checked)
# ---------------------------------------------------------------------------
TEXT_FIXTURE="$WORK/text.txt"
TEXT_MARKER="cdn-text-marker-$(now_ms)"
{
	printf 'cdn text fixture %s\n' "$TEXT_MARKER"
	printf 'the quick brown fox jumps over the lazy dog\n'
	i=1
	while [ "$i" -le 40 ]; do
		printf 'line %02d of the bridge CDN validation text fixture\n' "$i"
		i=$((i + 1))
	done
} >"$TEXT_FIXTURE"
PNG_FIXTURE="$FIXTURES_DIR/tiny.png"
PDF_FIXTURE="$FIXTURES_DIR/tiny.pdf"
MP4_FIXTURE="$FIXTURES_DIR/tiny.mp4"
for spec in "$TEXT_FIXTURE:text" "$PNG_FIXTURE:png" "$PDF_FIXTURE:pdf" "$MP4_FIXTURE:mp4"; do
	file="${spec%:*}"
	[ -f "$file" ] || continue
	pass "fixture $(basename "$file"): $(stat --format=%s "$file")B sha256=$(sha256_16 "$file")…"
done
[ -f "$PNG_FIXTURE" ] || fail "fixture $PNG_FIXTURE is missing (run scripts/validate/fixtures/make-cdn-fixtures.sh)"
[ -f "$PDF_FIXTURE" ] || fail "fixture $PDF_FIXTURE is missing (run scripts/validate/fixtures/make-cdn-fixtures.sh)"
if [ "$FAILURES" -ne 0 ]; then
	log "t20_cdn: FAIL ($FAILURES fixture problem(s))"
	exit 1
fi

# ---------------------------------------------------------------------------
# 1. preconditions
# ---------------------------------------------------------------------------
if [ -z "$CDN_BASE_URL" ] || [ -z "$CDN_TOKEN" ]; then
	log "t20_cdn: CDN_BASE_URL/CDN_TOKEN not set — live CDN matrix skipped"
	for label in \
		"round-trip text file (encrypt/upload/fetch/decrypt)" \
		"round-trip binary files (PNG/PDF)" \
		"wrong-key decryption must fail" \
		"wrapper page: CSP/nosniff, no plaintext, id only as data" \
		"missing resource: 404" \
		"unauthenticated upload/list/revoke: 401" \
		"expiry override + invalid/past rejection + post-expiry 404" \
		"size cap: 413 above cdn.max_file_bytes" \
		"per-user cap FIFO pruning" \
		"revoke own file: 204 then 404" \
		"cross-user isolation" \
		"bridge discovery advertises cdn_base_url"
	do
		skip_check "$label (set CDN_BASE_URL + CDN_TOKEN)"
	done
	log "t20_cdn: SKIP-ONLY run (skips: $SKIPS)"
	exit 0
fi

case "$CDN_BASE_URL" in
http://*|https://*) ;;
*)
	fail "CDN_BASE_URL must include the scheme (got a value without http:// or https://)"
	log "t20_cdn: FAIL (bad CDN_BASE_URL)"
	exit 1
	;;
esac

command -v curl >/dev/null 2>&1 || { fail "curl is required"; log "t20_cdn: FAIL"; exit 1; }
command -v jq >/dev/null 2>&1 || { fail "jq is required"; log "t20_cdn: FAIL"; exit 1; }
if ! command -v "$CDN_PYTHON" >/dev/null 2>&1; then
	fail "python3 ($CDN_PYTHON) is required for the app-contract crypto helper"
	log "t20_cdn: FAIL"
	exit 1
fi
[ -f "$CDN_CRYPT_HELPER" ] || { fail "helper $CDN_CRYPT_HELPER is missing"; log "t20_cdn: FAIL"; exit 1; }
if command -v "$CDN_NODE" >/dev/null 2>&1 && "$CDN_NODE" --version >/dev/null 2>&1; then
	NODE_OK=1
	log "node $("$CDN_NODE" --version) available: WebCrypto re-check enabled"
else
	skip_check "node WebCrypto re-check disabled ($CDN_NODE not found) — structural wrapper checks still run"
fi

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
cdn_expect_status() { # LABEL WANT — records PASS/FAIL, never aborts the run
	if [ "$CDN_STATUS" = "$2" ]; then
		pass "$1: HTTP $CDN_STATUS"
	else
		fail "$1: HTTP $CDN_STATUS (want $2)"
	fi
	return 0
}

cdn_upload_file() { # LABEL BODY_FILE [EXPIRES] [TOKEN]
	local label="$1" body="$2" expires="${3:-$CDN_EXPIRES}" token="${4:-$CDN_TOKEN}" query="" rc=0
	CDN_LAST_ID=""
	CDN_LAST_SIZE=""
	CDN_LAST_EXPIRES_MS=""
	[ -n "$expires" ] && query="?expires=$expires"
	cdn_curl POST "/f$query" "$token" "$body" "application/octet-stream" || rc=$?
	if [ "$rc" -ne 0 ]; then
		fail "$label: upload transport error to $CDN_BASE_URL"
		return 1
	fi
	if [ "$CDN_STATUS" != "201" ]; then
		fail "$label: upload HTTP $CDN_STATUS (want 201, error=$(jq -r '.error // "?"' "$CDN_BODY" 2>/dev/null || true))"
		return 1
	fi
	CDN_LAST_ID="$(jq -r '.id // empty' "$CDN_BODY" 2>/dev/null || true)"
	CDN_LAST_SIZE="$(jq -r '.size // empty' "$CDN_BODY" 2>/dev/null || true)"
	CDN_LAST_EXPIRES_MS="$(jq -r '.expires_at_ms // empty' "$CDN_BODY" 2>/dev/null || true)"
	if ! [[ "$CDN_LAST_ID" =~ ^[A-Za-z0-9_-]{43}$ ]]; then
		fail "$label: upload returned an id that is not 43 base64url characters"
		return 1
	fi
	if [ "$CDN_LAST_SIZE" != "$(stat --format=%s "$body")" ]; then
		fail "$label: upload reported size $CDN_LAST_SIZE, sent $(stat --format=%s "$body")B"
		return 1
	fi
	printf '%s\n' "$CDN_LAST_ID" >>"$WORK/uploads.txt"
	return 0
}

cdn_fetch_raw() { # LABEL ID — anonymous ciphertext fetch; leaves it in $CDN_BODY
	local label="$1" id="$2" rc=0
	cdn_curl GET "/f/$id/raw" "" || rc=$?
	if [ "$rc" -ne 0 ]; then
		fail "$label: raw fetch transport error"
		return 1
	fi
	return 0
}

cdn_check_wrapper() { # LABEL ID KEY PLAINTEXT_FILE [PLAINTEXT_MARKER]
	local label="$1" id="$2" key="$3" plain="$4" rc=0 csp nosniff count
	cdn_curl GET "/f/$id" "" || rc=$?
	if [ "$rc" -ne 0 ]; then
		fail "$label: wrapper fetch transport error"
		return 1
	fi
	cdn_expect_status "$label wrapper (no fragment)" 200 || return 1
	csp="$(cdn_header "Content-Security-Policy")"
	nosniff="$(cdn_header "X-Content-Type-Options")"
	case "$csp" in
	*"default-src 'none'"*"sandbox allow-scripts"*|*"sandbox allow-scripts"*"default-src 'none'"*)
		pass "$label wrapper: CSP locks to default-src 'none' + sandbox allow-scripts"
		;;
	*)
		fail "$label wrapper: CSP missing default-src 'none'/'sandbox allow-scripts' (got: ${csp:0:120})"
		;;
	esac
	if [ "$nosniff" = "nosniff" ]; then
		pass "$label wrapper: X-Content-Type-Options: nosniff"
	else
		fail "$label wrapper: X-Content-Type-Options='$nosniff' (want nosniff)"
	fi
	if grep -qF "const fileID = \"$id\";" "$CDN_BODY"; then
		pass "$label wrapper: id embedded as the data literal"
	else
		fail "$label wrapper: id is not embedded as const fileID data"
	fi
	count="$(grep -o -F "$id" "$CDN_BODY" | wc -l | tr -d ' ')"
	if [ "$count" = "1" ]; then
		pass "$label wrapper: id appears exactly once (data only, never as markup)"
	else
		fail "$label wrapper: id appears $count times in the page (want exactly the 1 data literal)"
	fi
	if [ -n "$key" ] && grep -qF "$key" "$CDN_BODY"; then
		fail "$label wrapper: decryption key leaked into the page"
	elif [ -n "$key" ]; then
		pass "$label wrapper: decryption key absent from the page"
	fi
	if "$CDN_PYTHON" - "$CDN_BODY" "$plain" <<'PY'
import sys

page = open(sys.argv[1], "rb").read()
plain = open(sys.argv[2], "rb").read()
raise SystemExit(0 if plain not in page else 1)
PY
	then
		pass "$label wrapper: no plaintext bytes in the page"
	else
		fail "$label wrapper: plaintext bytes leaked into the page"
	fi
	if [ -n "${5:-}" ]; then
		if grep -qF "$5" "$CDN_BODY"; then
			fail "$label wrapper: plaintext marker leaked into the page"
		fi
	fi
	return 0
}

cdn_node_verify() { # LABEL RAW_URL KEY SHA SIZE SNIFF
	local label="$1" raw="$2" key="$3" sha="$4" size="$5" sniff="${6:-}"
	local json rc=0
	if [ "$CDN_NODE_TLS_INSECURE" = "1" ]; then
		json="$(NODE_TLS_REJECT_UNAUTHORIZED=0 "$CDN_NODE" "$CDN_NODE_HELPER" "$raw" "$key" "$sha" "$size" "$sniff" 2>/dev/null)" || rc=$?
	else
		json="$("$CDN_NODE" "$CDN_NODE_HELPER" "$raw" "$key" "$sha" "$size" "$sniff" 2>/dev/null)" || rc=$?
	fi
	if [ "$rc" -ne 0 ] || [ "$(jq -r '.ok // false' <<<"$json" 2>/dev/null || echo false)" != "true" ]; then
		fail "$label: node WebCrypto decrypt failed"
		return 1
	fi
	local observed_sniff
	observed_sniff="$(jq -r 'if (.sniff // "") == "" then "none" else .sniff end' <<<"$json")"
	pass "$label: node WebCrypto decrypt matches ($(jq -r '.sha256[0:16]' <<<"$json")…, size $(jq -r '.size' <<<"$json")B, sniff ${observed_sniff})"
	return 0
}

# cdn_roundtrip LABEL PLAINTEXT_FILE EXPECTED_SNIFF [PLAINTEXT_MARKER]
cdn_roundtrip() {
	local label="$1" plain="$2" sniff="${3:-}" marker="${4:-}"
	local body="$WORK/$label.body" enc_json rc=0 raw_copy="$WORK/$label.raw" dec_copy="$WORK/$label.dec"
	CDN_LAST_ID=""
	CDN_LAST_KEY=""
	enc_json="$("$CDN_PYTHON" "$CDN_CRYPT_HELPER" encrypt "$plain" "$body")" || {
		fail "$label: local encryption helper failed"
		return 1
	}
	CDN_LAST_KEY="$(jq -r '.key' <<<"$enc_json")"
	local expected_sha
	expected_sha="$(jq -r '.plaintext_sha256' <<<"$enc_json")"
	local cipher_size
	cipher_size="$(stat --format=%s "$body")"
	if [ -n "$CDN_MAX_FILE_BYTES" ] && [ "$CDN_MAX_FILE_BYTES" -gt 0 ] && [ "$cipher_size" -gt "$CDN_MAX_FILE_BYTES" ]; then
		skip_check "$label: ciphertext ${cipher_size}B exceeds the probeable cdn.max_file_bytes=$CDN_MAX_FILE_BYTES; round-trip skipped for this fixture"
		return 0
	fi
	pass "$label: encrypted locally (${cipher_size}B ciphertext = nonce||ct||tag, 32B key kept in the fragment)"
	cdn_upload_file "$label upload" "$body" || return 1
	local id="$CDN_LAST_ID"
	pass "$label upload: id $id, size $CDN_LAST_SIZE, expires_at_ms $CDN_LAST_EXPIRES_MS"

	cdn_fetch_raw "$label" "$id" || return 1
	cdn_expect_status "$label raw (anonymous, no fragment needed)" 200 || return 1
	cp "$CDN_BODY" "$raw_copy"
	if [ "$(sha256_file "$raw_copy")" = "$(sha256_file "$body")" ]; then
		pass "$label raw: fetched ciphertext is byte-identical ($(sha256_16 "$raw_copy")…)"
	else
		fail "$label raw: fetched ciphertext differs from the uploaded body"
	fi
	local content_type nosniff disposition cors
	content_type="$(cdn_header "Content-Type")"
	nosniff="$(cdn_header "X-Content-Type-Options")"
	disposition="$(cdn_header "Content-Disposition")"
	cors="$(cdn_header "Access-Control-Allow-Origin")"
	if [ "$content_type" = "application/octet-stream" ] && [ "$nosniff" = "nosniff" ]; then
		pass "$label raw: octet-stream + nosniff"
	else
		fail "$label raw: Content-Type='$content_type' nosniff='$nosniff' (want octet-stream + nosniff)"
	fi
	case "$disposition" in
	attachment*) pass "$label raw: attachment disposition" ;;
	*) fail "$label raw: Content-Disposition='$disposition' (want attachment)" ;;
	esac
	if [ "$cors" = "*" ]; then
		pass "$label raw: CORS * for the sandboxed wrapper"
	else
		fail "$label raw: Access-Control-Allow-Origin='$cors' (want *)"
	fi

	"$CDN_PYTHON" "$CDN_CRYPT_HELPER" decrypt "$raw_copy" "$CDN_LAST_KEY" "$dec_copy" >/dev/null || {
		fail "$label: decryption of the fetched ciphertext failed"
		return 1
	}
	if sha256_compare "$expected_sha" "$dec_copy" && cmp -s "$plain" "$dec_copy"; then
		pass "$label: decrypt round-trip is byte-exact ($(stat --format=%s "$plain")B)"
	else
		fail "$label: decrypted bytes differ from the plaintext"
	fi

	if [ "$NODE_OK" = "1" ]; then
		cdn_node_verify "$label" "$CDN_BASE_URL/f/$id/raw" "$CDN_LAST_KEY" "$expected_sha" "$(stat --format=%s "$plain")" "$sniff" || true
	fi

	cdn_check_wrapper "$label" "$id" "$CDN_LAST_KEY" "$plain" "$marker" || true

	return 0
}

# cdn_revoke_checked LABEL ID — the owner revokes; link dies immediately.
cdn_revoke_checked() {
	local label="$1" id="$2" rc=0
	cdn_curl DELETE "/f/$id" "$CDN_TOKEN" || rc=$?
	if [ "$rc" -ne 0 ]; then
		fail "$label: revoke transport error"
		return 1
	fi
	cdn_expect_status "$label revoke" 204 || return 1
	cdn_fetch_raw "$label revoke" "$id" || return 1
	cdn_expect_status "$label raw after revoke" 404 || return 1
	cdn_curl GET "/f/$id" "" || rc=$?
	cdn_expect_status "$label wrapper after revoke" 404 || return 1
	pass "$label: revoked link is dead (raw + wrapper 404)"
	return 0
}

# duration_seconds VALUE — "60s"/"12h"/"7d" to seconds (0 on unknown shape).
duration_seconds() {
	local value="$1" number
	case "$value" in
	*s) number="${value%s}"; case "$number" in ''|*[!0-9]*) return 1 ;; esac; printf '%s\n' "$number" ;;
	*m) number="${value%m}"; case "$number" in ''|*[!0-9]*) return 1 ;; esac; printf '%s\n' "$((number * 60))" ;;
	*h) number="${value%h}"; case "$number" in ''|*[!0-9]*) return 1 ;; esac; printf '%s\n' "$((number * 3600))" ;;
	*d) number="${value%d}"; case "$number" in ''|*[!0-9]*) return 1 ;; esac; printf '%s\n' "$((number * 86400))" ;;
	*) return 1 ;;
	esac
}

# ---------------------------------------------------------------------------
# 2. preflight: base reachable, token accepted, list contract
# ---------------------------------------------------------------------------
rc=0
cdn_curl GET "/f" "$CDN_TOKEN" || rc=$?
if [ "$rc" -ne 0 ]; then
	fail "preflight: $CDN_BASE_URL is not reachable"
	log "t20_cdn: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
	exit 1
fi
if [ "$CDN_STATUS" = "401" ]; then
	fail "preflight: the CDN rejected CDN_TOKEN (HTTP 401) — check the paired-device token"
	log "t20_cdn: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
	exit 1
fi
if [ "$CDN_STATUS" != "200" ]; then
	fail "preflight: GET /f returned HTTP $CDN_STATUS (want 200)"
	log "t20_cdn: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
	exit 1
fi
CDN_CAP_DISCOVERED="$(jq -r '.cap_bytes // empty' "$CDN_BODY" 2>/dev/null || true)"
CDN_USED_DISCOVERED="$(jq -r '.used_bytes // empty' "$CDN_BODY" 2>/dev/null || true)"
pass "preflight: $CDN_BASE_URL reachable, token accepted (cap_bytes=${CDN_CAP_DISCOVERED:-?}, used_bytes=${CDN_USED_DISCOVERED:-?})"

# ---------------------------------------------------------------------------
# 3. (a)(c)(k) round trips: text, PNG, PDF
# ---------------------------------------------------------------------------
cdn_roundtrip "text" "$TEXT_FIXTURE" "" "$TEXT_MARKER" || true
TEXT_ID="$CDN_LAST_ID"

# (b) wrong key must not decrypt the fetched ciphertext.
if [ -n "$TEXT_ID" ]; then
	WRONG_KEY="$("$CDN_PYTHON" "$CDN_CRYPT_HELPER" random-key)"
	rc=0
	cdn_fetch_raw "wrong key" "$TEXT_ID" || rc=$?
	if [ "$rc" -eq 0 ]; then
		cdn_expect_status "wrong key: ciphertext fetch" 200 || true
		wrong_out="$WORK/wrong.dec"
		rm -f "$wrong_out"
		rc=0
		"$CDN_PYTHON" "$CDN_CRYPT_HELPER" decrypt "$CDN_BODY" "$WRONG_KEY" "$wrong_out" >/dev/null 2>&1 || rc=$?
		if [ "$rc" -eq 3 ] && [ ! -e "$wrong_out" ]; then
			pass "wrong key: decryption fails (ciphertext undecryptable with a foreign key)"
		elif [ "$rc" -eq 0 ]; then
			fail "wrong key: decryption SUCCEEDED with a foreign key"
		else
			fail "wrong key: helper exited $rc (want 3, decryption failure)"
		fi
	fi
	cdn_revoke_checked "text" "$TEXT_ID" || true
fi

cdn_roundtrip "png" "$PNG_FIXTURE" "image/png" || true
PNG_ID="$CDN_LAST_ID"
if [ -n "$PNG_ID" ]; then
	cdn_revoke_checked "png" "$PNG_ID" || true
fi

cdn_roundtrip "pdf" "$PDF_FIXTURE" "application/pdf" || true
PDF_ID="$CDN_LAST_ID"
if [ -n "$PDF_ID" ]; then
	cdn_revoke_checked "pdf" "$PDF_ID" || true
fi

# ---------------------------------------------------------------------------
# 4. (d) missing resource and hostile id shapes: 404, never reflected
# ---------------------------------------------------------------------------
MISSING_ID="$("$CDN_PYTHON" "$CDN_CRYPT_HELPER" random-key)"
rc=0
cdn_curl GET "/f/$MISSING_ID" "" || rc=$?
[ "$rc" -ne 0 ] && fail "missing resource: transport error"
[ "$rc" -eq 0 ] && cdn_expect_status "missing id wrapper" 404
rc=0
cdn_curl GET "/f/$MISSING_ID/raw" "" || rc=$?
[ "$rc" -ne 0 ] && fail "missing resource: transport error"
[ "$rc" -eq 0 ] && cdn_expect_status "missing id raw" 404
for hostile in "..%2F..%2Fetc%2Fpasswd" "not-an-id!"; do
	rc=0
	cdn_curl GET "/f/$hostile" "" || rc=$?
	[ "$rc" -ne 0 ] && fail "hostile id '$hostile': transport error"
	[ "$rc" -eq 0 ] && cdn_expect_status "hostile id '$hostile'" 404
	if [ "$rc" -eq 0 ] && grep -qE "passwd|<script" "$CDN_BODY"; then
		fail "hostile id '$hostile' was reflected into the response"
	fi
done

# ---------------------------------------------------------------------------
# 5. (e) unauthenticated upload/list/revoke: 401
# ---------------------------------------------------------------------------
head -c 64 /dev/urandom >"$WORK/probe.bin"
rc=0
cdn_curl POST "/f" "" "$WORK/probe.bin" "application/octet-stream" || rc=$?
if [ "$rc" -eq 0 ]; then
	cdn_expect_status "unauthenticated upload" 401 || true
	if [ "$(jq -r '.error // empty' "$CDN_BODY" 2>/dev/null || true)" = "unauthorized" ]; then
		pass "unauthenticated upload: unauthorized error envelope"
	else
		fail "unauthenticated upload: missing unauthorized error code"
	fi
fi
rc=0
cdn_curl POST "/f" "validate-bad-token" "$WORK/probe.bin" "application/octet-stream" || rc=$?
[ "$rc" -eq 0 ] && cdn_expect_status "bad-token upload" 401
rc=0
cdn_curl GET "/f" "" || rc=$?
[ "$rc" -eq 0 ] && cdn_expect_status "unauthenticated list" 401
rc=0
cdn_curl DELETE "/f/$MISSING_ID" "" || rc=$?
[ "$rc" -eq 0 ] && cdn_expect_status "unauthenticated revoke" 401

# ---------------------------------------------------------------------------
# 6. (f) expiry overrides
# ---------------------------------------------------------------------------
head -c 128 /dev/urandom >"$WORK/expiry.bin"
if cdn_upload_file "short-expiry upload" "$WORK/expiry.bin" "$CDN_SHORT_EXPIRES"; then
	SHORT_ID="$CDN_LAST_ID"
	SHORT_EXPIRES_MS="$CDN_LAST_EXPIRES_MS"
	SHORT_SECONDS="$(duration_seconds "$CDN_SHORT_EXPIRES" 2>/dev/null || true)"
	if [ -z "$SHORT_SECONDS" ]; then
		fail "CDN_SHORT_EXPIRES='$CDN_SHORT_EXPIRES' is not a s/m/h/d duration (e.g. 60s, 5m, 1h)"
		SHORT_SECONDS=0
	fi
	NOW="$(now_ms)"
	if [ "$SHORT_SECONDS" -gt 0 ] &&
		[ "$SHORT_EXPIRES_MS" -ge "$((NOW + SHORT_SECONDS * 1000 / 2))" ] &&
		[ "$SHORT_EXPIRES_MS" -le "$((NOW + SHORT_SECONDS * 1000 + 15000))" ]; then
		pass "expiry override ?expires=$CDN_SHORT_EXPIRES accepted (expires_at_ms=$SHORT_EXPIRES_MS matches now+${SHORT_SECONDS}s)"
	else
		fail "expiry override ?expires=$CDN_SHORT_EXPIRES: expires_at_ms=$SHORT_EXPIRES_MS does not match now+${SHORT_SECONDS}s"
	fi
	for bad in "0" "bogus" "-5m" "400d"; do
		rc=0
		cdn_curl POST "/f?expires=$bad" "$CDN_TOKEN" "$WORK/probe.bin" "application/octet-stream" || rc=$?
		if [ "$rc" -ne 0 ]; then
			fail "invalid expiry '$bad': transport error"
			continue
		fi
		cdn_expect_status "invalid expiry '$bad'" 400 || continue
		if [ "$(jq -r '.error // empty' "$CDN_BODY" 2>/dev/null || true)" = "bad_request" ]; then
			pass "invalid expiry '$bad': bad_request envelope"
		else
			fail "invalid expiry '$bad': missing bad_request error code"
		fi
	done
	wait_for=$((SHORT_SECONDS + CDN_EXPIRY_WAIT))
	log "expiry: waiting ${wait_for}s for ?expires=$CDN_SHORT_EXPIRES to lapse"
	sleep "$wait_for"
	rc=0
	cdn_fetch_raw "expired link" "$SHORT_ID" || rc=$?
	[ "$rc" -eq 0 ] && cdn_expect_status "expired link raw" 404
	rc=0
	cdn_curl GET "/f/$SHORT_ID" "" || rc=$?
	[ "$rc" -eq 0 ] && cdn_expect_status "expired link wrapper" 404
	pass "expiry: read path treats lapsed links as gone (note: the hourly cdn GC frees the bytes later; gc runs are admin-only)"
fi

# ---------------------------------------------------------------------------
# 7. (g) size cap: above cdn.max_file_bytes -> 413
# ---------------------------------------------------------------------------
if [ -z "$CDN_MAX_FILE_BYTES" ]; then
	skip_check "size-cap 413 probe disabled: rerun with CDN_MAX_FILE_BYTES=<configured cdn.max_file_bytes> against a staging deploy with a small cap (the live default is 100 MiB, unpractical to probe over the network)"
else
	require_int "CDN_MAX_FILE_BYTES" "$CDN_MAX_FILE_BYTES"
	if [ "$CDN_MAX_FILE_BYTES" -gt "$CDN_MAX_UPLOAD_BYTES" ]; then
		skip_check "size-cap 413 probe skipped: cap $CDN_MAX_FILE_BYTES exceeds the CDN_MAX_UPLOAD_BYTES guard ($CDN_MAX_UPLOAD_BYTES); use a small staging cap"
	elif [ "$CDN_MAX_FILE_BYTES" -lt 32 ]; then
		skip_check "size-cap 413 probe skipped: cap $CDN_MAX_FILE_BYTES is too small to carry a GCM body"
	else
		head -c "$((CDN_MAX_FILE_BYTES + 1))" /dev/urandom >"$WORK/oversized.bin"
		rc=0
		cdn_curl POST "/f?expires=$CDN_EXPIRES" "$CDN_TOKEN" "$WORK/oversized.bin" "application/octet-stream" || rc=$?
		if [ "$rc" -ne 0 ]; then
			fail "size cap: oversized upload transport error"
		else
			cdn_expect_status "oversized upload ($((CDN_MAX_FILE_BYTES + 1))B > cap $CDN_MAX_FILE_BYTES)" 413 || true
			if [ "$(jq -r '.error // empty' "$CDN_BODY" 2>/dev/null || true)" = "file_too_large" ]; then
				pass "size cap: file_too_large error envelope"
			else
				fail "size cap: missing file_too_large error code"
			fi
			head -c "$CDN_MAX_FILE_BYTES" /dev/urandom >"$WORK/at-cap.bin"
			if cdn_upload_file "at-cap upload ($CDN_MAX_FILE_BYTES B)" "$WORK/at-cap.bin"; then
				pass "size cap: at-limit upload accepted"
				cdn_revoke_checked "at-cap" "$CDN_LAST_ID" || true
			fi
		fi
	fi
fi

# ---------------------------------------------------------------------------
# 8. (h) per-user cap + FIFO pruning
# ---------------------------------------------------------------------------
if [ -z "$CDN_PER_USER_CAP_BYTES" ]; then
	skip_check "FIFO quota probe disabled: observed cap_bytes=${CDN_CAP_DISCOVERED:-unknown}; rerun with CDN_PER_USER_CAP_BYTES=<configured cdn.per_user_cap_bytes> against a staging deploy with a small cap"
else
	require_int "CDN_PER_USER_CAP_BYTES" "$CDN_PER_USER_CAP_BYTES"
	if [ "$CDN_PER_USER_CAP_BYTES" -gt "$CDN_MAX_UPLOAD_BYTES" ]; then
		skip_check "FIFO quota probe skipped: cap $CDN_PER_USER_CAP_BYTES exceeds the CDN_MAX_UPLOAD_BYTES guard ($CDN_MAX_UPLOAD_BYTES); use a small staging cap"
	elif [ "$CDN_PER_USER_CAP_BYTES" -lt 64 ]; then
		skip_check "FIFO quota probe skipped: cap $CDN_PER_USER_CAP_BYTES is too small to probe meaningfully"
	else
		FIFO_SIZE=$((CDN_PER_USER_CAP_BYTES / 2 + 1))
		if [ -n "$CDN_MAX_FILE_BYTES" ] && [ "$FIFO_SIZE" -gt "$CDN_MAX_FILE_BYTES" ]; then
			skip_check "FIFO quota probe skipped: ${FIFO_SIZE}B per-file probes exceed cdn.max_file_bytes ($CDN_MAX_FILE_BYTES); lower the per-user cap relative to the file cap"
		else
			head -c "$FIFO_SIZE" /dev/urandom >"$WORK/fifo-a.bin"
			head -c "$FIFO_SIZE" /dev/urandom >"$WORK/fifo-b.bin"
			if cdn_upload_file "FIFO oldest file" "$WORK/fifo-a.bin"; then
				FIFO_A_ID="$CDN_LAST_ID"
				if cdn_upload_file "FIFO newest file" "$WORK/fifo-b.bin"; then
					FIFO_B_ID="$CDN_LAST_ID"
					rc=0
					cdn_fetch_raw "FIFO" "$FIFO_A_ID" || rc=$?
					[ "$rc" -eq 0 ] && cdn_expect_status "FIFO: oldest file pruned (silent 201 pruning)" 404
					rc=0
					cdn_fetch_raw "FIFO" "$FIFO_B_ID" || rc=$?
					[ "$rc" -eq 0 ] && cdn_expect_status "FIFO: newest file kept" 200
					rc=0
					cdn_curl GET "/f" "$CDN_TOKEN" || rc=$?
					if [ "$rc" -eq 0 ] && [ "$CDN_STATUS" = "200" ]; then
						kept="$(jq -r --arg id "$FIFO_B_ID" '[.files[].id] | index($id) != null' "$CDN_BODY")"
						pruned="$(jq -r --arg id "$FIFO_A_ID" '[.files[].id] | index($id) != null' "$CDN_BODY")"
						used="$(jq -r '.used_bytes // empty' "$CDN_BODY")"
						if [ "$kept" = "true" ] && [ "$pruned" = "false" ] && [ -n "$used" ] && [ "$used" -le "$CDN_PER_USER_CAP_BYTES" ]; then
							pass "FIFO: list shows the newest file only (used_bytes=$used <= cap=$CDN_PER_USER_CAP_BYTES)"
						else
							fail "FIFO: list state wrong (newest=$kept, oldest=$pruned, used=$used, cap=$CDN_PER_USER_CAP_BYTES)"
						fi
					else
						fail "FIFO: list after pruning returned HTTP $CDN_STATUS"
					fi
					cdn_revoke_checked "FIFO" "$FIFO_B_ID" || true
				fi
			fi
		fi
	fi
fi

# ---------------------------------------------------------------------------
# 9. (i) revoke own file: 204, then 404, second revoke 404
# ---------------------------------------------------------------------------
head -c 256 /dev/urandom >"$WORK/revoke.bin"
if cdn_upload_file "revoke probe upload" "$WORK/revoke.bin"; then
	REVOKE_ID="$CDN_LAST_ID"
	cdn_revoke_checked "revoke probe" "$REVOKE_ID" || true
	rc=0
	cdn_curl DELETE "/f/$REVOKE_ID" "$CDN_TOKEN" || rc=$?
	[ "$rc" -eq 0 ] && cdn_expect_status "second revoke of the same file" 404
	rc=0
	cdn_curl GET "/f" "$CDN_TOKEN" || rc=$?
	if [ "$rc" -eq 0 ] && [ "$CDN_STATUS" = "200" ]; then
		if [ "$(jq -r --arg id "$REVOKE_ID" '[.files[].id] | index($id) != null' "$CDN_BODY")" = "false" ]; then
			pass "revoke: list no longer offers the revoked id"
		else
			fail "revoke: revoked id is still in the list"
		fi
	fi
fi

# ---------------------------------------------------------------------------
# 10. (j) cross-user isolation
# ---------------------------------------------------------------------------
if [ -z "$CDN_TOKEN_2" ]; then
	skip_check "cross-user isolation disabled: set CDN_TOKEN_2 to a second paired device token (a different Matrix user) to enable it"
else
	rc=0
	cdn_curl GET "/f" "$CDN_TOKEN_2" || rc=$?
	if [ "$rc" -ne 0 ] || [ "$CDN_STATUS" = "401" ]; then
		skip_check "cross-user isolation skipped: the second token was rejected by the bridge (HTTP ${CDN_STATUS:-transport-error})"
	else
		head -c 256 /dev/urandom >"$WORK/foreign.bin"
		if cdn_upload_file "foreign-owner upload" "$WORK/foreign.bin" "$CDN_EXPIRES" "$CDN_TOKEN"; then
			FOREIGN_ID="$CDN_LAST_ID"
			rc=0
			cdn_curl GET "/f" "$CDN_TOKEN_2" || rc=$?
			if [ "$rc" -eq 0 ] && [ "$CDN_STATUS" = "200" ]; then
				if [ "$(jq -r --arg id "$FOREIGN_ID" '[.files[].id] | index($id) != null' "$CDN_BODY")" = "false" ]; then
					pass "cross-user: the second user's list does not reveal the id"
				else
					fail "cross-user: the second user's list reveals the first user's id"
				fi
			fi
			rc=0
			cdn_curl DELETE "/f/$FOREIGN_ID" "$CDN_TOKEN_2" || rc=$?
			[ "$rc" -eq 0 ] && cdn_expect_status "cross-user: second user cannot revoke (foreign ids look unknown)" 404
			rc=0
			cdn_fetch_raw "cross-user" "$FOREIGN_ID" || rc=$?
			[ "$rc" -eq 0 ] && cdn_expect_status "cross-user: file still fetchable after the foreign revoke attempt" 200
			cdn_revoke_checked "cross-user" "$FOREIGN_ID" || true
		fi
	fi
fi

# ---------------------------------------------------------------------------
# 11. (k) discovery: bridge info advertises cdn_base_url
# ---------------------------------------------------------------------------
if [ "$CDN_CHECK_DISCOVERY" != "1" ]; then
	skip_check "cdn_base_url discovery check disabled (CDN_CHECK_DISCOVERY=0)"
else
	rc=0
	info="$(bridge_info 2>/dev/null)" || rc=$?
	if [ "$rc" -ne 0 ] || [ -z "$info" ]; then
		skip_check "cdn_base_url discovery skipped: bridge info not reachable at $BRIDGE_URL (set BRIDGE_URL/BRIDGE_TOKEN to check)"
	else
		advertised="$(jq -r '.cdn_base_url // empty' <<<"$info")"
		if [ -z "$advertised" ]; then
			fail "discovery: bridge info has no cdn_base_url (the app will not offer URL uploads)"
		elif [ "${advertised%/}" = "$CDN_BASE_URL" ]; then
			pass "discovery: bridge advertises cdn_base_url=$advertised"
		else
			fail "discovery: bridge advertises cdn_base_url='$advertised', harness points at '$CDN_BASE_URL'"
		fi
	fi
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t20_cdn: PASS (skips: $SKIPS)"
	exit 0
fi
log "t20_cdn: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
