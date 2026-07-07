package didsimport

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseResult is what every Parse* returns: the entries that successfully
// parsed, plus per-line warnings for entries that didn't (so the GUI can
// surface a single combined "47 rows imported, 3 skipped at parse" message).
type ParseResult struct {
	Entries  []Entry
	Warnings []string
}

const (
	// MaxRows is a hard cap on a single import job. 10k is what the
	// existing form documents; bumping it requires re-checking the
	// SSE buffer + per-row insert latency budget.
	MaxRows = 10_000
)

// ParseRange covers the legacy form mode: a numeric start (and optional
// end) E.164. Both are digits-only after trimming. End defaults to start.
// Returns one Entry per integer in [start, end], in ascending order.
//
// Errors out (not warns) if start is empty, non-numeric, end < start, or
// the range exceeds MaxRows.
func ParseRange(startStr, endStr string) (ParseResult, error) {
	startStr = onlyDigits(startStr)
	endStr = onlyDigits(endStr)
	if startStr == "" {
		return ParseResult{}, errors.New("start E.164 is required")
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start <= 0 {
		return ParseResult{}, fmt.Errorf("bad start: %q", startStr)
	}
	end := start
	if endStr != "" {
		end, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil || end < start {
			return ParseResult{}, fmt.Errorf("bad end: %q", endStr)
		}
	}
	if end-start+1 > MaxRows {
		return ParseResult{}, fmt.Errorf("range %d..%d exceeds max %d per import", start, end, MaxRows)
	}
	out := make([]Entry, 0, end-start+1)
	for n := start; n <= end; n++ {
		out = append(out, Entry{E164: strconv.FormatInt(n, 10)})
	}
	return ParseResult{Entries: out}, nil
}

// ParseBulk splits free-form text on commas / whitespace / newlines and
// keeps anything that looks like an E.164 (digits only after stripping
// '+', leading zeros, and common separators like '-' '(' ')' ' '). One
// entry per accepted token; per-line warnings for rejected garbage.
//
// Comment lines starting with '#' are ignored silently.
func ParseBulk(text string) (ParseResult, error) {
	var out ParseResult
	for ln, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tokenize within the line (handles a paste of "44...,44...,44...")
		toks := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		})
		for _, t := range toks {
			t = normalizeE164(t)
			if t == "" {
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("line %d: skipped %q (not a valid E.164)", ln+1, raw))
				continue
			}
			out.Entries = append(out.Entries, Entry{E164: t})
		}
	}
	if len(out.Entries) > MaxRows {
		return ParseResult{}, fmt.Errorf("parsed %d entries, exceeds max %d per import", len(out.Entries), MaxRows)
	}
	if len(out.Entries) == 0 && len(out.Warnings) == 0 {
		return ParseResult{}, errors.New("no entries found in input")
	}
	return out, nil
}

// ParseCSV handles uploaded CSVs. The format is intentionally lenient:
//
//   - Comment lines starting with '#' are ignored.
//   - The first non-comment row is inspected: if its first cell parses as
//     a number-like E.164, the file is treated as headerless and the first
//     column is e164. Otherwise the row is a header and column names are
//     resolved case-insensitively against a small known set.
//   - Recognized headers: e164 / number / did, country / country_iso,
//     did_type / type, supplier_id, channel_cap / supplier_channel_cap.
//   - Any unrecognized column is silently ignored — pasted carrier sheets
//     often carry extra columns (notes, label, region) and we don't want
//     to refuse the whole upload over a stray header.
//
// Per-row override semantics: if a cell is empty, the worker falls back to
// the Job's defaults / form-level supplier id; if a cell is non-empty, it
// overrides the form for that row.
func ParseCSV(r io.Reader) (ParseResult, error) {
	rd := csv.NewReader(r)
	rd.FieldsPerRecord = -1 // tolerate ragged rows
	rd.LazyQuotes = true    // tolerate stray quotes in pasted data
	rd.TrimLeadingSpace = true

	// Read everything up front. CSVs in the import path are tiny (<=10k
	// rows, ~1MB worst case) — streaming would complicate the parse-warning
	// flow with no real memory benefit.
	rows, err := rd.ReadAll()
	if err != nil {
		return ParseResult{}, fmt.Errorf("csv parse: %w", err)
	}
	// Drop comment lines.
	clean := rows[:0]
	for _, row := range rows {
		if len(row) == 0 || strings.HasPrefix(strings.TrimSpace(row[0]), "#") {
			continue
		}
		clean = append(clean, row)
	}
	if len(clean) == 0 {
		return ParseResult{}, errors.New("csv is empty")
	}

	// Header detection. If the first cell of the first row, after
	// normalization, parses as digits-only of plausible E.164 length,
	// the row IS data, not header.
	colIdx := map[string]int{}
	startRow := 0
	if !looksLikeE164(clean[0][0]) {
		for i, name := range clean[0] {
			key := strings.ToLower(strings.TrimSpace(name))
			switch key {
			case "e164", "number", "did", "phone", "msisdn":
				colIdx["e164"] = i
			case "country", "country_iso", "iso", "cc":
				colIdx["country"] = i
			case "did_type", "type", "kind":
				colIdx["did_type"] = i
			case "supplier_id", "supplier":
				colIdx["supplier_id"] = i
			case "channel_cap", "supplier_channel_cap", "channels", "cap":
				colIdx["channel_cap"] = i
			}
		}
		if _, ok := colIdx["e164"]; !ok {
			return ParseResult{}, errors.New("csv header missing an e164 / number / did column")
		}
		startRow = 1
	} else {
		// Headerless: positional columns in the natural order.
		colIdx = map[string]int{
			"e164":         0,
			"country":      1,
			"did_type":     2,
			"supplier_id":  3,
			"channel_cap":  4,
		}
	}

	var out ParseResult
	for r := startRow; r < len(clean); r++ {
		row := clean[r]
		if len(row) == 0 {
			continue
		}
		get := func(key string) string {
			i, ok := colIdx[key]
			if !ok || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}
		e164 := normalizeE164(get("e164"))
		if e164 == "" {
			if get("e164") == "" {
				continue // entirely blank row
			}
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("csv row %d: skipped %q (not a valid E.164)", r+1, get("e164")))
			continue
		}
		e := Entry{E164: e164}
		if v := strings.ToUpper(get("country")); v != "" && len(v) == 2 {
			e.Country = v
		}
		if v := strings.ToLower(get("did_type")); v != "" {
			switch v {
			case "mobile", "national", "local", "tollfree", "toll_free", "toll-free":
				if v == "toll_free" || v == "toll-free" {
					v = "tollfree"
				}
				e.DIDType = v
			default:
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("csv row %d: unknown did_type %q (using form default)", r+1, v))
			}
		}
		if v := get("supplier_id"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				e.SupplierID = n
			} else {
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("csv row %d: supplier_id %q not numeric (using form default)", r+1, v))
			}
		}
		if v := get("channel_cap"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				if n < 1 {
					e.ChannelCap = -1
				} else {
					e.ChannelCap = n
				}
			}
		}
		out.Entries = append(out.Entries, e)
		if len(out.Entries) > MaxRows {
			return ParseResult{}, fmt.Errorf("csv exceeds %d rows", MaxRows)
		}
	}
	if len(out.Entries) == 0 {
		return out, errors.New("csv had no valid e164 rows")
	}
	return out, nil
}

// ExampleCSV is what the GUI offers for download via "show example".
// Keep it short, comment-annotated, and exercise every optional column
// so admins see how per-row overrides combine with form defaults.
const ExampleCSV = `# DIDStorage bulk-import example.
# Required column: e164. All others are optional and override the form-level
# defaults when present. Headerless files work too — drop the header row
# and the columns are read in this same order.
e164,country_iso,did_type,supplier_id,channel_cap
442038968001,GB,mobile,,
442038968002,,mobile,,5
525599632249,MX,mobile,2,
18005551212,US,tollfree,,
`

// onlyDigits strips everything that isn't an ASCII digit. Used for the
// range parser where the input MUST be a pure integer (no '+', no '.').
func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeE164 returns the digits-only form of an E.164 number, or ""
// if the input doesn't plausibly look like one. Accepts inputs like
// "+44 20 3896 8001", "(44) 020-3896-8001", "00442038968001", returns
// "442038968001" or similar (we don't strip the international "00"
// prefix because some carriers represent numbers that way; admins can
// fix individual rows post-import if needed). We DO strip the leading
// '+' since dids.e164 is digits-only by convention.
func normalizeE164(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	digits := onlyDigits(s)
	if !looksLikeE164(digits) {
		return ""
	}
	return digits
}

// looksLikeE164 is a plausibility check: 6..15 digits, all numeric. This
// catches both real international numbers (E.164 max is 15) and very short
// local-only formats; tighter validation belongs in the carrier integration
// layer, not in the import parser.
func looksLikeE164(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	s = onlyDigits(s)
	return len(s) >= 6 && len(s) <= 15
}
