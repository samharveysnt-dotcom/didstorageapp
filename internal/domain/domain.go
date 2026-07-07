// Package domain holds the canonical types and small pure helpers the rest of
// the system shares. Anything that isn't a pure type or pure function belongs
// in a more specific package (billing, sipctl, httpapi, etc.).
package domain

import (
	"math"
	"strings"
	"time"
)

// CallState classifies a CDR into a one-word outcome an operator can scan.
//
//	"answered"   billsec > 0, completed normally
//	"busy"       cause 17
//	"no_answer"  cause 18 / 19
//	"rejected"   cause 21
//	"no_route"   cause 3 / 28 / 88 — couldn't reach destination at all
//	"failed"     anything else with billsec == 0
func CallState(billsec int, hangupCause string) string {
	if billsec > 0 && (hangupCause == "16" || hangupCause == "" || hangupCause == "normal" || hangupCause == "normal-recovered") {
		return "answered"
	}
	switch hangupCause {
	case "17":
		return "busy"
	case "18", "19":
		return "no_answer"
	case "21":
		return "rejected"
	case "3", "28", "88":
		return "no_route"
	}
	if billsec > 0 {
		return "answered"
	}
	return "failed"
}

// NormalizeRouteTarget rewrites the operator's route_target so the dialplan
// always gets a value in the right shape for the route_kind.
//
//	sip_uri     → must be "sip:user@host[:port]" — prepend "sip:" if missing
//	ip          → must be plain "ip[:port]"     — strip any "sip:user@" prefix
//	sip_account → must be just the SIP username — strip any scheme/host
func NormalizeRouteTarget(kind, target string) string {
	t := strings.TrimSpace(target)
	switch kind {
	case "sip_uri":
		low := strings.ToLower(t)
		if !strings.HasPrefix(low, "sip:") && !strings.HasPrefix(low, "sips:") {
			t = "sip:" + t
		}
	case "ip":
		if i := strings.LastIndex(t, "@"); i >= 0 {
			t = t[i+1:]
		}
		t = strings.TrimPrefix(strings.TrimPrefix(t, "sips:"), "sip:")
	case "sip_account":
		t = strings.TrimPrefix(strings.TrimPrefix(t, "sips:"), "sip:")
		if i := strings.Index(t, "@"); i >= 0 {
			t = t[:i]
		}
	}
	return strings.TrimSpace(t)
}

type DIDType string

const (
	DIDMobile   DIDType = "mobile"
	DIDNational DIDType = "national"
	DIDLocal    DIDType = "local"
	DIDTollfree DIDType = "tollfree"
)

type RouteKind string

const (
	RouteSIPAccount RouteKind = "sip_account"
	RouteSIPURI     RouteKind = "sip_uri"
	RouteIP         RouteKind = "ip"
)

type AssignmentStatus string

const (
	AssignmentActive    AssignmentStatus = "active"
	AssignmentSuspended AssignmentStatus = "suspended"
	AssignmentCancelled AssignmentStatus = "cancelled"
)

type LedgerKind string

const (
	LedgerTopup       LedgerKind = "topup"
	LedgerNRC         LedgerKind = "nrc"
	LedgerMRC         LedgerKind = "mrc"
	LedgerChannelFee  LedgerKind = "channel_fee"
	LedgerCallCharge  LedgerKind = "call_charge"
	LedgerManualAdj   LedgerKind = "manual_adj"
	LedgerRefund      LedgerKind = "refund"
)

// ChargeForCall returns the customer-side charge for a completed call leg.
// Mirrors SupplierChargeForCall exactly — same min/inc + ceil semantics —
// only it returns (billedSeconds, chargedMinutes, chargeCents) so callers can
// snapshot the actual billed-seconds onto the cdrs row and still surface a
// minute count to legacy templates.
//
// Notation is "<min>/<inc>" seconds:
//
//	60/60 → 60s connection minimum, 60s round-up (legacy default)
//	60/1  → 60s connection minimum, then per-second
//	6/6   → 6s minimum, 6s increments
//	60/6  → 60s minimum, 6s increments
//
// Defaults: any non-positive value → 60.  Examples at 1.5¢/min:
//
//	billsec= 1, 60/60 → billed= 60, charge= ceil( 60/60 * 1.5)= 2¢
//	billsec=61, 60/60 → billed=120, charge= ceil(120/60 * 1.5)= 3¢
//	billsec=61, 60/1  → billed= 61, charge= ceil( 61/60 * 1.5)= 2¢
//	billsec= 7, 6/6   → billed= 12, charge= ceil( 12/60 * 1.5)= 1¢
func ChargeForCall(billsec, billMinSeconds, billIncSeconds int, ratePerMinuteCents float64) (billedSeconds int, chargedMinutes int, chargeCents int) {
	if billsec <= 0 {
		return 0, 0, 0
	}
	if billMinSeconds <= 0 {
		billMinSeconds = 60
	}
	if billIncSeconds <= 0 {
		billIncSeconds = 60
	}
	billed := billsec
	if billed < billMinSeconds {
		billed = billMinSeconds
	} else if rem := billed % billIncSeconds; rem != 0 {
		billed += billIncSeconds - rem
	}
	perSec := ratePerMinuteCents / 60.0
	chargeCents = int(math.Ceil(float64(billed) * perSec))
	chargedMinutes = int(math.Ceil(float64(billed) / 60.0))
	billedSeconds = billed
	return
}

// SupplierChargeForCall computes the supplier-side cost for a call of
// `billsec` seconds at `perMinCents` cents/minute, with the supplier's
// billing increment ("min/inc"). Examples for a 75s call at 0.4¢/min:
//
//	1/1   → bill 75s   = 75/60 * 0.4 = 0.50 ¢ → ceil → 1 ¢
//	6/6   → bill 78s   = 78/60 * 0.4 = 0.52 ¢ → ceil → 1 ¢
//	60/60 → bill 120s  = 2/1  * 0.4  = 0.80 ¢ → ceil → 1 ¢
//
// Defaults: blank min/inc → 60/60. Round each step UP — both the time and
// the cent value — matching how every operator we've ever seen does it.
func SupplierChargeForCall(billsec, billMinSeconds, billIncSeconds int, perMinCents float64) int {
	if billsec <= 0 || perMinCents <= 0 {
		return 0
	}
	if billMinSeconds <= 0 {
		billMinSeconds = 60
	}
	if billIncSeconds <= 0 {
		billIncSeconds = 60
	}
	billed := billsec
	if billed < billMinSeconds {
		billed = billMinSeconds
	} else if rem := billed % billIncSeconds; rem != 0 {
		billed += billIncSeconds - rem
	}
	perSec := perMinCents / 60.0
	return int(math.Ceil(float64(billed) * perSec))
}

// FlagEmoji returns the unicode regional-indicator pair for a 2-letter
// ISO-3166 alpha-2 country code (e.g. "GB" → 🇬🇧). Returns "" for invalid
// input so the caller can render a placeholder.
func FlagEmoji(iso string) string {
	if len(iso) != 2 {
		return ""
	}
	a := byte(strings.ToUpper(iso)[0])
	b := byte(strings.ToUpper(iso)[1])
	if a < 'A' || a > 'Z' || b < 'A' || b > 'Z' {
		return ""
	}
	return string([]rune{0x1F1E6 + rune(a-'A'), 0x1F1E6 + rune(b-'A')})
}

// MaxSecondsForBalance returns the largest call duration we can authorize given
// the user's balance and the per-minute rate. Used to arm Kamailio's dialog
// timer for the hard mid-call cutoff.
//
// We're conservative: we floor to whole minutes the balance can cover, then
// convert to seconds. A user with 5¢ at 1.5¢/min gets 3 full minutes = 180s
// authorized (NOT 200s, even though 5/1.5 ≈ 3.33).
func MaxSecondsForBalance(balanceCents int64, ratePerMinuteCents float64) int {
	if ratePerMinuteCents <= 0 {
		// If the rate is zero (e.g. promotional), allow a generous default cap.
		return 4 * 60 * 60 // 4 hours
	}
	wholeMinutes := math.Floor(float64(balanceCents) / ratePerMinuteCents)
	if wholeMinutes < 0 {
		return 0
	}
	return int(wholeMinutes) * 60
}

// CleanCallerURI returns a presentation-friendly form of a SIP caller / called
// URI value. It handles the messy stew that SIP-derived CDR fields throw
// at us:
//
//	"Display Name" <sip:user@host:port>      → user@host
//	<sip:447956…@1.2.3.4>                    → 447956…@1.2.3.4
//	<447956816884>                           → 447956816884
//	sip:bob@example.com;tag=abc              → bob@example.com
//	447956816884                              → 447956816884   (no-op)
//
// We deliberately keep the host part (when present) — it disambiguates a
// caller "100" from peer A vs peer B in a CDR list. Strip it client-side if
// the consumer wants just the digit string.
func CleanCallerURI(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 1) Pull a quoted display-name off the front: "Foo Bar" <…>
	if strings.HasPrefix(s, `"`) {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			s = strings.TrimSpace(s[end+2:])
		}
	}
	// 2) If the URI is angle-quoted, keep just the inside.
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s, ">"); j > i {
			s = s[i+1 : j]
		}
	}
	// 3) Strip scheme.
	low := strings.ToLower(s)
	switch {
	case strings.HasPrefix(low, "sips:"):
		s = s[5:]
	case strings.HasPrefix(low, "sip:"):
		s = s[4:]
	case strings.HasPrefix(low, "tel:"):
		s = s[4:]
	}
	// 4) Drop any URI parameters (after ;).
	if i := strings.Index(s, ";"); i >= 0 {
		s = s[:i]
	}
	// 5) Drop a trailing port from the host part.
	//    user@1.2.3.4:5060 → user@1.2.3.4
	if at := strings.Index(s, "@"); at >= 0 {
		host := s[at+1:]
		if colon := strings.LastIndex(host, ":"); colon >= 0 {
			// Only strip if what follows is all-numeric (a port, not part
			// of an IPv6 address).
			port := host[colon+1:]
			if port != "" && allDigits(port) {
				s = s[:at+1] + host[:colon]
			}
		}
	}
	return strings.TrimSpace(s)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// SanitizeCallID strips the "@host[:port]" suffix Asterisk and many
// SIP stacks append to the locally-generated random part of a Call-ID. We
// store the prefix-only form so a customer-visible CDR doesn't leak our
// supplier's IP address.
//
// Examples:
//
//	286afc1104a6bf79565ebaad44672495@217.73.68.39:5060  → 286afc1104a6bf79565ebaad44672495
//	abcd-1234@1.2.3.4                                    → abcd-1234
//	already-clean                                        → already-clean
func SanitizeCallID(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// NextAnniversary returns the next billing date (UTC) for an assignment whose
// anniversary day is `day` (1..28), starting from `from`. If `from` is on or
// after this month's anniversary, the next one is in the following month.
func NextAnniversary(from time.Time, day int) time.Time {
	if day < 1 {
		day = 1
	}
	if day > 28 {
		day = 28
	}
	from = from.UTC()
	candidate := time.Date(from.Year(), from.Month(), day, 0, 0, 0, 0, time.UTC)
	if !candidate.After(from) {
		candidate = candidate.AddDate(0, 1, 0)
	}
	return candidate
}
