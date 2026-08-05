# RTP timestamp inversion under loss (audit residual §6)

**Status:** Open. Not blocking, does not prevent subjective audio
improvement, but a stricter far end may handle it worse than our test
peer did. Requires an Asterisk-level fix, not a config knob.

**First seen:** 2026-08-05 audit test call, `sipcap3.pcap` on
`144.31.164.72`, third call at t≈63s. 166 backward-timestamp packets out
of 1070 (**15.5%**) on our outbound stream. Anomaly rate tracks the
inbound loss rate (18.7%) 1:1; a low-loss control call on identical
config had 1 anomaly in 3073 packets.

**Not seen:** 2026-08-05 15:30 post-fix retest (call to sip:4444@sip2sip.info).
Inbound loss was 0.0%, so PLC never had to synthesise any frames, so the
condition can't fire.

## Observed pattern

Outbound RTP frames on the affected call, contiguous sequence numbers
(zero discontinuities), clean 20 ms wall-clock spacing:

```
seq=17115 ts=2587959192 dts=+632     ← anomaly (would want +160)
seq=17116 ts=2587959352 dts=+160     ← normal (1 frame @ 8 kHz)
seq=17117 ts=2587959040 dts=-312     ← BACKWARDS
seq=17118 ts=2587959672 dts=+632     ← anomaly again
```

Numerology (8 kHz PCM, so 160 samples = 20 ms = 1 frame):

| Value | Samples | ms | Frames |
|---|---|---|---|
| Normal delta | 160 | 20 | 1 |
| `+632` | 632 | 79 | 3.95 |
| `-312` | 312 | 39 | 1.95 |
| Net per (`+632`, `-312`) pair | 320 | 40 | 2 |
| 8-sample offset in both figures | 8 | 1 | 0.05 |

## Working hypothesis

Asterisk's `JITTERBUFFER` emits a **PLC interpolation frame** with a
synthesised timestamp when it detects a sequence-number gap on the
read-side (that's the whole point of the interlock in `main/plc.c` +
`main/abstract_jb.c:385`). The interpolation frame's timestamp is
computed from "expected time" (when the missing frame *would* have
arrived), while the next *real* frame that the buffer delivers carries
its **original RTP timestamp** from the wire — which is EARLIER than
the synthesised one because the buffer was ahead of playout by the
buffer's target depth (`fixed`/60ms in the current config).

Net effect: the outbound RTP stream contains alternating "synthesised
future" and "real historical" timestamps, giving the `+632 / −312`
alternation. The 8-sample (1 ms) drift is consistent with a small
integer rounding on the interpolation timestamp calculation — a
`sample_rate / 1000` cadence approximation somewhere in the buffer
resync path.

## Impact assessment

- **Sequence numbers stay perfectly contiguous.** Receivers that
  order packets by `rtp.seq` — the RFC 3550-conformant path — are not
  affected.
- **Receivers that use `rtp.timestamp` for playout scheduling** (some
  strict softphones, some carrier-grade SBCs) may mis-schedule the
  "backwards" frame. Symptom would be a warble or click, NOT dropped
  audio.
- Subjective feedback on the affected call was *"sounded much
  better"* despite the anomalies — the improvement from PLC
  concealment massively outweighed any residual timestamp confusion.

Bottom line: **acceptable-but-suboptimal**. Worth a follow-up patch or
Asterisk upgrade path, not a rollback of the audio-remediation.

## Investigation checklist for the next attempt

Requires deliberately creating loss on the return leg. A convenient
way is `tc netem` on the RTP port range from an intermediate host,
OR reusing the audit's original supplier route (which had 19% real
loss from an upstream policer):

```bash
# 1. Capture with timestamp column explicit
tshark -T fields -e frame.time_epoch -e rtp.seq -e rtp.timestamp \
       -Y "rtp and ip.src == 144.31.164.72" \
       -r /var/log/testcalls/testcall-<TS>.pcap \
  | awk 'NR==1{prev=$3;next} {d=$3-prev; if(d<0) printf "BACKWARDS seq=%s ts=%s d=%d\n", $2, $3, d; prev=$3}'

# 2. Count backwards steps as % of total
tshark -T fields -e rtp.timestamp -Y "rtp and ip.src == 144.31.164.72" \
       -r /var/log/testcalls/testcall-<TS>.pcap \
  | awk 'NR==1{prev=$1;count=0;inv=0;next} {d=$1-prev; if(d<0)inv++; count++; prev=$1} \
         END{printf "packets=%d  backwards=%d (%.1f%%)\n", count, inv, 100*inv/count}'
```

**Config permutations to compare (all reload-only, no restart needed):**

| Test | JITTERBUFFER on jb-called-trunk | genericplc_on_equal_codecs | Expected outcome |
|---|---|---|---|
| Baseline (current) | `fixed`/60 ms | true | ~15% backwards @ 19% loss |
| Adaptive | `adaptive`/200,1000,40 | true | may auto-resize away the drift |
| Fixed 30 ms | `fixed`/30,1000 | true | half the buffer depth = less drift? |
| Fixed 100 ms | `fixed`/100,1000 | true | larger buffer, timestamps sync better? |
| PLC off | `fixed`/60 ms | false | if backwards vanish → PLC is the source |
| No JB | *(remove b() handler)* | true | PLC can't fire without JB — control |

The `PLC off` test is the decisive one. If backwards vanish, the source
is `main/plc.c`'s interpolation timestamp calculation. If they persist,
it's the buffer's own resync path in `main/abstract_jb.c`.

## If it reproduces and matters

Options in order of effort:

1. **Bump to Asterisk 21 or 22.** Check upstream commit log for
   `plc.c` / `abstract_jb.c` fixes between 20.20.1 and current. The
   audit noted `strictrtp`, `rtcpinterval` etc. as compiled-in
   defaults on 20.20.1 — the timestamp resync code has had multiple
   fix commits post-20.

2. **Patch our source build.** `scripts/bootstrap.sh` builds Asterisk
   from source at pinned `ASTERISK_VERSION`, so a local patch to
   `main/abstract_jb.c` around the interpolation-frame timestamp
   calculation is deployable via a bootstrap re-run
   (`ASTERISK_VERSION=20.20.1-patched` after committing the patch to
   `/usr/src/asterisk/`).

3. **Move to Opus with in-band FEC** on the affected leg. Requires
   both ends offering Opus, which wholesale SIP trunks generally
   don't. Only viable on softphone-to-softphone paths we control.

## Related files

- `/etc/asterisk/extensions.conf` — `[jb-called-trunk]` context @ line ~200
- `/etc/asterisk/codecs.conf` line 75 — `genericplc_on_equal_codecs => true`
- Asterisk source: `main/abstract_jb.c:385` (interpolation trigger),
  `main/plc.c` (synthesis), `main/channel.c:6792` (PLC-on-equal-codecs
  gate)

## Not to try

Recorded so nobody wastes time:

- **Toggling `rtp_symmetric`** — orthogonal to timestamp math.
- **Raising jitter buffer beyond ~100 ms** — pushes one-way latency
  past ITU-T G.114's 150 ms guideline for no benefit.
- **`send_rpid=no` / connection-line suppression** — no re-INVITEs
  observed on the affected dialog.
- **DSCP marking (`tos_audio=ef`)** — audit already rejected as dead
  on this virtio NIC.
