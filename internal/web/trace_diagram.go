package web

import (
	"fmt"
	"html/template"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"didstorage/internal/causes"
	"didstorage/internal/siptrace"
)

// SequenceDiagram is the data shape handed to the template to render the SVG
// ladder. Lanes are vertical lifelines (one per distinct SIP endpoint) and
// Arrows are horizontal hops between them at increasing y. Phases drive the
// background colour-bands behind groups of arrows so the eye can pick up the
// call's structural sections (setup / ringback / talk / teardown).
//
// AdminMarkers are horizontal divider lines stamped onto the ladder at the
// wall-clock moment an admin used /live to hangup / warn / redirect the call.
// They're positioned right after the SIP packet whose UnixTime is closest to
// the action's timestamp, so the eye can read "this packet just happened,
// then ADMIN HANGUP" without having to cross-reference timestamps manually.
type SequenceDiagram struct {
	Width        int
	Height       int
	Lanes        []sdLane
	Arrows       []sdArrow
	Phases       []sdPhase
	AdminMarkers []sdAdminMarker
	HasData      bool
}

type sdLane struct {
	X           int    // x-center of the lifeline
	Label       string // display label (already sanitized for the viewer)
	Kind        string // "ours" / "supplier" / "peer" — drives lane colour
	PacketCount int    // number of messages this endpoint participated in
}

type sdArrow struct {
	Index        int    // 1-based packet number (matches Trace.Messages[Index-1])
	X1, X2       int    // start/end x (lane centers)
	Y            int    // baseline y (post-variable-height layout)
	Label        string // method or status line
	Class        string // "request" / "response-2xx" / "response-4xx" / etc.
	Time         string // "13:30:16.456"
	DeltaMs      int    // ms elapsed since the previous packet (0 for first)
	DeltaLabel   string // "+0.123s" / "+5ms" / "" for first
	Reverse      bool   // true if drawn right→left (used for marker positioning)
	IsRetransmit bool   // same CSeq+line as a prior packet
	RetransmitN  int    // number of times this CSeq+line seen so far (2 = first dup)
	Phase        string // "setup" | "ringback" | "talk" | "teardown" | "other"
}

// sdPhase is a horizontal background band spanning a range of rows. We render
// one rect per phase before the arrows so the bg colour reads through any
// gaps between arrows.
type sdPhase struct {
	Kind  string // setup / ringback / talk / teardown
	Y     int    // top y
	H     int    // height
	Label string // shown on the right edge ("setup · 1.2s", etc.)
}

// sdAdminMarker is a horizontal divider on the ladder marking an admin live-
// action (Hangup/Warn/Redirect). The class drives the colour; Title goes in
// the badge ("ADMIN HANGUP"); AdminEmail + Reason + Time fill the tooltip
// content. Y is positioned just below the SIP packet whose timestamp is
// closest to the action's wall-clock — visually reads as "after this packet,
// the admin did X".
type sdAdminMarker struct {
	Y          int
	Class      string // "hangup" | "warn" | "redirect"
	Title      string // "ADMIN HANGUP"
	AdminEmail string
	Reason     string
	Time       string // HH:MM:SS local
	AfterArrow int    // 1-based arrow index this marker comes after (display label)
	BadgeW     int    // pre-computed badge width so the template avoids math
	BadgeX     int    // pre-computed left-x for badge (centered)
}

// AdminActionEvent is what the trace handler queries out of user_block_log for
// events scoped to this call's order in the call's time window. We pass these
// in to BuildSequenceDiagram so marker layout can find the closest arrow.
type AdminActionEvent struct {
	Action     string // "live_hangup" | "live_warn" | "live_redirect"
	AdminEmail string
	Reason     string
	UnixTime   float64
	TimeLabel  string // pre-formatted HH:MM:SS
}

// Finding is one item shown in the auto-diagnosis banner. Severity is one of
// "warn" / "err" / "info". RefMessages contains 1-based packet indexes the
// finding refers to — the template makes them clickable so the user lands
// directly on the offending arrow.
type Finding struct {
	Severity    string
	Title       string
	Detail      string
	RefMessages []int
}

// BuildSequenceDiagram derives a SequenceDiagram from a trace. Returns
// HasData=false if the trace has fewer than two messages or no endpoints —
// the template uses that to fall back to a "no diagram available" message.
//
// adminEvents are optional — admin live-actions (hangup/warn/redirect) for
// this call, used to stamp marker rows onto the ladder at the wall-clock
// moment each action occurred. Pass nil if you don't have them; the diagram
// renders without markers.
func BuildSequenceDiagram(tr *siptrace.Trace, ourPublicIP string, supplierIPs map[string]bool, adminEvents []AdminActionEvent) SequenceDiagram {
	if tr == nil || len(tr.Messages) == 0 || len(tr.Endpoints) == 0 {
		return SequenceDiagram{}
	}

	type laneInfo struct {
		addr string
		kind string
		bias int
	}
	var infos []laneInfo
	for _, addr := range tr.Endpoints {
		ip := stripPort(addr)
		kind := "peer"
		bias := 5
		switch {
		case ip == ourPublicIP || ipPrivate(ip):
			kind = "ours"
			bias = 3
		case supplierIPs[ip]:
			kind = "supplier"
			bias = 1
		}
		infos = append(infos, laneInfo{addr, kind, bias})
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].bias < infos[j].bias })

	// Per-lane participation count. Done before arrow layout so we can stamp
	// the count onto the lane card in one pass.
	laneCounts := map[string]int{}
	for _, m := range tr.Messages {
		laneCounts[m.SrcAddr]++
		laneCounts[m.DstAddr]++
	}

	const (
		laneSpacing   = 280
		laneFirstX    = 200
		lifelineTop   = 110 // taller — endpoint cards now ~80px tall
		rowFirstY     = 138
		rowHeightMin  = 38
		rowHeightMax  = 110
		bottomPad     = 28
		minWidth      = 760
	)
	indexOf := map[string]int{}
	var lanes []sdLane
	for i, info := range infos {
		x := laneFirstX + i*laneSpacing
		lanes = append(lanes, sdLane{
			X: x, Label: info.addr, Kind: info.kind,
			PacketCount: laneCounts[info.addr],
		})
		indexOf[info.addr] = i
	}

	// Retransmit detection: key on (CSeq method + first-line class). We don't
	// have full headers per message, but tshark gives us SrcAddr+DstAddr+
	// Summary; pairing those is a good-enough heuristic for retransmits over
	// the wire. (Real production SIP retransmits keep the same Via branch +
	// CSeq, which produces identical Summary lines on the same socket pair.)
	seen := map[string]int{}
	retransmitKey := func(m siptrace.Message) string {
		return m.SrcAddr + "|" + m.DstAddr + "|" + shortenSummary(m.Summary)
	}

	// First pass — collect (msgIdx, fromIdx, toIdx) for every non-self-loop
	// message. We need this list to compute deltas against the previous
	// rendered arrow (not the previous Message — self-loops are skipped).
	var msgIdxs []int
	var fromIdxs, toIdxs []int
	for i, m := range tr.Messages {
		fromIdx, ok1 := indexOf[m.SrcAddr]
		toIdx, ok2 := indexOf[m.DstAddr]
		if !ok1 || !ok2 || fromIdx == toIdx {
			continue
		}
		msgIdxs = append(msgIdxs, i)
		fromIdxs = append(fromIdxs, fromIdx)
		toIdxs = append(toIdxs, toIdx)
	}

	// Second pass — compute Y with log-scaled gaps between adjacent arrows.
	// Goal: short bursts of activity (<100ms) sit tight (min height); silent
	// stretches (1s+) get visually big rows. Bounded so a 5-minute hold ring
	// doesn't blow the SVG to 10000px.
	var arrows []sdArrow
	y := rowFirstY
	var prevUnix float64
	for i, mi := range msgIdxs {
		m := tr.Messages[mi]
		deltaMs := 0
		deltaLabel := ""
		if i > 0 {
			gap := m.UnixTime - prevUnix
			if gap < 0 {
				gap = 0
			}
			deltaMs = int(gap * 1000)
			deltaLabel = formatDelta(gap)
			scaled := rowHeightMin + int(math.Log10(float64(deltaMs+1))*22)
			if scaled < rowHeightMin {
				scaled = rowHeightMin
			}
			if scaled > rowHeightMax {
				scaled = rowHeightMax
			}
			y += scaled
		}
		prevUnix = m.UnixTime
		key := retransmitKey(m)
		seen[key]++
		isRet := seen[key] > 1
		arrows = append(arrows, sdArrow{
			Index:        mi + 1,
			X1:           lanes[fromIdxs[i]].X,
			X2:           lanes[toIdxs[i]].X,
			Y:            y,
			Label:        shortenSummary(m.Summary),
			Class:        classifyMessage(m.Summary),
			Time:         timeShort(m.Time),
			DeltaMs:      deltaMs,
			DeltaLabel:   deltaLabel,
			Reverse:      lanes[toIdxs[i]].X < lanes[fromIdxs[i]].X,
			IsRetransmit: isRet,
			RetransmitN:  seen[key],
		})
	}

	classifyPhases(arrows, tr.Messages, msgIdxs)
	phases := buildPhaseBands(arrows)

	width := laneFirstX + (len(lanes)-1)*laneSpacing + 140
	if width < minWidth {
		width = minWidth
	}
	markers := buildAdminMarkers(arrows, tr.Messages, msgIdxs, adminEvents, width)
	height := lifelineTop + 20 + bottomPad
	if len(arrows) > 0 {
		height = arrows[len(arrows)-1].Y + rowHeightMin + bottomPad
	}
	// If a marker sits below the last arrow (admin action after the call's
	// final captured packet), extend the canvas so it doesn't get clipped.
	for _, mk := range markers {
		if mk.Y+30 > height {
			height = mk.Y + 30
		}
	}

	return SequenceDiagram{
		Width:        width,
		Height:       height,
		Lanes:        lanes,
		Arrows:       arrows,
		Phases:       phases,
		AdminMarkers: markers,
		HasData:      true,
	}
}

// buildAdminMarkers maps each admin event to the closest SIP packet by
// timestamp and produces a horizontal marker positioned just below that
// packet's Y. The marker reads "after this packet, admin did X".
//
// Events that happened before the first packet sit above row 1; events after
// the last packet sit below the last arrow (the canvas height is extended in
// the caller to keep them inside the SVG).
func buildAdminMarkers(arrows []sdArrow, msgs []siptrace.Message, msgIdxs []int, events []AdminActionEvent, diagramWidth int) []sdAdminMarker {
	if len(events) == 0 || len(arrows) == 0 {
		return nil
	}
	out := make([]sdAdminMarker, 0, len(events))
	for _, ev := range events {
		// Walk arrows in order; the LAST arrow whose timestamp <= the
		// event's timestamp is what the marker sits after. If the event
		// predates every packet, AfterArrow=0 and Y sits at the top.
		afterIdx := -1
		for i, mi := range msgIdxs {
			if msgs[mi].UnixTime <= ev.UnixTime {
				afterIdx = i
			} else {
				break
			}
		}
		var y int
		if afterIdx < 0 {
			y = arrows[0].Y - 24
		} else {
			y = arrows[afterIdx].Y + 24
		}
		title := adminActionTitle(ev.Action)
		// Badge width — roughly 7px per title char + reserved space for the
		// right-aligned email/time. Floor at 320 so short labels still read
		// well; cap to leave 40px gutter on each side of the diagram.
		badgeW := len(title)*7 + 220
		if badgeW < 320 {
			badgeW = 320
		}
		if max := diagramWidth - 80; badgeW > max {
			badgeW = max
		}
		badgeX := (diagramWidth - badgeW) / 2
		if badgeX < 40 {
			badgeX = 40
		}
		out = append(out, sdAdminMarker{
			Y:          y,
			Class:      classifyAdminAction(ev.Action),
			Title:      title,
			AdminEmail: ev.AdminEmail,
			Reason:     ev.Reason,
			Time:       ev.TimeLabel,
			AfterArrow: afterIdx + 1,
			BadgeW:     badgeW,
			BadgeX:     badgeX,
		})
	}
	return out
}

func classifyAdminAction(a string) string {
	switch a {
	case "live_hangup":
		return "hangup"
	case "live_warn":
		return "warn"
	case "live_redirect":
		return "redirect"
	}
	return "other"
}

func adminActionTitle(a string) string {
	switch a {
	case "live_hangup":
		return "ADMIN HANGUP"
	case "live_warn":
		return "ADMIN WARN (prompt + hangup)"
	case "live_redirect":
		return "ADMIN REDIRECT"
	}
	return strings.ToUpper(a)
}

// classifyPhases walks the arrow list and labels each with the call-phase it
// belongs to. State machine: setup → ringback (on 1xx) → talk (on 2xx for
// INVITE) → teardown (on BYE) → other (after teardown 200). It's intentionally
// forgiving — a missing 200 OK means we never enter "talk"; missing BYE means
// we never enter "teardown".
func classifyPhases(arrows []sdArrow, msgs []siptrace.Message, msgIdxs []int) {
	state := "setup"
	for i := range arrows {
		m := msgs[msgIdxs[i]]
		summary := m.Summary
		// State transitions read off the first-line summary.
		switch {
		case state == "setup" && strings.HasPrefix(summary, "SIP/") && firstCode(summary) >= 100 && firstCode(summary) < 200:
			state = "ringback"
		case (state == "setup" || state == "ringback") && strings.HasPrefix(summary, "SIP/") && firstCode(summary) >= 200 && firstCode(summary) < 300:
			state = "talk"
		case state == "talk" && (strings.HasPrefix(summary, "BYE ") || strings.HasPrefix(summary, "CANCEL ")):
			state = "teardown"
		}
		arrows[i].Phase = state
		// Apply the state AFTER classifying, so the line that triggered the
		// transition (e.g. the 180) belongs to the phase it ended (setup).
		// Re-set for next iteration only if the message itself was the
		// trigger.
		switch {
		case strings.HasPrefix(summary, "SIP/") && firstCode(summary) >= 200 && firstCode(summary) < 300:
			// 200 OK lands in "talk" itself so the green ribbon visibly
			// starts at the 200, not the next packet. Overwrite.
			if state == "talk" && arrows[i].Phase != "teardown" {
				arrows[i].Phase = "talk"
			}
		}
	}
}

func firstCode(summary string) int {
	if !strings.HasPrefix(summary, "SIP/") {
		return 0
	}
	parts := strings.SplitN(summary, " ", 3)
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}

// buildPhaseBands collapses adjacent same-phase arrows into one rectangle for
// the SVG background. We over-extend each band slightly so neighbouring rows
// blend rather than leaving a 1px gap.
func buildPhaseBands(arrows []sdArrow) []sdPhase {
	if len(arrows) == 0 {
		return nil
	}
	var out []sdPhase
	startIdx := 0
	for i := 1; i <= len(arrows); i++ {
		if i == len(arrows) || arrows[i].Phase != arrows[startIdx].Phase {
			top := arrows[startIdx].Y - 22
			var bottom int
			if i == len(arrows) {
				bottom = arrows[i-1].Y + 22
			} else {
				bottom = arrows[i].Y - 22
			}
			out = append(out, sdPhase{
				Kind:  arrows[startIdx].Phase,
				Y:     top,
				H:     bottom - top,
				Label: phaseLabel(arrows[startIdx].Phase, arrows[i-1].Y-arrows[startIdx].Y),
			})
			startIdx = i
		}
	}
	return out
}

func phaseLabel(kind string, _ int) string {
	switch kind {
	case "setup":
		return "Setup"
	case "ringback":
		return "Ringback"
	case "talk":
		return "Talk"
	case "teardown":
		return "Teardown"
	}
	return ""
}

// BuildFindings runs heuristics over the trace and surfaces actionable
// observations in the auto-diagnosis banner. Each finding has a severity and
// optionally references specific 1-based packet indexes that the template
// turns into click-to-locate chips.
//
// Kept intentionally rule-based (no ML, no fuzzy matching): explicit rules
// are easier to extend and debug, and SIP debugging is a domain with
// well-defined "smells".
func BuildFindings(tr *siptrace.Trace, hangupCause string) []Finding {
	var out []Finding
	if tr == nil || len(tr.Messages) == 0 {
		return out
	}

	// --- 1. Retransmit detection ---
	seen := map[string]int{}
	type retEntry struct {
		method string
		count  int
		idxs   []int
	}
	rets := map[string]*retEntry{}
	for i, m := range tr.Messages {
		key := m.SrcAddr + "|" + m.DstAddr + "|" + shortenSummary(m.Summary)
		seen[key]++
		if seen[key] > 1 {
			r := rets[key]
			if r == nil {
				r = &retEntry{method: shortenSummary(m.Summary)}
				rets[key] = r
			}
			r.count++
			r.idxs = append(r.idxs, i+1)
		}
	}
	for _, r := range rets {
		out = append(out, Finding{
			Severity:    "warn",
			Title:       fmt.Sprintf("Retransmit detected — %s seen %d times", r.method, r.count+1),
			Detail:      "Same first-line + endpoints repeated. Usually indicates packet loss or upstream slow to ack. Check network latency between the endpoints.",
			RefMessages: r.idxs,
		})
	}

	// --- 2. Final response heuristics ---
	switch {
	case tr.FinalSIPCode == 0:
		out = append(out, Finding{
			Severity: "warn",
			Title:    "No SIP response captured",
			Detail:   "Request side observed but no provisional or final response. Suggests the upstream never replied — either it never received the INVITE (NAT/firewall) or the pcap rolled before the reply landed.",
		})
	case tr.FinalSIPCode == 200:
		// Find ACK and 200 indexes to measure ACK delay.
		var twoIdx, ackIdx int
		for i, m := range tr.Messages {
			s := strings.TrimSpace(m.Summary)
			if twoIdx == 0 && strings.HasPrefix(s, "SIP/") && firstCode(s) == 200 && strings.Contains(s, "OK") {
				twoIdx = i + 1
			}
			if twoIdx > 0 && ackIdx == 0 && strings.HasPrefix(s, "ACK ") {
				ackIdx = i + 1
				break
			}
		}
		if twoIdx > 0 && ackIdx == 0 {
			out = append(out, Finding{
				Severity:    "err",
				Title:       "No ACK observed after 200 OK",
				Detail:      "The 200 OK answered but no matching ACK followed. Common cause: NAT mapping expired on the caller side, or the ACK was sent to a stale Contact URI.",
				RefMessages: []int{twoIdx},
			})
		} else if twoIdx > 0 && ackIdx > 0 {
			gap := tr.Messages[ackIdx-1].UnixTime - tr.Messages[twoIdx-1].UnixTime
			if gap > 1.0 {
				out = append(out, Finding{
					Severity:    "warn",
					Title:       fmt.Sprintf("ACK arrived %.2fs after 200 OK", gap),
					Detail:      "Expected <500ms in a healthy call. Slow ACK can mean RTT issues or a busy caller endpoint.",
					RefMessages: []int{twoIdx, ackIdx},
				})
			}
		}
	case tr.FinalSIPCode >= 400 && tr.FinalSIPCode < 500:
		out = append(out, Finding{
			Severity: "err",
			Title:    fmt.Sprintf("Call rejected — SIP %d %s", tr.FinalSIPCode, tr.FinalSIPReason),
			Detail:   sipRejectionExplanation(tr.FinalSIPCode),
		})
	case tr.FinalSIPCode >= 500 && tr.FinalSIPCode < 600:
		out = append(out, Finding{
			Severity: "err",
			Title:    fmt.Sprintf("Upstream server error — SIP %d %s", tr.FinalSIPCode, tr.FinalSIPReason),
			Detail:   "5xx responses indicate the upstream endpoint had an internal problem handling the request. Check the supplier or peer's logs / capacity.",
		})
	}

	// --- 3. Platform-attributable denials (cause came from our own /authorize) ---
	if causes.IsPlatform(hangupCause) {
		label, detail := causeDescribe(hangupCause)
		out = append(out, Finding{
			Severity: "err",
			Title:    "Platform denied the call · " + label,
			Detail:   detail,
		})
	}

	// --- 4. Excessive ringing time ---
	var inviteIdx, finalIdx int
	for i, m := range tr.Messages {
		s := strings.TrimSpace(m.Summary)
		if inviteIdx == 0 && strings.HasPrefix(s, "INVITE ") {
			inviteIdx = i + 1
		}
		if inviteIdx > 0 && strings.HasPrefix(s, "SIP/") && firstCode(s) >= 200 {
			finalIdx = i + 1
			break
		}
	}
	if inviteIdx > 0 && finalIdx > 0 {
		ring := tr.Messages[finalIdx-1].UnixTime - tr.Messages[inviteIdx-1].UnixTime
		if ring > 30 {
			out = append(out, Finding{
				Severity:    "warn",
				Title:       fmt.Sprintf("Long ring time — %.1fs from INVITE to final response", ring),
				Detail:      "Most carriers timeout ringback at 30-60s. Long ring suggests the destination endpoint is unreachable but accepting INVITEs.",
				RefMessages: []int{inviteIdx, finalIdx},
			})
		}
	}

	return out
}

func sipRejectionExplanation(code int) string {
	// Short reasons keyed by the most common rejection codes we see in this
	// platform. Generic fallback for codes we haven't catalogued.
	switch code {
	case 403:
		return "Caller not authorized for this DID or supplier IP not allowed. Check supplier_ip_group_members for the source IP."
	case 404:
		return "DID not provisioned on the platform. Confirm the dids row exists with status='assigned' or 'reserved'."
	case 408:
		return "Timeout reaching downstream endpoint. Likely the routed target (sip_uri/ip/sip_account) is offline."
	case 480:
		return "Quarantined or temporarily unavailable. Usually a blocked-user state on this platform; unblock to restore."
	case 486:
		return "Destination busy. The peer/softphone is on another call or rejected via Busy Here."
	case 487:
		return "Request terminated — the caller hung up before the call was answered."
	case 488:
		return "Codec mismatch in SDP offer/answer. Check the supported codec lists on both sides."
	case 491:
		return "Pending transaction conflict. Re-INVITE during another in-flight INVITE."
	case 503:
		return "Upstream service unavailable. Carrier-side capacity, restart, or maintenance window."
	}
	return ""
}

// SplitRawByFrame breaks tshark -V verbose output into one chunk per Frame
// header so the side panel can show "this packet only". Best-effort: returns
// the original whole string as a single chunk if the split doesn't yield a
// count that approximately matches the message list. The template uses the
// chunks via JS lookup by Message index.
func SplitRawByFrame(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	// Re-attach the "Frame " prefix to each chunk after splitting.
	re := regexp.MustCompile(`(?m)^Frame `)
	idxs := re.FindAllStringIndex(raw, -1)
	if len(idxs) == 0 {
		return []string{raw}
	}
	out := make([]string, 0, len(idxs))
	for i, m := range idxs {
		start := m[0]
		end := len(raw)
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		out = append(out, strings.TrimRight(raw[start:end], " \n\r\t"))
	}
	return out
}

func stripPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func ipPrivate(ip string) bool {
	return strings.HasPrefix(ip, "10.") ||
		strings.HasPrefix(ip, "192.168.") ||
		strings.HasPrefix(ip, "172.") ||
		strings.HasPrefix(ip, "127.")
}

func shortenSummary(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "SIP/") {
		parts := strings.SplitN(s, " ", 3)
		if len(parts) >= 2 {
			if len(parts) == 3 {
				return parts[1] + " " + parts[2]
			}
			return parts[1]
		}
		return s
	}
	parts := strings.SplitN(s, " ", 2)
	return parts[0]
}

func classifyMessage(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "SIP/") {
		parts := strings.SplitN(s, " ", 3)
		if len(parts) >= 2 {
			c := parts[1]
			if len(c) > 0 {
				return "response-" + string(c[0]) + "xx"
			}
		}
		return "response"
	}
	return "request"
}

func timeShort(iso string) string {
	if len(iso) < 19 {
		return iso
	}
	t := iso[11:]
	if i := strings.Index(t, "."); i >= 0 && len(t) > i+4 {
		return t[:i+4]
	}
	if i := strings.Index(t, "Z"); i >= 0 {
		return t[:i]
	}
	return t
}

func formatDelta(seconds float64) string {
	if seconds < 0.001 {
		return "+0ms"
	}
	if seconds < 1 {
		return fmt.Sprintf("+%dms", int(seconds*1000))
	}
	return fmt.Sprintf("+%.2fs", seconds)
}

// EndResultLabel returns the human end-state of the call so the trace page can
// show "answered $0.02" / "rejected: insufficient_channels" / "no answer" /
// "no_route 28" / "no SIP packets observed". Pure formatting helper.
func EndResultLabel(billsec int, hangupCause string, finalSIP int, finalReason string, hasMessages bool) template.HTML {
	if !hasMessages && billsec == 0 && hangupCause == "" {
		return template.HTML(`<span class="pill pill-cancelled">no captures</span>`)
	}
	if causes.IsPlatform(hangupCause) {
		label, detail := causeDescribe(hangupCause)
		return template.HTML(
			`<span class="pill pill-suspended">rejected · ` + template.HTMLEscapeString(label) + `</span> ` +
				`<span class="tip" data-tip="` + template.HTMLEscapeString(detail) + `">` + string(iconSVG("info")) + `</span>`,
		)
	}
	if billsec > 0 {
		return template.HTML(`<span class="pill pill-active">answered</span> <span class="muted">` + fmt.Sprintf("%ds", billsec) + `</span>`)
	}
	if hangupCause != "" {
		label, detail := causeDescribe(hangupCause)
		out := `<span class="pill pill-warn">` + template.HTMLEscapeString(label) + `</span>`
		if detail != "" {
			out += ` <span class="tip" data-tip="` + template.HTMLEscapeString(detail) + `">` + string(iconSVG("info")) + `</span>`
		}
		return template.HTML(out)
	}
	if finalSIP > 0 {
		cls := "pill-warn"
		if finalSIP >= 200 && finalSIP < 300 {
			cls = "pill-active"
		} else if finalSIP >= 400 {
			cls = "pill-suspended"
		}
		return template.HTML(`<span class="pill ` + cls + `">SIP ` + fmt.Sprintf("%d %s", finalSIP, template.HTMLEscapeString(finalReason)) + `</span>`)
	}
	return template.HTML(`<span class="pill pill-cancelled">unknown</span>`)
}
