// Package siptrace looks up the SIP messages for a given Call-ID by greping
// the rolling pcap files captured by the sip-capture systemd service. Used by
// both the admin GUI (/cdrs/.../sip-trace) and the reseller API
// (/api/v1/cdrs/{call_id}/sip-trace).
//
// Two call-id forms can be supplied:
//   - Sanitized prefix     (e.g. "286afc1104a6bf79565ebaad44672495")
//   - Full Asterisk form   (e.g. "286afc1104a6bf79565ebaad44672495@1.2.3.4:5060")
//
// We strip the @host part and use a substring match (`sip.Call-ID contains
// "prefix"`). The 32-char hex prefix is unique enough that false positives are
// vanishingly unlikely, and `contains` is much faster than a `matches` regex.
//
// Performance notes
// ─────────────────
// PcapDir holds rolling daily captures that can run multi-GB. Naively running
// `tshark -Y filter` across every file is dominated by linear scans of pcaps
// that don't contain the call. We do two things to keep lookups under a
// second on a busy server:
//
//  1. Pre-filter with `grep -F` — UDP SIP is plaintext on the wire so the
//     Call-ID hex appears verbatim in the pcap. `grep -l` rules out files
//     that don't contain it before tshark touches them.
//  2. Run tshark in parallel against the (usually one) matching pcap, with a
//     bounded concurrency so a multi-call lookup can't fork-bomb the box.
package siptrace

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"didstorage/internal/domain"
)

const PcapDir = "/var/lib/didstorage/sip-traces"

// Concurrency cap for tshark + grep workers. Empirically 4 keeps a 4-core
// box pegged without thrashing.
const maxParallel = 4

// cacheTTL is how long we keep a parsed Trace in memory. Once a pcap rolls
// out of retention the call's packets disappear, so caching for the same
// duration as pcap retention (7d) is the natural ceiling. We keep entries
// shorter (24h) so a fresh tshark re-runs occasionally and we pick up any
// late-arriving packets that drained from the kernel-side capture buffer
// after the initial parse.
const cacheTTL = 24 * time.Hour

type cacheEntry struct {
	trace *Trace
	at    time.Time
}

var (
	cacheMu sync.RWMutex
	cache   = map[string]cacheEntry{}
)

func cacheGet(key string) *Trace {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	e, ok := cache[key]
	if !ok || time.Since(e.at) > cacheTTL {
		return nil
	}
	// Return a clone — Sanitize() mutates in place and the same cached
	// trace is reused by both admin (raw) and reseller (rewritten) paths.
	return cloneTrace(e.trace)
}

func cloneTrace(t *Trace) *Trace {
	if t == nil {
		return nil
	}
	out := &Trace{
		CallID:    t.CallID,
		Raw:       t.Raw,
		PcapFiles: append([]string(nil), t.PcapFiles...),
		Messages:  append([]Message(nil), t.Messages...),
	}
	return out
}

func cachePut(key string, tr *Trace) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache[key] = cacheEntry{trace: tr, at: time.Now()}
	// Cheap purge: if the map is getting big, drop expired entries.
	if len(cache) > 256 {
		cutoff := time.Now().Add(-cacheTTL)
		for k, e := range cache {
			if e.at.Before(cutoff) {
				delete(cache, k)
			}
		}
	}
}

// Message is one SIP packet observed on the wire.
type Message struct {
	UnixTime  float64 `json:"unix_time"`
	Time      string  `json:"time"` // ISO-8601 UTC
	Direction string  `json:"direction"` // "in" / "out" relative to ourPublicIP
	SrcAddr   string  `json:"src_addr"`  // ip:port (or sanitized label)
	DstAddr   string  `json:"dst_addr"`  // ip:port (or sanitized label)
	Summary   string  `json:"summary"`   // first line of the SIP message
}

// Trace is the full set of SIP messages for a Call-ID, plus a raw tshark dump
// for human inspection. Endpoints + final response are computed from the
// message list so consumers (admin GUI, reseller API) can render an end-result
// label or a sequence diagram without parsing tshark output themselves.
type Trace struct {
	CallID    string    `json:"call_id"`
	Messages  []Message `json:"messages"`
	Raw       string    `json:"raw"`
	PcapFiles []string  `json:"pcap_files,omitempty"`

	// Endpoints lists distinct ip:port pairs seen on the wire, in
	// first-appearance order. Resellers consume the same shape.
	Endpoints []string `json:"endpoints"`

	// FinalSIPCode and FinalSIPReason are the highest-precedence final
	// response observed (200 trumps 4xx trumps no-response). Empty if no
	// response was captured.
	FinalSIPCode   int    `json:"final_sip_code,omitempty"`
	FinalSIPReason string `json:"final_sip_reason,omitempty"`

	// MethodCounts maps SIP method or response class -> count, useful for the
	// overview "what happened" cards.
	MethodCounts map[string]int `json:"method_counts,omitempty"`
}

// Sanitization controls how a Trace is rewritten for a reseller.
type Sanitization struct {
	IPRewrites map[string]string
}

// Lookup returns the merged trace matching callID. Either the sanitized
// prefix or the full form works. The trace is sorted by UnixTime.
func Lookup(ctx context.Context, callID, ourPublicIP string) (*Trace, error) {
	pcaps, err := filepath.Glob(filepath.Join(PcapDir, "*.pcap"))
	if err != nil {
		return nil, fmt.Errorf("glob pcaps: %w", err)
	}
	sort.Strings(pcaps)

	prefix := domain.SanitizeCallID(callID)
	if prefix == "" {
		return &Trace{CallID: prefix, PcapFiles: pcaps}, nil
	}

	// Cache hit → done in microseconds. Pcaps are append-only rolling files
	// that don't change after they've been written for the call's window, so
	// stale cache is not a concern within the TTL.
	if cached := cacheGet(prefix); cached != nil {
		return cached, nil
	}

	tr := &Trace{CallID: prefix, PcapFiles: pcaps}
	matched := preFilterPcaps(ctx, pcaps, prefix)
	if len(matched) == 0 {
		// Even an empty result is worth caching briefly — saves re-grep on
		// reload of a "no captures" page. Use the same TTL.
		cachePut(prefix, tr)
		return tr, nil
	}

	filter := `sip.Call-ID contains "` + prefix + `"`

	type result struct {
		messages []Message
		raw      string
	}
	results := make([]result, len(matched))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, p := range matched {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Run the two tshark passes in parallel — they each take a full
			// pcap parse, so doing them serially doubles wall time. The
			// outer sem already caps total tshark concurrency.
			var inner sync.WaitGroup
			inner.Add(2)
			go func() {
				defer inner.Done()
				out, _ := runTshark(ctx, p, filter,
					"-T", "fields",
					"-e", "frame.time_epoch",
					"-e", "ip.src", "-e", "udp.srcport",
					"-e", "ip.dst", "-e", "udp.dstport",
					"-e", "sip.Request-Line",
					"-e", "sip.Status-Line",
					"-E", "separator=|",
				)
				results[i].messages = parseFieldDump(out, ourPublicIP)
			}()
			go func() {
				defer inner.Done()
				raw, _ := runTshark(ctx, p, filter, "-O", "sip", "-V")
				results[i].raw = string(raw)
			}()
			inner.Wait()
		}(i, p)
	}
	wg.Wait()

	for _, r := range results {
		tr.Messages = append(tr.Messages, r.messages...)
		tr.Raw += r.raw
	}
	sort.Slice(tr.Messages, func(i, j int) bool {
		return tr.Messages[i].UnixTime < tr.Messages[j].UnixTime
	})
	tr.derive()
	cachePut(prefix, tr)
	return tr, nil
}

// derive populates Endpoints, FinalSIPCode, FinalSIPReason, MethodCounts
// from tr.Messages. Idempotent; safe to call after a Sanitize() pass too.
func (tr *Trace) derive() {
	seen := map[string]bool{}
	tr.Endpoints = tr.Endpoints[:0]
	for _, m := range tr.Messages {
		for _, addr := range []string{m.SrcAddr, m.DstAddr} {
			if addr != ":" && !seen[addr] {
				seen[addr] = true
				tr.Endpoints = append(tr.Endpoints, addr)
			}
		}
	}

	tr.MethodCounts = map[string]int{}
	tr.FinalSIPCode = 0
	tr.FinalSIPReason = ""
	for _, m := range tr.Messages {
		s := strings.TrimSpace(m.Summary)
		if s == "" {
			continue
		}
		// Request line example:  "INVITE sip:… SIP/2.0"
		// Status line example:   "SIP/2.0 200 OK"
		if strings.HasPrefix(s, "SIP/") {
			parts := strings.SplitN(s, " ", 3)
			if len(parts) >= 2 {
				code := parts[1]
				if n, err := strconv.Atoi(code); err == nil {
					reason := ""
					if len(parts) == 3 {
						reason = parts[2]
					}
					// Final response = highest 200, else last 4xx/5xx/6xx.
					if n >= 200 && (tr.FinalSIPCode == 0 || responseRank(n) > responseRank(tr.FinalSIPCode)) {
						tr.FinalSIPCode = n
						tr.FinalSIPReason = reason
					}
					class := fmt.Sprintf("%dxx", n/100)
					tr.MethodCounts[class]++
					continue
				}
			}
		} else {
			// Request method (first word).
			parts := strings.SplitN(s, " ", 2)
			method := strings.ToUpper(parts[0])
			if method != "" {
				tr.MethodCounts[method]++
			}
		}
	}
}

// responseRank lets us compare two final SIP codes — 2xx wins over 4xx/5xx/6xx
// (a successful call beats a transient busy in our "final" notion).
func responseRank(code int) int {
	switch code / 100 {
	case 2:
		return 100
	case 3:
		return 50
	case 4:
		return 30
	case 5:
		return 20
	case 6:
		return 10
	}
	return 0
}

// preFilterPcaps returns only those pcaps that contain `needle` as a literal
// substring. `grep -F` on a pcap works because SIP over UDP is on-the-wire
// plaintext — the Call-ID hex appears verbatim in the captured bytes. Scans
// run in parallel so a fleet of pcaps doesn't add up to serial latency.
func preFilterPcaps(ctx context.Context, pcaps []string, needle string) []string {
	keep := make([]bool, len(pcaps))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, p := range pcaps {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			// grep -l: list filenames with at least one match, exit 0.
			// --binary-files=text: don't bail just because the file looks binary.
			// -q would also work but we want to keep the same semantics here.
			cmd := exec.CommandContext(cctx, "grep", "-lF", "--binary-files=text", needle, p)
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Run(); err == nil {
				keep[i] = true
			}
		}(i, p)
	}
	wg.Wait()
	out := make([]string, 0, len(pcaps))
	for i, k := range keep {
		if k {
			out = append(out, pcaps[i])
		}
	}
	return out
}

// parseFieldDump turns the `-T fields` output of tshark into Messages.
func parseFieldDump(out []byte, ourPublicIP string) []Message {
	var msgs []Message
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 7)
		if len(parts) < 7 {
			continue
		}
		ts, _ := strconv.ParseFloat(parts[0], 64)
		summary := strings.TrimSpace(parts[5])
		if summary == "" {
			summary = strings.TrimSpace(parts[6])
		}
		dir := "in"
		if parts[1] == ourPublicIP {
			dir = "out"
		}
		msgs = append(msgs, Message{
			UnixTime:  ts,
			Time:      time.Unix(int64(ts), int64((ts-float64(int64(ts)))*1e9)).UTC().Format(time.RFC3339Nano),
			Direction: dir,
			SrcAddr:   parts[1] + ":" + parts[2],
			DstAddr:   parts[3] + ":" + parts[4],
			Summary:   summary,
		})
	}
	return msgs
}

// Sanitize rewrites a Trace in place. Safe to call with nil/empty rewrites.
func (tr *Trace) Sanitize(s Sanitization) {
	if tr == nil || len(s.IPRewrites) == 0 {
		return
	}
	keys := make([]string, 0, len(s.IPRewrites))
	for k := range s.IPRewrites {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	rewrite := func(s2 string) string {
		for _, k := range keys {
			if k == "" {
				continue
			}
			s2 = strings.ReplaceAll(s2, k, s.IPRewrites[k])
		}
		return s2
	}

	for i := range tr.Messages {
		tr.Messages[i].SrcAddr = rewrite(tr.Messages[i].SrcAddr)
		tr.Messages[i].DstAddr = rewrite(tr.Messages[i].DstAddr)
		tr.Messages[i].Summary = rewrite(tr.Messages[i].Summary)
	}
	tr.Raw = rewrite(tr.Raw)
	// Refresh derived fields against the rewritten endpoint labels so the
	// API consumer / sequence diagram show the sanitized identifiers.
	tr.derive()
}

// runTshark runs tshark and returns its stdout. Importantly it returns
// whatever stdout was captured even if tshark exits non-zero — common when
// reading a live pcap (last packet appears truncated) but earlier packets
// are valid.
func runTshark(ctx context.Context, pcap, filter string, extra ...string) ([]byte, error) {
	args := []string{"-n", "-r", pcap, "-Y", filter}
	args = append(args, extra...)
	// Per-pcap timeout. preFilterPcaps already culled empties so this caps
	// the worst case on a pcap that *does* contain the call-id but is huge
	// (multi-GB rolling-day file).
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "tshark", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	err := cmd.Run()
	return stdout.Bytes(), err
}

// regexMeta and escapeRegex are kept for callers that want to build a regex
// (no longer used by Lookup itself, but exposed so admin scripts can reuse
// the helper).
var regexMeta = regexp.MustCompile(`[.+*?()|\[\]{}^$\\]`)

func escapeRegex(s string) string {
	return regexMeta.ReplaceAllStringFunc(s, func(m string) string { return `\` + m })
}
