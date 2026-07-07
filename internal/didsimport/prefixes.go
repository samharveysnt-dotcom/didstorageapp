// Package didsimport handles bulk-import of DID numbers from the admin GUI.
// Supports three input modes (numeric range, newline list, CSV upload) with
// per-row override, prefix-based country auto-detection, and a per-job
// progress / log channel the GUI subscribes to over SSE.
package didsimport

import (
	"sort"
	"strings"
)

// CountryPrefix maps an ITU dial prefix to the ISO-3166 alpha-2 country code
// we store in dids.country_iso. The list covers every assigned country code
// at time of writing. Longer prefixes (e.g. "1242" Bahamas) come before
// shorter overlapping ones (e.g. "1" US/CA) — the matcher picks the longest
// match first so a Bahamas number isn't misclassified as US/CA.
//
// Source: ITU-T E.164 assignments. The North American Numbering Plan (NANP)
// shares prefix "1" between US, Canada, and 25+ Caribbean countries; the
// matcher returns the most specific country when the area code maps to a
// non-US/CA territory, and falls back to "US" for plain "1" numbers (we
// can't distinguish US from CA from the country code alone; admin can
// override per row or post-import).
type CountryPrefix struct {
	Prefix string // digits only, no '+'
	ISO    string // ISO-3166 alpha-2
}

// rawPrefixes is the unsorted source list. matchOrder is built from it at
// init() and is what MatchCountry walks (longest first, then alphabetical).
var rawPrefixes = []CountryPrefix{
	// North America Numbering Plan — overlapping prefixes for territories,
	// then "1" itself for US/CA.
	{"1242", "BS"}, {"1246", "BB"}, {"1264", "AI"}, {"1268", "AG"},
	{"1284", "VG"}, {"1340", "VI"}, {"1345", "KY"}, {"1441", "BM"},
	{"1473", "GD"}, {"1649", "TC"}, {"1664", "MS"}, {"1670", "MP"},
	{"1671", "GU"}, {"1684", "AS"}, {"1721", "SX"}, {"1758", "LC"},
	{"1767", "DM"}, {"1784", "VC"}, {"1787", "PR"}, {"1809", "DO"},
	{"1829", "DO"}, {"1849", "DO"}, {"1868", "TT"}, {"1869", "KN"},
	{"1876", "JM"}, {"1939", "PR"},
	{"1", "US"}, // default NANP fallback

	// Europe (country codes 3xx and 4xx and shared 7xx)
	{"30", "GR"}, {"31", "NL"}, {"32", "BE"}, {"33", "FR"}, {"34", "ES"},
	{"36", "HU"}, {"39", "IT"},
	{"350", "GI"}, {"351", "PT"}, {"352", "LU"}, {"353", "IE"}, {"354", "IS"},
	{"355", "AL"}, {"356", "MT"}, {"357", "CY"}, {"358", "FI"}, {"359", "BG"},
	{"370", "LT"}, {"371", "LV"}, {"372", "EE"}, {"373", "MD"}, {"374", "AM"},
	{"375", "BY"}, {"376", "AD"}, {"377", "MC"}, {"378", "SM"}, {"379", "VA"},
	{"380", "UA"}, {"381", "RS"}, {"382", "ME"}, {"383", "XK"}, {"385", "HR"},
	{"386", "SI"}, {"387", "BA"}, {"389", "MK"},
	{"40", "RO"}, {"41", "CH"}, {"43", "AT"}, {"44", "GB"}, {"45", "DK"},
	{"46", "SE"}, {"47", "NO"}, {"48", "PL"}, {"49", "DE"},
	{"420", "CZ"}, {"421", "SK"}, {"423", "LI"},

	// Africa / Middle East
	{"20", "EG"}, {"211", "SS"}, {"212", "MA"}, {"213", "DZ"}, {"216", "TN"},
	{"218", "LY"}, {"220", "GM"}, {"221", "SN"}, {"222", "MR"}, {"223", "ML"},
	{"224", "GN"}, {"225", "CI"}, {"226", "BF"}, {"227", "NE"}, {"228", "TG"},
	{"229", "BJ"}, {"230", "MU"}, {"231", "LR"}, {"232", "SL"}, {"233", "GH"},
	{"234", "NG"}, {"235", "TD"}, {"236", "CF"}, {"237", "CM"}, {"238", "CV"},
	{"239", "ST"}, {"240", "GQ"}, {"241", "GA"}, {"242", "CG"}, {"243", "CD"},
	{"244", "AO"}, {"245", "GW"}, {"246", "IO"}, {"248", "SC"}, {"249", "SD"},
	{"250", "RW"}, {"251", "ET"}, {"252", "SO"}, {"253", "DJ"}, {"254", "KE"},
	{"255", "TZ"}, {"256", "UG"}, {"257", "BI"}, {"258", "MZ"}, {"260", "ZM"},
	{"261", "MG"}, {"262", "RE"}, {"263", "ZW"}, {"264", "NA"}, {"265", "MW"},
	{"266", "LS"}, {"267", "BW"}, {"268", "SZ"}, {"269", "KM"},
	{"27", "ZA"},
	{"290", "SH"}, {"291", "ER"}, {"297", "AW"}, {"298", "FO"}, {"299", "GL"},

	// Latin America
	{"51", "PE"}, {"52", "MX"}, {"53", "CU"}, {"54", "AR"}, {"55", "BR"},
	{"56", "CL"}, {"57", "CO"}, {"58", "VE"},
	{"500", "FK"}, {"501", "BZ"}, {"502", "GT"}, {"503", "SV"}, {"504", "HN"},
	{"505", "NI"}, {"506", "CR"}, {"507", "PA"}, {"508", "PM"}, {"509", "HT"},
	{"590", "GP"}, {"591", "BO"}, {"592", "GY"}, {"593", "EC"}, {"594", "GF"},
	{"595", "PY"}, {"596", "MQ"}, {"597", "SR"}, {"598", "UY"}, {"599", "CW"},

	// Southeast Asia / Oceania (6xx)
	{"60", "MY"}, {"61", "AU"}, {"62", "ID"}, {"63", "PH"}, {"64", "NZ"},
	{"65", "SG"}, {"66", "TH"},
	{"670", "TL"}, {"672", "AQ"}, {"673", "BN"}, {"674", "NR"}, {"675", "PG"},
	{"676", "TO"}, {"677", "SB"}, {"678", "VU"}, {"679", "FJ"}, {"680", "PW"},
	{"681", "WF"}, {"682", "CK"}, {"683", "NU"}, {"685", "WS"}, {"686", "KI"},
	{"687", "NC"}, {"688", "TV"}, {"689", "PF"}, {"690", "TK"}, {"691", "FM"},
	{"692", "MH"},

	// Russia / former USSR (7xx)
	{"7840", "AB"}, {"7940", "AB"}, // Abkhazia
	{"7", "RU"}, // RU/KZ share prefix "7"; admin picks per range

	// East Asia
	{"81", "JP"}, {"82", "KR"}, {"84", "VN"}, {"86", "CN"},
	{"850", "KP"}, {"852", "HK"}, {"853", "MO"}, {"855", "KH"}, {"856", "LA"},
	{"880", "BD"}, {"886", "TW"},

	// South / West Asia (9xx)
	{"90", "TR"}, {"91", "IN"}, {"92", "PK"}, {"93", "AF"}, {"94", "LK"},
	{"95", "MM"}, {"98", "IR"},
	{"960", "MV"}, {"961", "LB"}, {"962", "JO"}, {"963", "SY"}, {"964", "IQ"},
	{"965", "KW"}, {"966", "SA"}, {"967", "YE"}, {"968", "OM"}, {"970", "PS"},
	{"971", "AE"}, {"972", "IL"}, {"973", "BH"}, {"974", "QA"}, {"975", "BT"},
	{"976", "MN"}, {"977", "NP"}, {"992", "TJ"}, {"993", "TM"}, {"994", "AZ"},
	{"995", "GE"}, {"996", "KG"}, {"998", "UZ"},

	// Non-geographic / special
	{"800", "INT"}, // global toll-free
	{"808", "INT"}, // shared cost
	{"870", "INT"}, // INMARSAT
	{"878", "INT"}, // Universal Personal Telecom
	{"881", "INT"}, // Global Mobile Satellite
	{"882", "INT"}, // International Networks
	{"883", "INT"}, // International Networks
	{"888", "INT"}, // Telecommunications for Disaster Relief
	{"979", "INT"}, // International Premium Rate
}

// matchOrder is rawPrefixes sorted by descending prefix length so the
// MatchCountry walk picks the most specific entry first.
var matchOrder []CountryPrefix

func init() {
	matchOrder = append(matchOrder, rawPrefixes...)
	sort.Slice(matchOrder, func(i, j int) bool {
		if len(matchOrder[i].Prefix) != len(matchOrder[j].Prefix) {
			return len(matchOrder[i].Prefix) > len(matchOrder[j].Prefix)
		}
		return matchOrder[i].Prefix < matchOrder[j].Prefix
	})
}

// MatchCountry returns the ISO-3166 alpha-2 code of the country whose dial
// prefix best matches the given E.164 number (digits only, no '+'). Returns
// ("", false) when no prefix in the table matches — caller should fall back
// to a per-import default or surface an error to the admin.
//
// "INT" is returned for global / non-geographic ranges (toll-free, satellite,
// etc.). It isn't a real ISO code; callers using country_iso as a FK should
// either remap it to a placeholder country row or reject the import for
// those numbers.
func MatchCountry(e164 string) (iso string, ok bool) {
	e164 = strings.TrimPrefix(e164, "+")
	for _, p := range matchOrder {
		if strings.HasPrefix(e164, p.Prefix) {
			return p.ISO, true
		}
	}
	return "", false
}
