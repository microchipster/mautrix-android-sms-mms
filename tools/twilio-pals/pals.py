#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
"""twilio-pals: drive two virtual Twilio numbers to test group SMS/MMS.

Group MMS is a carrier feature, so exercising it needs 2+ real numbers. This
script turns two Twilio long codes ("a" and "b") into scriptable participants
that can text your phone, reply in the group and attach media.

Usage:
    pals.py send --from a --to phone,b --text "hey" [--media URL]
    pals.py recv [--number a|b|all] [--since ISO8601] [--limit N]
    pals.py watch [--number all] [--interval 5]

Config (first match wins):
    $TWILIO_ACCOUNT_SID / $TWILIO_AUTH_TOKEN / $PAL_A / $PAL_B / $PHONE
    ~/.config/twilio-pals/config.json
    {
      "account_sid": "AC...",
      "auth_token": "...",
      "numbers": {"a": "+15550001111", "b": "+15550002222"},
      "phone": "+15550100100"
    }

Aliases for --to: "phone" (your phone), "a", "b" or any E.164 number.
Group sends (more than one recipient) require --media: Twilio only supports
multi-recipient messages as MMS.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

API = "https://api.twilio.com/2010-04-01"
CONFIG_PATH = os.path.expanduser("~/.config/twilio-pals/config.json")


def die(message: str) -> "None":
    print(f"error: {message}", file=sys.stderr)
    sys.exit(1)


def load_config() -> dict:
    cfg: dict = {}
    if os.path.exists(CONFIG_PATH):
        with open(CONFIG_PATH, encoding="utf-8") as fh:
            cfg = json.load(fh)
    sid = os.environ.get("TWILIO_ACCOUNT_SID") or cfg.get("account_sid")
    token = os.environ.get("TWILIO_AUTH_TOKEN") or cfg.get("auth_token")
    numbers = dict(cfg.get("numbers") or {})
    for key, env in (("a", "PAL_A"), ("b", "PAL_B")):
        if os.environ.get(env):
            numbers[key] = os.environ[env]
    phone = os.environ.get("PHONE") or cfg.get("phone")
    if not sid or not token:
        die(f"missing Twilio credentials (env or {CONFIG_PATH})")
    if not numbers:
        die(f"missing PAL_A/PAL_B (env or {CONFIG_PATH})")
    return {"sid": sid, "token": token, "numbers": numbers, "phone": phone}


def request(cfg: dict, method: str, path: str, params: dict | list[tuple[str, str]] | None = None) -> dict:
    url = f"{API}/Accounts/{cfg['sid']}{path}"
    data = urllib.parse.urlencode(params).encode() if params else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header(
        "Authorization",
        "Basic " + base64.b64encode(f"{cfg['sid']}:{cfg['token']}".encode()).decode(),
    )
    if data:
        req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as err:
        body = err.read().decode(errors="replace")
        try:
            detail = json.loads(body).get("message", body)
        except json.JSONDecodeError:
            detail = body
        die(f"Twilio HTTP {err.code}: {detail}")
    except urllib.error.URLError as err:
        die(f"Twilio unreachable: {err.reason}")
    return {}


def resolve(cfg: dict, aliases: list[str]) -> list[str]:
    out: list[str] = []
    for alias in aliases:
        alias = alias.strip()
        if not alias:
            continue
        if alias == "phone":
            if not cfg["phone"]:
                die("no phone number configured; use an E.164 number instead of 'phone'")
            out.append(cfg["phone"])
        elif alias in cfg["numbers"]:
            out.append(cfg["numbers"][alias])
        elif alias.startswith("+"):
            out.append(alias)
        else:
            die(f"unknown recipient alias {alias!r} (use phone/a/b or +E.164)")
    return out


def cmd_send(cfg: dict, args: argparse.Namespace) -> None:
    sender = resolve(cfg, [args.sender or "a"])[0]
    recipients = resolve(cfg, args.to.split(","))
    if not recipients:
        die("no recipients")
    if len(recipients) > 1 and not args.media:
        die("multi-recipient messages require --media (Twilio only groups MMS)")
    params: list[tuple[str, str]] = [("From", sender), ("To", ",".join(recipients))]
    if args.text:
        params.append(("Body", args.text))
    for media in args.media:
        params.append(("MediaUrl", media))
    resp = request(cfg, "POST", "/Messages.json", params)
    print(
        f"sent {resp.get('sid', '?')} {sender} -> {', '.join(recipients)} "
        f"status={resp.get('status')} parts={resp.get('num_segments')}",
    )


def iso(value: str) -> str:
    dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def fetch(cfg: dict, number: str, limit: int) -> list[dict]:
    """Return messages involving `number`, newest first."""
    seen: dict[str, dict] = {}
    for param in ("From", "To"):
        page = request(
            cfg,
            "GET",
            f"/Messages.json?{urllib.parse.urlencode({param: number, 'PageSize': limit})}",
        )
        for msg in page.get("messages", []):
            seen[msg["sid"]] = msg
    return sorted(seen.values(), key=lambda m: m.get("date_created", ""), reverse=True)


def describe(cfg: dict, number: str, msg: dict) -> str:
    direction = "OUT" if msg.get("from") == number else "IN "
    media = int(msg.get("num_media") or 0)
    body = (msg.get("body") or "").replace("\n", " ")
    if len(body) > 120:
        body = body[:117] + "..."
    tags = []
    if media:
        tags.append(f"media={media}")
    if msg.get("num_segments") and msg["num_segments"] != "1":
        tags.append(f"segments={msg['num_segments']}")
    if msg.get("status") not in (None, "received", "delivered", "sent"):
        tags.append(f"status={msg['status']}")
    suffix = f" [{', '.join(tags)}]" if tags else ""
    return f"{msg.get('date_created')} {number} {direction} {msg.get('from')} -> {msg.get('to')}: {body}{suffix}"


def cmd_recv(cfg: dict, args: argparse.Namespace) -> None:
    aliases = list(cfg["numbers"]) if args.number == "all" else [args.number]
    since = iso(args.since) if args.since else None
    for alias in aliases:
        number = cfg["numbers"][alias]
        messages = fetch(cfg, number, args.limit)
        if since:
            messages = [m for m in messages if m.get("date_created", "") >= since]
        print(f"--- {alias} {number} ({len(messages)}) ---")
        for msg in messages:
            print(describe(cfg, number, msg))


def cmd_watch(cfg: dict, args: argparse.Namespace) -> None:
    aliases = list(cfg["numbers"]) if args.number == "all" else [args.number]
    numbers = {alias: cfg["numbers"][alias] for alias in aliases}
    print(f"watching {', '.join(numbers.values())} every {args.interval}s (ctrl-c to stop)")
    seen: set[str] = set()
    first = True
    while True:
        for alias, number in numbers.items():
            for msg in fetch(cfg, number, 20):
                sid = msg.get("sid", "")
                if sid in seen:
                    continue
                seen.add(sid)
                # Only report inbound traffic plus our own sends for context.
                if msg.get("from") == number:
                    if not first:
                        continue
                    direction = "OUT"
                else:
                    direction = "IN "
                print(f"{datetime.now().strftime('%H:%M:%S')} {direction} ({alias}) {describe(cfg, number, msg)}")
        first = False
        try:
            time.sleep(args.interval)
        except KeyboardInterrupt:
            return


def main() -> None:
    parser = argparse.ArgumentParser(description="drive two Twilio numbers for group SMS/MMS tests")
    sub = parser.add_subparsers(dest="command", required=True)

    send = sub.add_parser("send", help="send an SMS/MMS from one of the numbers")
    send.add_argument("--from", dest="sender", default="a", help="sender alias (default: a)")
    send.add_argument("--to", required=True, help="comma-separated recipients (phone/a/b/+E.164)")
    send.add_argument("--text", default="", help="message body")
    send.add_argument("--media", action="append", default=[], help="media URL (repeatable)")
    send.set_defaults(func=cmd_send)

    recv = sub.add_parser("recv", help="list messages involving the numbers")
    recv.add_argument("--number", default="all", help="a, b or all (default: all)")
    recv.add_argument("--since", help="only messages at/after this ISO8601 timestamp")
    recv.add_argument("--limit", type=int, default=50)
    recv.set_defaults(func=cmd_recv)

    watch = sub.add_parser("watch", help="poll for new inbound messages")
    watch.add_argument("--number", default="all", help="a, b or all (default: all)")
    watch.add_argument("--interval", type=float, default=5.0)
    watch.set_defaults(func=cmd_watch)

    args = parser.parse_args()
    cfg = load_config()
    args.func(cfg, args)


if __name__ == "__main__":
    main()
