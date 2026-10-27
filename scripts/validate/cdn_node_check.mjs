// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// cdn_node_check.mjs — drive the wrapper page's WebCrypto steps in Node.
//
// The bridge CDN wrapper (connector/cdn.go) decrypts the fetched ciphertext
// with a fragment key using SubtleCrypto and sniffs the plaintext type from
// magic bytes. This helper performs the identical steps against a live URL so
// t20_cdn.sh can prove decryptability without a browser.
//
// Usage:
//   node cdn_node_check.mjs RAW_URL KEY_B64URL EXPECTED_SHA256 \
//        [EXPECTED_SIZE] [EXPECTED_SNIFF]
//
// KEY_B64URL is never printed. Prints JSON:
//   {"ok":true,"size":N,"sha256":"…","sniff":"video/mp4","first_bytes":"…"}
// and exits non-zero with {"ok":false,"error":"…"} on any failure.

import { createHash } from "node:crypto";

const [rawURL, keyText, expectedSHA, expectedSize = "", expectedSniff = ""] = process.argv.slice(2);

function die(message) {
	console.log(JSON.stringify({ ok: false, error: message }));
	process.exit(1);
}

if (!rawURL || !keyText || !expectedSHA) {
	die("usage: cdn_node_check.mjs RAW_URL KEY_B64URL EXPECTED_SHA256 [EXPECTED_SIZE] [EXPECTED_SNIFF]");
}

const webcrypto = globalThis.crypto ?? (await import("node:crypto")).webcrypto;

function decodeKey(text) {
	if (!/^[A-Za-z0-9_-]+$/.test(text)) throw new Error("invalid key characters");
	const bytes = Buffer.from(text, "base64url");
	if (bytes.length !== 32) throw new Error(`key is ${bytes.length} bytes, want 32`);
	return new Uint8Array(bytes);
}

function matchesAt(bytes, offset, text) {
	if (bytes.length < offset + text.length) return false;
	for (let i = 0; i < text.length; i++) if (bytes[offset + i] !== text.charCodeAt(i)) return false;
	return true;
}

// Mirror of sniffType() in connector/cdn.go — same order, same magics.
function sniffType(bytes) {
	if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return "image/jpeg";
	if (bytes.length >= 8 && bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e && bytes[3] === 0x47) return "image/png";
	if (matchesAt(bytes, 0, "GIF87a") || matchesAt(bytes, 0, "GIF89a")) return "image/gif";
	if (matchesAt(bytes, 0, "RIFF") && matchesAt(bytes, 8, "WEBP")) return "image/webp";
	if (matchesAt(bytes, 4, "ftyp") && (matchesAt(bytes, 8, "avif") || matchesAt(bytes, 8, "avis"))) return "image/avif";
	if (matchesAt(bytes, 0, "OggS")) return "audio/ogg";
	if (matchesAt(bytes, 0, "fLaC")) return "audio/flac";
	if (matchesAt(bytes, 0, "RIFF") && matchesAt(bytes, 8, "WAVE")) return "audio/wav";
	if (matchesAt(bytes, 0, "ID3")) return "audio/mpeg";
	if (bytes.length >= 2 && bytes[0] === 0xff && (bytes[1] & 0xe0) === 0xe0) return "audio/mpeg";
	if (matchesAt(bytes, 4, "ftyp") && matchesAt(bytes, 8, "M4A ")) return "audio/mp4";
	if (bytes.length >= 4 && bytes[0] === 0x1a && bytes[1] === 0x45 && bytes[2] === 0xdf && bytes[3] === 0xa3) return "video/webm";
	if (matchesAt(bytes, 0, "%PDF-")) return "application/pdf";
	if (matchesAt(bytes, 4, "ftyp")) return "video/mp4";
	return "";
}

let keyBytes;
try {
	keyBytes = decodeKey(keyText);
} catch (error) {
	die(String(error.message ?? error));
}

let response;
let ciphertext;
try {
	response = await fetch(rawURL, { cache: "no-store" });
	if (!response.ok) die(`ciphertext fetch returned HTTP ${response.status}`);
	ciphertext = new Uint8Array(await response.arrayBuffer());
} catch (error) {
	die(`ciphertext fetch failed: ${error.message ?? error}`);
}
if (ciphertext.length <= 12) die("ciphertext is too short to be valid");

let plaintext;
try {
	const key = await webcrypto.subtle.importKey("raw", keyBytes, "AES-GCM", false, ["decrypt"]);
	plaintext = new Uint8Array(
		await webcrypto.subtle.decrypt({ name: "AES-GCM", iv: ciphertext.slice(0, 12) }, key, ciphertext.slice(12)),
	);
} catch {
	die("decryption failed: wrong key or corrupted file");
}

const sha256 = createHash("sha256").update(plaintext).digest("hex");
const sniff = sniffType(plaintext);
const firstBytes = Buffer.from(plaintext.slice(0, 8)).toString("hex");

if (sha256 !== expectedSHA) die(`plaintext sha256 mismatch (${sha256.slice(0, 16)}… vs ${expectedSHA.slice(0, 16)}…)`);
if (expectedSize !== "" && String(plaintext.length) !== expectedSize) {
	die(`plaintext size mismatch (${plaintext.length} vs ${expectedSize})`);
}
if (expectedSniff !== "" && sniff !== expectedSniff) {
	die(`sniff mismatch (${sniff || "unrecognized"} vs ${expectedSniff})`);
}

console.log(JSON.stringify({ ok: true, size: plaintext.length, sha256, sniff, first_bytes: firstBytes }));
