// Package callquality analyses RTP media statistics for a completed call so
// operators can diagnose audible artefacts — buffering, dropouts, robotic
// audio, one-way media — from the same rolling pcap files that back the SIP
// trace viewer.
//
// The analysis runs `tshark -q -z rtp,streams` with heuristic RTP dissection
// enabled (media packets have no fixed port; the SDP that would tell tshark
// their port is in a separate direction so it can't self-associate without
// help), then filters the resulting stream table down to just the flows that
// touched an IP address seen in the SIP dialog. Each surviving stream gets
// packet count / loss / jitter / max-delta metrics, and the whole set is
// rolled up into a plain-English verdict.
//
// Verdict thresholds mirror what ITU-T G.107 / RFC 3550 practitioners treat
// as audible on a G.711 stream:
//
//	packet loss:       >0.5% noticeable · ≥1% poor · ≥5% bad
//	max inter-arrival: >60ms noticeable · ≥100ms poor · ≥200ms bad
//	mean jitter:       >20ms borderline · ≥30ms poor · ≥50ms bad
//
// The "buffering" symptom the user described almost always maps to mean
// jitter ≥30ms combined with occasional max-delta ≥100ms — the jitter
// buffer refills constantly and audible pumping results.
package callquality

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"didstorage/internal/siptrace"
)

// Report is the analysed output for one call. Attached to the trace-page
// template render context; also returned by the reseller API sip-trace
// endpoint (sanitised — the same IP-rewrite pass runs).
type Report struct {
	Streams []Stream `json:"streams"`
	Verdict Verdict  `json:"verdict"`
	// ScannedPcaps is the set of capture files we looked at. Empty when the
	// pcap directory itself is missing.
	ScannedPcaps []string `json:"scanned_pcaps,omitempty"`
	// NoRTPCaptured is set when RTP dissection returned zero streams for
	// this call's endpoints in the time window. On a fresh install this
	// almost always means the capture filter is SIP-only (default was
	// port 5060/5061), not that the call actually had no media.
	NoRTPCaptured bool `json:"no_rtp_captured"`
}

// Stream is one RTP flow — one direction of audio.
type Stream struct {
	SrcAddr      string  `json:"src_addr"`      // ip:port
	DstAddr      string  `json:"dst_addr"`      // ip:port
	SSRC         string  `json:"ssrc"`          // hex, tshark's format
	PayloadType  string  `json:"payload_type"`  // "ITU-T G.711 PCMU", "GSM 06.10", …
	Packets      int     `json:"packets"`
	Lost         int     `json:"lost"`
	LossPercent  float64 `json:"loss_percent"`
	MaxDeltaMs   float64 `json:"max_delta_ms"`   // biggest gap between two arrivals
	MeanJitterMs float64 `json:"mean_jitter_ms"` // RFC 3550 interarrival jitter avg
	MaxJitterMs  float64 `json:"max_jitter_ms"`
	Problems     bool    `json:"problems"`  // tshark's own X flag
	Direction    string  `json:"direction"` // "in" (they→us) / "out" (us→them) / "?"

	// TimingUnreliable is true when tshark's jitter / max-delta numbers on
	// this stream can't be trusted at face value. Set when the payload
	// column contains Comfort Noise (RFC 3389, PT=13) or two comma-separated
	// codec names — both cases confuse tshark's inter-arrival-time
	// accumulator. Silence packets in DTX / CN streams arrive irregularly by
	// design, so a jitter buffer isn't refilling; a codec transition resets
	// the RTP timestamp base but tshark keeps computing from the old one, so
	// the accumulator explodes into multi-minute nonsense values.
	//
	// When true, the verdict logic skips this stream's jitter and max-delta
	// contributions — packet loss remains a valid signal.
	TimingUnreliable bool   `json:"timing_unreliable"`
	TimingNote       string `json:"timing_note,omitempty"`
}

// Verdict rolls all streams up into one label the GUI can badge.
type Verdict struct {
	Level   string   `json:"level"`   // "good" | "acceptable" | "poor" | "bad" | "unknown"
	Summary string   `json:"summary"` // one-line explanation, shown under the badge
	Issues  []string `json:"issues"`  // per-symptom bullet list
}

// Analyze runs tshark on the pcaps and returns a scoped Report.
//
//	ourPublicIP  — the platform's outward-facing IP; used to label direction.
//	dialogIPs    — every ip:port label seen on the SIP side. RTP streams that
//	               don't touch one of these hosts are dropped, so we don't
//	               show media from unrelated concurrent calls.
//	windowStart/End — call's [started_at, ended_at]. Time-filters tshark's
//	               read so we don't count RTP from before/after this call.
func Analyze(
	ctx context.Context,
	ourPublicIP string,
	dialogIPs []string,
	windowStart, windowEnd time.Time,
) (*Report, error) {
	pcaps, err := filepath.Glob(filepath.Join(siptrace.PcapDir, "*.pcap"))
	if err != nil {
		return nil, fmt.Errorf("glob pcaps: %w", err)
	}
	sort.Strings(pcaps)

	rep := &Report{ScannedPcaps: pcaps}
	if len(pcaps) == 0 {
		rep.NoRTPCaptured = true
		rep.Verdict = Verdict{Level: "unknown", Summary: "No pcaps available to analyse."}
		return rep, nil
	}

	// Slack the window a beat either side. Early media starts before the 200
	// OK; RTP can trickle for a few packets after BYE while asterisk drains.
	tStart := windowStart.Add(-5 * time.Second).Unix()
	tEnd := windowEnd.Add(5 * time.Second).Unix()
	if windowStart.IsZero() {
		tStart = 0
	}
	if windowEnd.IsZero() {
		tEnd = time.Now().Add(24 * time.Hour).Unix()
	}

	// IP set for scoping. Empty = show every stream (rare — only when the
	// SIP side had no messages, e.g. supplier-IP-ACL denial with the pcap
	// still holding the INVITE).
	ipSet := map[string]bool{}
	for _, a := range dialogIPs {
		host, _ := splitAddr(a)
		if host != "" {
			ipSet[host] = true
		}
	}

	var all []Stream
	for _, p := range pcaps {
		streams, err := tsharkRTPStreams(ctx, p, tStart, tEnd)
		if err != nil {
			// Skip this pcap but keep going — one broken file shouldn't
			// zero the whole report.
			continue
		}
		all = append(all, streams...)
	}

	// Scope + direction.
	var scoped []Stream
	seen := map[string]bool{}
	for _, s := range all {
		srcIP, _ := splitAddr(s.SrcAddr)
		dstIP, _ := splitAddr(s.DstAddr)
		if len(ipSet) > 0 && !ipSet[srcIP] && !ipSet[dstIP] {
			continue
		}
		// Dedupe on SSRC+src+dst — same stream can appear if it straddled a
		// midnight rotation and we scanned two pcaps.
		key := s.SSRC + "|" + s.SrcAddr + "|" + s.DstAddr
		if seen[key] {
			continue
		}
		seen[key] = true

		if srcIP == ourPublicIP {
			s.Direction = "out"
		} else if dstIP == ourPublicIP {
			s.Direction = "in"
		} else {
			s.Direction = "?"
		}
		scoped = append(scoped, s)
	}
	// Sort by direction then packet count desc — the main stream shows first.
	sort.Slice(scoped, func(i, j int) bool {
		if scoped[i].Direction != scoped[j].Direction {
			return scoped[i].Direction < scoped[j].Direction
		}
		return scoped[i].Packets > scoped[j].Packets
	})

	rep.Streams = scoped
	if len(scoped) == 0 {
		rep.NoRTPCaptured = true
	}
	rep.Verdict = deriveVerdict(scoped)
	return rep, nil
}

// splitAddr splits "ip:port" (IPv4-only, matching sip-capture's filter). We
// don't parse IPv6 — the capture filter excludes it.
func splitAddr(a string) (string, string) {
	if i := strings.LastIndexByte(a, ':'); i > 0 {
		return a[:i], a[i+1:]
	}
	return a, ""
}

// tsharkRTPStreams runs `tshark -z rtp,streams` on one pcap and parses the
// tap output. Heuristic RTP dissection is enabled so packets are recognised
// even when tshark didn't observe the SDP offer/answer.
func tsharkRTPStreams(ctx context.Context, pcap string, tStart, tEnd int64) ([]Stream, error) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// -2 -R runs a two-pass read that lets the display filter constrain what
	// the -z tap sees (a plain -Y bypasses stat taps in most tshark builds).
	// The filter uses frame.time_epoch so we don't count RTP from unrelated
	// calls that landed in the same daily pcap.
	filter := fmt.Sprintf(
		`rtp and frame.time_epoch >= %d and frame.time_epoch <= %d`,
		tStart, tEnd,
	)
	args := []string{
		"-n", "-r", pcap,
		"-o", "rtp.heuristic_rtp:TRUE",
		"-2", "-R", filter,
		"-q", "-z", "rtp,streams",
	}
	cmd := exec.CommandContext(cctx, "tshark", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	// Ignore exit code — tshark sometimes returns non-zero on truncated
	// live pcaps but still writes valid stats above the error.
	_ = cmd.Run()
	return parseRTPStreams(stdout.Bytes()), nil
}

// parseRTPStreams parses tshark's `rtp,streams` tap. The table layout has
// wobbled slightly between tshark versions, so we work by column landmarks
// rather than fixed offsets. Recent layouts:
//
//	Start time  End time  Src IP  Port  Dest IP  Port  SSRC  Payload  Pkts  Lost  Min Delta  Mean Delta  Max Delta  Min Jitter  Mean Jitter  Max Jitter  Problems?
//
// Older layouts drop the Min columns. We handle both.
func parseRTPStreams(out []byte) []Stream {
	var streams []Stream
	scan := bufio.NewScanner(bytes.NewReader(out))
	scan.Buffer(make([]byte, 1<<20), 1<<22)
	inTable := false
	for scan.Scan() {
		line := strings.TrimRight(scan.Text(), " \t\r")
		if strings.Contains(line, "RTP Streams") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "=") {
			inTable = false
			continue
		}
		low := strings.ToLower(trim)
		if strings.Contains(low, "start time") && strings.Contains(low, "end time") {
			continue
		}
		if s := parseStreamLine(strings.Fields(line)); s != nil {
			streams = append(streams, *s)
		}
	}
	return streams
}

// parseStreamLine turns one row of the RTP-streams table into a Stream.
// Returns nil on rows we don't recognise (blank, banner, error text).
//
// Real tshark 4.x row shape (whitespace-separated):
//
//	<startT> <endT> <srcIP> <srcPort> <dstIP> <dstPort> <SSRC> <payload…>
//	  <pkts> <lost> "(X.X%)" <min-delta> <mean-delta> <max-delta>
//	  <min-jitter> <mean-jitter> <max-jitter> [X]
//
// The "(X.X%)" token is tshark's parenthesised loss-percentage AFTER the
// Lost integer — it's a second whitespace token, not part of the number.
// Older builds omit the min-delta and min-jitter columns, giving:
//
//	<startT> <endT> ... <ssrc> <payload…> <pkts> <lost> "(X.X%)"
//	  <mean-delta> <max-delta> <mean-jitter> <max-jitter> [X]
//
// Strip the "(X.X%)" token first, then everything after "lost" is purely
// numeric (5–8 fields) and we can key off the count.
func parseStreamLine(f []string) *Stream {
	if len(f) < 10 {
		return nil
	}
	// "X" in the last column is tshark's own "this stream had problems"
	// flag — same one their GUI paints red.
	problems := false
	if f[len(f)-1] == "X" {
		problems = true
		f = f[:len(f)-1]
	}

	// Drop the parenthesised loss-percentage token (e.g. "(0.4%)"). It's
	// redundant — we recompute LossPercent from Packets+Lost ourselves —
	// and its presence would shift every downstream numeric column by one.
	// Preserve everything else.
	cleaned := make([]string, 0, len(f))
	for _, t := range f {
		if strings.HasPrefix(t, "(") && strings.HasSuffix(t, "%)") {
			continue
		}
		cleaned = append(cleaned, t)
	}
	f = cleaned
	if len(f) < 10 {
		return nil
	}

	s := &Stream{
		SrcAddr: f[2] + ":" + f[3],
		DstAddr: f[4] + ":" + f[5],
		SSRC:    f[6],
	}

	// Payload label may span 1–4 whitespace tokens ("PCMU" · "g711U" ·
	// "ITU-T G.711 PCMU"). The tail is 5–8 purely-numeric columns:
	//   8: pkts lost min-delta mean-delta max-delta min-jitter mean-jitter max-jitter
	//   6: pkts lost mean-delta max-delta mean-jitter max-jitter
	//   5: pkts lost max-delta mean-jitter max-jitter
	//
	// Find the split by walking from the right, counting how many trailing
	// tokens are numeric. Everything before that is payload.
	parseNum := func(x string) (float64, bool) {
		x = strings.TrimSpace(x)
		x = strings.TrimSuffix(x, "ms")
		v, err := strconv.ParseFloat(x, 64)
		return v, err == nil
	}
	numericTail := 0
	for i := len(f) - 1; i >= 7; i-- {
		if _, ok := parseNum(f[i]); !ok {
			break
		}
		numericTail++
	}

	var n int
	switch {
	case numericTail >= 8:
		n = 8
	case numericTail >= 6:
		n = 6
	case numericTail >= 5:
		n = 5
	default:
		return nil
	}
	nums := make([]float64, n)
	base := len(f) - n
	for i := 0; i < n; i++ {
		nums[i], _ = parseNum(f[base+i])
	}
	s.PayloadType = strings.Join(f[7:base], " ")
	switch n {
	case 8: // pkts lost mind meand maxd minj meanj maxj
		s.Packets = int(nums[0])
		s.Lost = int(nums[1])
		s.MaxDeltaMs = nums[4]
		s.MeanJitterMs = nums[6]
		s.MaxJitterMs = nums[7]
	case 6: // pkts lost meand maxd meanj maxj
		s.Packets = int(nums[0])
		s.Lost = int(nums[1])
		s.MaxDeltaMs = nums[3]
		s.MeanJitterMs = nums[4]
		s.MaxJitterMs = nums[5]
	case 5: // pkts lost maxd meanj maxj
		s.Packets = int(nums[0])
		s.Lost = int(nums[1])
		s.MaxDeltaMs = nums[2]
		s.MeanJitterMs = nums[3]
		s.MaxJitterMs = nums[4]
	}

	if s.Packets+s.Lost > 0 {
		s.LossPercent = float64(s.Lost) / float64(s.Packets+s.Lost) * 100.0
	}
	s.Problems = problems
	classifyPayload(s)
	return s
}

// classifyPayload decides whether the stream's jitter/max-delta numbers are
// trustworthy. Two cases zero-rated for verdict purposes:
//
//  1. Comfort Noise (RFC 3389, PT=13, appears as "CN" in the payload
//     column). CN packets are only sent during silence periods and arrive
//     at irregular intervals by design. tshark's jitter formula assumes
//     fixed-rate arrival, so the mixing of CN and audio packets produces
//     multi-minute nonsense jitter and 200-800ms fake "max gaps" that are
//     really just silence.
//  2. Payload transitions mid-stream (comma-separated payload names such
//     as "g711U, g711A"). Each codec has its own RTP timestamp base;
//     tshark keeps accumulating deltas across the transition, so jitter
//     appears to spike enormously at the switch.
//
// Silence-suppression bursts also inflate MaxDeltaMs beyond any real
// concern, so we clear that field too when Timing is unreliable.
func classifyPayload(s *Stream) {
	p := strings.ToUpper(strings.ReplaceAll(s.PayloadType, " ", ""))
	// Comma-separated payload names (codec change mid-stream).
	if strings.Contains(p, ",") {
		names := strings.Split(p, ",")
		distinct := map[string]bool{}
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n != "" {
				distinct[n] = true
			}
		}
		if len(distinct) > 1 {
			// Only unreliable when the codecs actually differ. CN mixed with
			// audio is the common trigger and is caught below anyway.
			hasCN := false
			for n := range distinct {
				if n == "CN" || strings.HasSuffix(n, "-CN") {
					hasCN = true
				}
			}
			if hasCN {
				s.TimingUnreliable = true
				s.TimingNote = "Comfort Noise (silence suppression) mixed with audio — jitter and max-gap numbers are not comparable across the CN transitions."
			} else {
				s.TimingUnreliable = true
				s.TimingNote = "Codec change mid-stream (" + s.PayloadType + ") — tshark's jitter accumulator resets across the transition, so the reported values overstate the actual variance."
			}
		}
	}
	// Standalone CN payload — no audio at all. Not really a media stream.
	if p == "CN" {
		s.TimingUnreliable = true
		s.TimingNote = "Standalone comfort-noise stream (no audio)."
	}
	// Sanity ceiling: real jitter for a 30-min call can't exceed a few
	// hundred ms without the call being unusable. Anything past 500ms mean
	// or 1000ms max is a tshark accumulator artefact; ignore it.
	if s.MeanJitterMs > 500 || s.MaxJitterMs > 1000 {
		s.TimingUnreliable = true
		if s.TimingNote == "" {
			s.TimingNote = "Jitter values above 500ms are almost always a tshark accumulator artefact (payload transition or SSRC wrap), not a real network condition."
		}
	}
}

// deriveVerdict rolls per-stream stats into an overall Level and a bulleted
// issue list. Thresholds are documented at the top of the file.
func deriveVerdict(streams []Stream) Verdict {
	v := Verdict{Level: "unknown"}
	if len(streams) == 0 {
		v.Level = "unknown"
		v.Summary = "No RTP media captured for this call — either the audio port range isn't in the sip-capture filter (see docs), or this call never carried media (signalling-only failure, no answer, etc)."
		return v
	}
	// Level ordering, worst wins.
	rank := map[string]int{"good": 1, "acceptable": 2, "poor": 3, "bad": 4}
	set := func(name string) {
		if rank[name] > rank[v.Level] {
			v.Level = name
		}
	}
	set("good")

	label := func(s Stream) string {
		d := ""
		switch s.Direction {
		case "in":
			d = " (inbound)"
		case "out":
			d = " (outbound)"
		}
		return s.SrcAddr + " → " + s.DstAddr + d
	}

	sawUnreliable := false
	for _, s := range streams {
		// Packet loss is always a valid signal, regardless of payload type.
		switch {
		case s.LossPercent >= 5:
			set("bad")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.1f%% packet loss (%d of %d) — heavy loss, audible as clicks, dropouts, robotic voice.",
				label(s), s.LossPercent, s.Lost, s.Packets+s.Lost))
		case s.LossPercent >= 1:
			set("poor")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.1f%% packet loss (%d of %d) — usually audible on unvoiced sounds ('s', 'f').",
				label(s), s.LossPercent, s.Lost, s.Packets+s.Lost))
		case s.LossPercent > 0:
			set("acceptable")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.2f%% packet loss (%d/%d) — negligible.",
				label(s), s.LossPercent, s.Lost, s.Packets+s.Lost))
		}

		// Skip jitter / max-delta for streams where tshark's numbers can't be
		// trusted (comfort noise, codec transition, nonsense-magnitude jitter).
		if s.TimingUnreliable {
			sawUnreliable = true
			continue
		}

		// Max inter-arrival delta. Jitter buffers usually hold 60–100ms; past
		// that packets get dropped and the buffer refills → audible.
		switch {
		case s.MaxDeltaMs >= 200:
			set("bad")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.0fms max gap between packets — long pause or burst arrival, will drop from any jitter buffer.",
				label(s), s.MaxDeltaMs))
		case s.MaxDeltaMs >= 100:
			set("poor")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.0fms max gap — beyond typical 60ms jitter buffer, likely audible.",
				label(s), s.MaxDeltaMs))
		case s.MaxDeltaMs >= 60:
			set("acceptable")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.0fms max gap — occasionally audible.",
				label(s), s.MaxDeltaMs))
		}

		// Mean jitter — the classic "buffering" tell.
		switch {
		case s.MeanJitterMs >= 50:
			set("bad")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.1fms mean jitter — network path is highly variable, jitter buffer refills constantly (classic 'buffering' symptom).",
				label(s), s.MeanJitterMs))
		case s.MeanJitterMs >= 30:
			set("poor")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.1fms mean jitter — noticeable audio variance.",
				label(s), s.MeanJitterMs))
		case s.MeanJitterMs >= 20:
			set("acceptable")
			v.Issues = append(v.Issues, fmt.Sprintf(
				"%s: %.1fms mean jitter — borderline.",
				label(s), s.MeanJitterMs))
		}
	}
	if sawUnreliable {
		v.Issues = append(v.Issues,
			"Note: one or more streams contained Comfort Noise (RFC 3389) or a codec change mid-call. Their jitter / max-gap numbers are inflated by design and were excluded from the verdict — see the per-stream table for the raw values.")
	}

	// One-way audio detection.
	var hasIn, hasOut bool
	for _, s := range streams {
		switch s.Direction {
		case "in":
			hasIn = true
		case "out":
			hasOut = true
		}
	}
	if hasIn != hasOut {
		set("poor")
		which := "outbound"
		if hasIn {
			which = "inbound"
		}
		v.Issues = append(v.Issues, fmt.Sprintf(
			"Only %s RTP was seen — likely one-way audio, or the missing side's RTP port range isn't in the sip-capture filter.",
			which))
	}

	switch v.Level {
	case "good":
		v.Summary = fmt.Sprintf(
			"Media looked clean across %d stream(s). No loss / jitter / gap thresholds crossed.",
			len(streams))
	case "acceptable":
		v.Summary = "Media had minor variance. Rare artefacts possible; a typical listener wouldn't notice."
	case "poor":
		v.Summary = "Media had measurable quality issues — audible clicks, short dropouts, or the start of buffering symptoms."
	case "bad":
		v.Summary = "Media quality was clearly bad. Buffering, robotic audio, or dropouts will have been obvious to the caller."
	}
	return v
}
