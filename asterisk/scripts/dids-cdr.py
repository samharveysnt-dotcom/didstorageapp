#!/usr/bin/env python3
"""
AGI hangup handler — fires once per call from the [from-supplier] h-extension.

Args (positional):
  call_id, cdr_start, cdr_answer, cdr_end, billsec, hangup_cause, src_uri, dst_uri

CDR timestamps come in Asterisk format "YYYY-MM-DD HH:MM:SS"; we parse to unix
seconds and post to didapi /sipctl/cdr which charges the user and writes the
CDR row.
"""
import json
import os
import sys
import time
import urllib.request
from datetime import datetime, timezone


def to_unix(s: str) -> int:
    s = (s or "").strip()
    if not s or s == "(null)":
        return 0
    try:
        # Asterisk default CDR format
        dt = datetime.strptime(s, "%Y-%m-%d %H:%M:%S")
        return int(dt.replace(tzinfo=timezone.utc).timestamp())
    except ValueError:
        try:
            return int(s)
        except ValueError:
            return 0


def main() -> None:
    while True:
        line = sys.stdin.readline()
        if not line or line.strip() == "":
            break

    args = sys.argv[1:]
    call_id      = args[0] if len(args) > 0 else ""
    cdr_start    = args[1] if len(args) > 1 else ""
    cdr_answer   = args[2] if len(args) > 2 else ""
    cdr_end      = args[3] if len(args) > 3 else ""
    billsec_str  = args[4] if len(args) > 4 else "0"
    hangup_cause = args[5] if len(args) > 5 else ""
    src_uri      = args[6] if len(args) > 6 else ""
    dst_uri      = args[7] if len(args) > 7 else ""

    if not call_id:
        return

    try:
        billsec = int(billsec_str)
    except ValueError:
        billsec = 0

    api_url = os.environ.get("DIDAPI_URL", "http://127.0.0.1") + "/sipctl/cdr"
    token = os.environ.get("KAMAILIO_AUTH_TOKEN", "")
    if not token:
        # See dids-authorize.py for the rationale on catching PermissionError.
        try:
            with open("/etc/didstorage/auth_token") as f:
                token = f.read().strip()
        except (FileNotFoundError, PermissionError) as e:
            sys.stderr.write(
                f"dids-cdr: cannot read /etc/didstorage/auth_token: {e}\n"
            )

    started_at  = to_unix(cdr_start) or int(time.time())
    answered_at = to_unix(cdr_answer)
    ended_at    = to_unix(cdr_end) or int(time.time())

    body = json.dumps({
        "call_id":        call_id,
        "reservation_id": call_id,
        "started_at":     started_at,
        "answered_at":    answered_at,
        "ended_at":       ended_at,
        "billsec":        billsec,
        "hangup_cause":   hangup_cause,
        "src_uri":        src_uri,
        "dst_uri":        dst_uri,
    }).encode()

    req = urllib.request.Request(
        api_url,
        data=body,
        headers={
            "Content-Type": "application/json",
            "X-DIDS-Auth":  token,
        },
        method="POST",
    )

    # Retry the POST up to 3 times with a small back-off. Every failed
    # attempt here leaves the /live index and the act:* concurrency SETs
    # holding a ghost row that only the reconciler goroutine can clean up
    # — retrying here catches the transient case (didapi mid-restart, DB
    # ledger stall on the write, TCP RST during connect) before it lands
    # in Redis at all. The reconciler is still the ultimate backstop.
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req, timeout=5) as _r:
                _r.read()
            return
        except Exception as e:
            sys.stderr.write(
                f"dids-cdr.py: attempt {attempt + 1}/3 failed: {e}\n"
            )
            if attempt < 2:
                time.sleep(0.5 * (attempt + 1))
    sys.stderr.write(
        f"dids-cdr.py: all attempts failed, call_id={call_id} left for reconciler\n"
    )


if __name__ == "__main__":
    main()
