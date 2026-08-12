#!/usr/bin/env python3
"""
imessage-queue-bridge.py — forward iMessage requests into navigatorr's queue.

Watches the local Messages database for new texts from allowlisted senders and
POSTs each one to navigatorr's /request endpoint, where it waits for an agent.

Two deliberate safety properties:

  * The allowlist is required. With no NAVQ_ALLOW set the bridge refuses to run
    rather than forwarding every message on the machine.
  * A trigger prefix is required by default, so ordinary conversation does not
    become media requests. Only "get boston legal" is forwarded, not "get milk"
    unless you say so.

chat.db is opened read-only and immutable so this never locks or mutates the
database Messages.app is using.

Environment:
    NAVQ_URL     navigatorr base URL          (default http://127.0.0.1:8099)
    NAVQ_TOKEN   bearer token                 (required)
    NAVQ_ALLOW   comma-separated handles       REQUIRED, e.g. "+15551234567,me"
    NAVQ_PREFIX  trigger prefix               (default "get "; set "" to accept all)
    NAVQ_STATE   watermark file               (default ~/.config/navigatorr/imessage.state)

Usage:
    python3 imessage-queue-bridge.py            # forward new messages
    python3 imessage-queue-bridge.py --dry-run  # show what would be forwarded
    python3 imessage-queue-bridge.py --since 24 # look back N hours on first run
"""
import argparse
import json
import os
import sqlite3
import sys
import time
import urllib.error
import urllib.request

CHAT_DB = os.path.expanduser("~/Library/Messages/chat.db")
# chat.db stores dates as nanoseconds since 2001-01-01 UTC.
APPLE_EPOCH = 978307200


def env(name, default=None):
    v = os.environ.get(name)
    return v if v not in (None, "") else default


def state_path():
    return env("NAVQ_STATE", os.path.expanduser("~/.config/navigatorr/imessage.state"))


def read_watermark(default_ts):
    try:
        with open(state_path()) as f:
            return float(f.read().strip())
    except (OSError, ValueError):
        return default_ts


def write_watermark(ts):
    p = state_path()
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w") as f:
        f.write(str(ts))


def fetch_messages(since_unix, allow):
    """Return [(unix_ts, handle, text)] of inbound messages newer than since_unix."""
    uri = "file:%s?mode=ro&immutable=1" % CHAT_DB
    con = sqlite3.connect(uri, uri=True)
    try:
        cur = con.execute(
            """
            SELECT m.date, COALESCE(h.id, ''), COALESCE(m.text, '')
            FROM message m
            LEFT JOIN handle h ON m.handle_id = h.ROWID
            WHERE m.is_from_me = 0
              AND m.text IS NOT NULL
              AND m.date > ?
            ORDER BY m.date ASC
            """,
            (int((since_unix - APPLE_EPOCH) * 1e9),),
        )
        rows = cur.fetchall()
    finally:
        con.close()

    out = []
    for date, handle, text in rows:
        ts = date / 1e9 + APPLE_EPOCH
        if handle in allow:
            out.append((ts, handle, text.strip()))
    return out


def post_request(base, token, text, source):
    body = json.dumps({"text": text, "source": source}).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/request",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.load(r)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--since", type=float, default=1.0,
                    help="hours to look back when no watermark exists (default 1)")
    a = ap.parse_args()

    allow = {h.strip() for h in (env("NAVQ_ALLOW", "") or "").split(",") if h.strip()}
    if not allow:
        sys.exit("NAVQ_ALLOW is required (comma-separated handles). Refusing to "
                 "forward every message on this machine.")

    prefix = os.environ.get("NAVQ_PREFIX", "get ")
    base = env("NAVQ_URL", "http://127.0.0.1:8099")
    token = env("NAVQ_TOKEN", "")
    if not token:
        sys.exit("NAVQ_TOKEN is required. navigatorr refuses to serve the queue "
                 "endpoint without a token, so an empty one cannot work.")

    if not os.path.exists(CHAT_DB):
        sys.exit("no chat.db at %s" % CHAT_DB)

    watermark = read_watermark(time.time() - a.since * 3600)
    msgs = fetch_messages(watermark, allow)

    sent = skipped = 0
    newest = watermark
    for ts, handle, text in msgs:
        newest = max(newest, ts)
        if prefix and not text.lower().startswith(prefix.lower()):
            skipped += 1
            continue
        payload = text[len(prefix):].strip() if prefix else text
        if not payload:
            skipped += 1
            continue
        if a.dry_run:
            print("[dry] %s -> %r" % (handle, payload))
            sent += 1
            continue
        try:
            it = post_request(base, token, payload, "imessage:%s" % handle)
            print("queued %s: %s" % (it.get("id"), payload))
            sent += 1
        except urllib.error.URLError as e:
            print("POST failed (%s) — leaving watermark unmoved" % e, file=sys.stderr)
            sys.exit(1)

    if not a.dry_run and newest > watermark:
        write_watermark(newest)
    print("done: %d queued, %d skipped, %d seen" % (sent, skipped, len(msgs)))


if __name__ == "__main__":
    main()
