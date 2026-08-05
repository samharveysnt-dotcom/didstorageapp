#!/bin/bash
# RETIRED 2026-08-05
#
# This script is Kamailio/rtpengine era. This platform switched to
# chan_pjsip long ago; running this against a live server would:
#   - rewrite LISTEN_ADDR to 127.0.0.1:8080, breaking the public :80 listener
#   - reinstall stale kamailio config on a server that no longer runs Kamailio
#   - short-circuit the migration tracking table
#
# It survives here only because it reads /root/.pg_didstorage_password
# which no server actually has, so it fails harmlessly. Rather than
# delete (which loses the fact of what happened), it now aborts loudly.
#
# Use `bash scripts/deploy.sh root@<host>` from the repo root instead.
# See README.md.

echo "" >&2
echo "  deploy/central/02-deploy-app.sh is RETIRED (Kamailio/rtpengine era)." >&2
echo "" >&2
echo "  Use:  bash scripts/deploy.sh root@<host>" >&2
echo "" >&2
echo "  See README.md for the full deploy flow." >&2
echo "" >&2
exit 2
