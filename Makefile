# DIDStorage — Makefile RETIRED 2026-08-05
#
# This Makefile was Kamailio/rtpengine-era and pointed at the wrong host
# (root@45.8.93.244). Its `migrate` target looped every *.up.sql with no
# _migrations_log gate, which on a live server would double-run every
# migration. Its `push` shipped kamailio/central.cfg and rtpengine.service,
# neither of which exist on the current chan_pjsip platform. Both dangers
# were only inert by accident (they read /root/.pg_didstorage_password,
# which does not exist).
#
# The current, correct deploy paths are:
#
#   scripts/install.sh                     Fresh install on a new Debian 12 VM.
#                                          Wipes anything already there. See README.md.
#
#   bash scripts/deploy.sh root@<host>     Update an existing server IN PLACE.
#                                          Ships new binary + migrations + Asterisk
#                                          configs; preserves DB, KYC, audio.
#
# Both live under scripts/ and share a common pre-deploy pg_dump + reload
# path. See README.md for the full flow.

.PHONY: retired

retired:
	@echo ""
	@echo "  This Makefile is retired. Use one of:"
	@echo ""
	@echo "    scripts/install.sh                      # fresh install"
	@echo "    bash scripts/deploy.sh root@<host>      # in-place update"
	@echo ""
	@echo "  See README.md."
	@exit 2

# Any legacy target (build/deploy/migrate/tail-app/…) falls through here
# so nobody can accidentally invoke one against the wrong host.
%:
	@$(MAKE) --no-print-directory retired
