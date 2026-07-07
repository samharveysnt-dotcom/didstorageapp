#!/usr/bin/env python3
"""
AGI script called from extensions.conf [from-supplier] on every inbound INVITE.

Args (positional):
  call_id, src_ip, to_uri, from_uri, asterisk_channel

asterisk_channel is the dialplan's ${CHANNEL} (e.g.
"PJSIP/globetelecom-00000063"). didapi stores it in the live-call metadata
so /live admin actions (hangup / warn / redirect) can target the exact
channel directly instead of grepping `pjsip show channels` by Exten — that
breaks after a redirect because the transferred channel's Exten changes.

Posts to didapi /sipctl/authorize, then sets dialplan vars:
  AUTH_DECISION       allow | deny | error
  AUTH_REASON         (deny / error reason)
  AUTH_ROUTE_KIND     sip_uri | ip | sip_account
  AUTH_ROUTE_TARGET   the target URI / ip:port / account-id
  AUTH_MAX_SECONDS    integer; the dialplan caps Dial() with L(ms) on this
  AUTH_RATE_PMC       per-minute rate in cents (string, may have decimals)
  AUTH_HANGUP_CAUSE   Q.850 cause to pass to Hangup() on the reject branch
                      (0 = fall back to the dialplan default, 21)
"""
import json
import os
import sys
import urllib.request


def agi_set(name: str, value: str) -> None:
    # AGI command: SET VARIABLE name "value"
    safe = str(value).replace('"', '\\"')
    sys.stdout.write(f'SET VARIABLE {name} "{safe}"\n')
    sys.stdout.flush()
    sys.stdin.readline()  # consume the AGI response


def main() -> None:
    # Read AGI environment header (we don't actually need it but must consume it).
    while True:
        line = sys.stdin.readline()
        if not line or line.strip() == "":
            break

    args = sys.argv[1:]
    call_id          = args[0] if len(args) > 0 else ""
    src_ip           = args[1] if len(args) > 1 else ""
    to_uri           = args[2] if len(args) > 2 else ""
    from_uri         = args[3] if len(args) > 3 else ""
    asterisk_channel = args[4] if len(args) > 4 else ""

    api_url = os.environ.get("DIDAPI_URL", "http://127.0.0.1") + "/sipctl/authorize"
    token = os.environ.get("KAMAILIO_AUTH_TOKEN", "")  # name kept for back-compat
    if not token:
        # Fallback: read from /etc/didstorage/auth_token. Catch both
        # FileNotFoundError (missing) and PermissionError (asterisk user
        # can't traverse /etc/didstorage — that dir must be 0755 even
        # though the file inside is 0640). Without this defence the
        # script crashed on a stack trace and Asterisk returned empty
        # AUTH_* vars, sending the call to the reject branch with no
        # cause code. See DEPLOY.md.
        try:
            with open("/etc/didstorage/auth_token") as f:
                token = f.read().strip()
        except (FileNotFoundError, PermissionError) as e:
            sys.stderr.write(
                f"dids-authorize: cannot read /etc/didstorage/auth_token: {e}\n"
                "dids-authorize: fix on the server: "
                "chmod 755 /etc/didstorage && "
                "chgrp asterisk /etc/didstorage/auth_token && "
                "chmod 0640 /etc/didstorage/auth_token\n"
            )
            # Fall through with empty token so /sipctl/authorize returns 401
            # and we set decision=error rather than crashing.

    body = json.dumps({
        "call_id":          call_id,
        "src_ip":           src_ip,
        "to_uri":           to_uri,
        "from_uri":         from_uri,
        "asterisk_channel": asterisk_channel,
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

    try:
        with urllib.request.urlopen(req, timeout=2) as r:
            resp = json.loads(r.read())
    except Exception as e:
        agi_set("AUTH_DECISION", "error")
        agi_set("AUTH_REASON",   f"http: {e}")
        return

    agi_set("AUTH_DECISION",     resp.get("decision", ""))
    agi_set("AUTH_REASON",       resp.get("reason", ""))
    agi_set("AUTH_ROUTE_KIND",   resp.get("route_kind", ""))
    agi_set("AUTH_ROUTE_TARGET", resp.get("route_target", ""))
    agi_set("AUTH_MAX_SECONDS",  str(resp.get("max_seconds", 0) or 0))
    agi_set("AUTH_RATE_PMC",     str(resp.get("rate_cents_per_min", 0) or 0))
    agi_set("AUTH_HANGUP_CAUSE", str(resp.get("hangup_cause", 0) or 0))


if __name__ == "__main__":
    main()
