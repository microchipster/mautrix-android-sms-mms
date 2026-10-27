#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
"""cdn_crypt.py — app-side crypto contract of the bridge file service.

Replicates the uploader described in docs/cdn-file-service.md: AES-256-GCM
with a random 32-byte key (unpadded base64url, lives only in the URL
fragment) and a random 12-byte nonce; the request body is
``nonce || ciphertext || tag``. Additional authenticated data is never used.

Only ids, sizes, keys and plaintext hashes are printed; plaintext/token/body
bytes stay out of the logs.

Usage:
  cdn_crypt.py encrypt PLAINTEXT_FILE CIPHERTEXT_FILE
      Prints JSON: {"key": "...", "size": N, "plaintext_size": N,
                    "plaintext_sha256": "..."}
  cdn_crypt.py decrypt CIPHERTEXT_FILE KEY_B64URL OUTPUT_FILE
      Prints the SHA-256 of the decrypted plaintext. Exits non-zero (and
      writes no output file) when the key is wrong or the body is corrupt.
  cdn_crypt.py random-key
      Prints a fresh unpadded base64url 32-byte key (for wrong-key probes).
"""

from __future__ import annotations

import base64
import hashlib
import json
import secrets
import sys

try:  # preferred: pyca/cryptography
    from cryptography.exceptions import InvalidTag as _InvalidTag
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM as _AESGCM
except ImportError:  # pragma: no cover - fallback for pycryptodome hosts
    _AESGCM = None

    class _InvalidTag(Exception):
        pass


try:  # fallback: pycryptodome
    from Crypto.Cipher import AES as _PyCryptodomeAES
except ImportError:  # pragma: no cover - neither backend installed
    _PyCryptodomeAES = None

KEY_BYTES = 32
NONCE_BYTES = 12
TAG_BYTES = 16


def b64url_encode(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def b64url_decode(text: str) -> bytes:
    padded = text + "=" * (-len(text) % 4)
    try:
        return base64.urlsafe_b64decode(padded.encode("ascii"))
    except (ValueError, base64.binascii.Error) as exc:
        raise ValueError("invalid base64url key") from exc


def encrypt(key: bytes, nonce: bytes, plaintext: bytes) -> bytes:
    if _AESGCM is not None:
        return _AESGCM(key).encrypt(nonce, plaintext, None)
    if _PyCryptodomeAES is None:
        raise RuntimeError("no AES-GCM backend: install python3-cryptography or pycryptodome")
    cipher = _PyCryptodomeAES.new(key, _PyCryptodomeAES.MODE_GCM, nonce=nonce)
    return b"".join(cipher.encrypt_and_digest(plaintext))


def decrypt(key: bytes, nonce: bytes, body: bytes) -> bytes:
    if _AESGCM is not None:
        return _AESGCM(key).decrypt(nonce, body, None)
    if _PyCryptodomeAES is None:
        raise RuntimeError("no AES-GCM backend: install python3-cryptography or pycryptodome")
    cipher = _PyCryptodomeAES.new(key, _PyCryptodomeAES.MODE_GCM, nonce=nonce)
    return cipher.decrypt_and_verify(body[:-TAG_BYTES], body[-TAG_BYTES:])


def parse_key(text: str) -> bytes:
    raw = b64url_decode(text.strip())
    if len(raw) != KEY_BYTES:
        raise ValueError(f"key is {len(raw)} bytes, want {KEY_BYTES} (AES-256)")
    return raw


def read_file(path: str) -> bytes:
    with open(path, "rb") as handle:
        return handle.read()


def write_file(path: str, data: bytes) -> None:
    with open(path, "wb") as handle:
        handle.write(data)


def fail(message: str, code: int = 1) -> "NoReturn":
    print(f"cdn_crypt: {message}", file=sys.stderr)
    raise SystemExit(code)


def main(argv: list[str]) -> int:
    if len(argv) < 2:
        fail("usage: cdn_crypt.py encrypt|decrypt|random-key ...", 2)
    command = argv[1]

    if command == "random-key" and len(argv) == 2:
        print(b64url_encode(secrets.token_bytes(KEY_BYTES)))
        return 0

    if command == "encrypt" and len(argv) == 4:
        plaintext = read_file(argv[2])
        key = secrets.token_bytes(KEY_BYTES)
        nonce = secrets.token_bytes(NONCE_BYTES)
        sealed = encrypt(key, nonce, plaintext)
        body = nonce + sealed
        write_file(argv[3], body)
        print(
            json.dumps(
                {
                    "key": b64url_encode(key),
                    "size": len(body),
                    "plaintext_size": len(plaintext),
                    "plaintext_sha256": hashlib.sha256(plaintext).hexdigest(),
                }
            )
        )
        return 0

    if command == "decrypt" and len(argv) == 5:
        body = read_file(argv[2])
        try:
            key = parse_key(argv[3])
        except ValueError as exc:
            fail(f"invalid key: {exc}", 3)
        if len(body) <= NONCE_BYTES + TAG_BYTES:
            fail("ciphertext body is too short to be valid", 3)
        try:
            plaintext = decrypt(key, body[:NONCE_BYTES], body[NONCE_BYTES:])
        except Exception:
            fail("decryption failed: wrong key or corrupted file", 3)
        write_file(argv[4], plaintext)
        print(hashlib.sha256(plaintext).hexdigest())
        return 0

    fail("usage: cdn_crypt.py encrypt PLAINTEXT CIPHERTEXT | decrypt CIPHERTEXT KEY OUTPUT | random-key", 2)
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
