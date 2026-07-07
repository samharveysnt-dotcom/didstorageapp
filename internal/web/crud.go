package web

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"didstorage/internal/asteriskcfg"
	"didstorage/internal/domain"
)

// ---- session flash helpers ----

func (h *Handler) flashOK(r *http.Request, msg string)  { h.Session.Put(r.Context(), "flash:ok", msg) }
func (h *Handler) flashErr(r *http.Request, msg string) { h.Session.Put(r.Context(), "flash:err", msg) }

func (h *Handler) popFlashes(r *http.Request) (ok, errm string) {
	ok = h.Session.PopString(r.Context(), "flash:ok")
	errm = h.Session.PopString(r.Context(), "flash:err")
	return
}

// ---- small parsing helpers ----

func atoi64(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
func atoi(s string) int     { v, _ := strconv.Atoi(s); return v }
func atof(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
func dollarsToCents(s string) int64 {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	v, _ := strconv.ParseFloat(s, 64)
	return int64(v*100 + 0.5)
}

func pathID(r *http.Request, name string) int64 { return atoi64(chi.URLParam(r, name)) }

func genSecret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ha1(user, realm, pass string) string {
	sum := md5.Sum([]byte(user + ":" + realm + ":" + pass))
	return hex.EncodeToString(sum[:])
}

const sipRealm = "didstorage.local"

// =====================================================================
// SUPPLIERS
// =====================================================================

func (h *Handler) supplierNew(w http.ResponseWriter, r *http.Request) {
	ok, em := h.popFlashes(r)
	h.render(w, "supplier_edit", map[string]any{
		"Title":    "New supplier",
		"Section":  "suppliers",
		"FlashOK":  ok,
		"FlashErr": em,
		"IsNew":    true,
		"Supplier": map[string]any{"Name": "", "Status": "active", "Notes": ""},
	})
}

func (h *Handler) supplierCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/suppliers/new", http.StatusFound)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		h.flashErr(r, "name is required")
		http.Redirect(w, r, "/suppliers/new", http.StatusFound)
		return
	}
	var id int64
	err := h.DB.QueryRow(r.Context(),
		`INSERT INTO suppliers (name, status, notes) VALUES ($1,$2,$3) RETURNING id`,
		name,
		nonempty(r.PostForm.Get("status"), "active"),
		r.PostForm.Get("notes"),
	).Scan(&id)
	if err != nil {
		h.flashErr(r, "create failed: "+err.Error())
		http.Redirect(w, r, "/suppliers/new", http.StatusFound)
		return
	}
	h.flashOK(r, "Supplier created")
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
}

func (h *Handler) supplierDetail(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var sup struct {
		ID                  int64
		Name, Status, Notes string
	}
	err := h.DB.QueryRow(r.Context(),
		`SELECT id, name, status, COALESCE(notes,'') FROM suppliers WHERE id=$1`, id,
	).Scan(&sup.ID, &sup.Name, &sup.Status, &sup.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.Log.Error("supplierDetail", "err", err)
		http.Error(w, "internal", 500)
		return
	}

	// A member row is either an IP/CIDR or a hostname (CHECK constraint
	// supplier_ip_member_one_of_check enforces it). We pre-compute Display
	// and Kind so the template doesn't have to branch.
	type ipMember struct {
		ID      int64
		Display string // either the IP/CIDR string or the hostname
		Kind    string // "cidr" | "hostname"
	}
	type ipGroup struct {
		ID      int64
		Name    string
		Members []ipMember
	}
	var groups []ipGroup
	rows, err := h.DB.Query(r.Context(),
		`SELECT id, name FROM supplier_ip_groups WHERE supplier_id=$1 ORDER BY id`, id)
	if err == nil {
		for rows.Next() {
			var g ipGroup
			rows.Scan(&g.ID, &g.Name)
			groups = append(groups, g)
		}
		rows.Close()
		for i := range groups {
			mr, err := h.DB.Query(r.Context(), `
				SELECT id,
				       COALESCE(host(cidr), ''),
				       COALESCE(hostname,   '')
				  FROM supplier_ip_group_members
				 WHERE group_id=$1
				 ORDER BY id`, groups[i].ID)
			if err == nil {
				for mr.Next() {
					var m ipMember
					var cidrStr, hostStr string
					mr.Scan(&m.ID, &cidrStr, &hostStr)
					if cidrStr != "" {
						m.Display, m.Kind = cidrStr, "cidr"
					} else {
						m.Display, m.Kind = hostStr, "hostname"
					}
					groups[i].Members = append(groups[i].Members, m)
				}
				mr.Close()
			}
		}
	}

	// Country-grouped rate-card view. Active cards (valid_to IS NULL) get the
	// full profit treatment; expired ones still show under the country
	// header but with margin = 0 so they don't pull the average down.
	type profitSample struct {
		Seconds   int     // call duration sample
		Customer  float64 // dollars charged to customer
		Supplier  float64 // dollars supplier charges us
		Profit    float64 // customer - supplier
	}
	type rateCard struct {
		ID                                             int64
		Country, CountryName, FlagEmoji, DIDType       string
		NRCDollars, MRCDollars, ChannelDollars         float64
		PerMinDollars                                  float64
		SupNRCDollars, SupMRCDollars, SupPerMinDollars float64
		// BillMin / BillInc are the *customer-side* billing increment
		// ("60/60" = 60s connection minimum, 60s round-up). SupBillMin /
		// SupBillInc are the *supplier-side* equivalent — what the supplier
		// uses to invoice US. Both pairs come from rate_cards and are
		// snapshotted onto cdrs at call time.
		BillMin, BillInc       int
		SupBillMin, SupBillInc int
		// Profit / margin breakdown.
		PerMinMargin, MRCMargin, NRCMargin float64
		MarginPct                          float64 // 0..100, per-minute margin %
		Samples                            []profitSample
		ValidFrom                          string
		ValidTo                            string
		Active                             bool
	}
	type countryGroup struct {
		ISO       string
		Name      string
		FlagEmoji string
		Active    int        // # of active rate cards under this country
		Total     int        // including expired
		ActiveKinds map[string]bool // did_type → present (for the modal's "still available" filter)
		Cards     []rateCard
	}

	var rateGroups []countryGroup
	rateGroupIdx := map[string]int{} // ISO → index in rateGroups slice

	rrows, err := h.DB.Query(r.Context(),
		`SELECT rc.id, rc.country_iso, c.name, rc.did_type::text,
		        rc.nrc_cents, rc.mrc_cents, rc.channel_monthly_cents, rc.per_minute_cents,
		        rc.bill_min_seconds, rc.bill_increment_seconds,
		        rc.supplier_per_minute_cents, rc.supplier_nrc_cents, rc.supplier_mrc_cents,
		        rc.supplier_bill_min_seconds, rc.supplier_bill_increment_seconds,
		        to_char(rc.valid_from,'YYYY-MM-DD'),
		        COALESCE(to_char(rc.valid_to,'YYYY-MM-DD'),''),
		        rc.valid_to IS NULL AS active
		   FROM rate_cards rc
		   LEFT JOIN countries c ON c.iso = rc.country_iso
		  WHERE rc.supplier_id = $1
		  ORDER BY rc.country_iso, rc.did_type, rc.valid_from DESC`, id)
	if err == nil {
		for rrows.Next() {
			var rc rateCard
			var name *string
			var nrc, mrc, ch int
			var pmin float64
			var supPMin float64
			var supNRC, supMRC int
			rrows.Scan(&rc.ID, &rc.Country, &name, &rc.DIDType,
				&nrc, &mrc, &ch, &pmin,
				&rc.BillMin, &rc.BillInc,
				&supPMin, &supNRC, &supMRC, &rc.SupBillMin, &rc.SupBillInc,
				&rc.ValidFrom, &rc.ValidTo, &rc.Active)
			if name != nil {
				rc.CountryName = *name
			}
			rc.FlagEmoji = domain.FlagEmoji(rc.Country)
			rc.NRCDollars = float64(nrc) / 100
			rc.MRCDollars = float64(mrc) / 100
			rc.ChannelDollars = float64(ch) / 100
			rc.PerMinDollars = pmin / 100
			rc.SupNRCDollars = float64(supNRC) / 100
			rc.SupMRCDollars = float64(supMRC) / 100
			rc.SupPerMinDollars = supPMin / 100
			rc.NRCMargin = rc.NRCDollars - rc.SupNRCDollars
			rc.MRCMargin = rc.MRCDollars - rc.SupMRCDollars
			rc.PerMinMargin = rc.PerMinDollars - rc.SupPerMinDollars
			if rc.PerMinDollars > 0 {
				rc.MarginPct = (rc.PerMinMargin / rc.PerMinDollars) * 100
			}

			// Profit samples at common call durations. Each side uses its
			// own min/inc — customer (rc.BillMin/BillInc) and supplier
			// (rc.SupBillMin/SupBillInc).
			for _, secs := range []int{30, 60, 180, 300, 600} {
				_, _, custCents := domain.ChargeForCall(secs, rc.BillMin, rc.BillInc, pmin)
				supCents := domain.SupplierChargeForCall(secs, rc.SupBillMin, rc.SupBillInc, supPMin)
				rc.Samples = append(rc.Samples, profitSample{
					Seconds:  secs,
					Customer: float64(custCents) / 100,
					Supplier: float64(supCents) / 100,
					Profit:   float64(custCents-supCents) / 100,
				})
			}

			// Bucket into country groups.
			gIdx, ok := rateGroupIdx[rc.Country]
			if !ok {
				rateGroups = append(rateGroups, countryGroup{
					ISO:         rc.Country,
					Name:        rc.CountryName,
					FlagEmoji:   rc.FlagEmoji,
					ActiveKinds: map[string]bool{},
				})
				gIdx = len(rateGroups) - 1
				rateGroupIdx[rc.Country] = gIdx
			}
			g := &rateGroups[gIdx]
			g.Total++
			if rc.Active {
				g.Active++
				g.ActiveKinds[rc.DIDType] = true
			}
			g.Cards = append(g.Cards, rc)
		}
		rrows.Close()
	}

	var didCount int
	h.DB.QueryRow(r.Context(), `SELECT count(*) FROM dids WHERE supplier_id=$1`, id).Scan(&didCount)

	countries := h.listCountries(r)

	ok, em := h.popFlashes(r)
	// Build country-keyed JSON of active types so the rate-modal JS can
	// disable already-taken (country, did_type) combos at form-time.
	activeByISO := map[string][]string{}
	for _, g := range rateGroups {
		var kinds []string
		for k := range g.ActiveKinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		activeByISO[g.ISO] = kinds
	}
	activeJSON, _ := json.Marshal(activeByISO)

	h.render(w, "supplier_detail", map[string]any{
		"Title":           "Supplier · " + sup.Name,
		"Section":         "suppliers",
		"FlashOK":         ok,
		"FlashErr":        em,
		"Supplier":        sup,
		"Groups":          groups, // IP groups (whitelisting tab)
		"RateGroups":      rateGroups,
		"Countries":       countries,
		"DIDCount":        didCount,
		"ActiveTypesJSON": template.JS(activeJSON),
	})
}

func (h *Handler) supplierUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
	} else {
		_, err := h.DB.Exec(r.Context(),
			`UPDATE suppliers SET name=$1, status=$2, notes=$3 WHERE id=$4`,
			strings.TrimSpace(r.PostForm.Get("name")),
			nonempty(r.PostForm.Get("status"), "active"),
			r.PostForm.Get("notes"), id)
		if err != nil {
			h.flashErr(r, "save failed: "+err.Error())
		} else {
			h.flashOK(r, "Saved")
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
}

func (h *Handler) ipGroupCreate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name != "" {
		_, err := h.DB.Exec(r.Context(),
			`INSERT INTO supplier_ip_groups (supplier_id, name) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			id, name)
		if err != nil {
			h.flashErr(r, "group create: "+err.Error())
		} else {
			h.flashOK(r, "Group created")
			// A group with no members yet won't appear in the generated
			// identify file (empty match list = PJSIP error). Regen is
			// still cheap; do it so the file stays sync'd with DB state.
			go h.regenSupplierIdentifies(r)
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
}

// ipGroupUpdate renames an IP group. Form field: name.
func (h *Handler) ipGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	gid := pathID(r, "gid")
	r.ParseForm()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		h.flashErr(r, "name required")
	} else {
		_, err := h.DB.Exec(r.Context(),
			`UPDATE supplier_ip_groups SET name=$1 WHERE id=$2 AND supplier_id=$3`,
			name, gid, id)
		if err != nil {
			h.flashErr(r, "rename: "+err.Error())
		} else {
			h.flashOK(r, "Group renamed")
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
}

// ipGroupDelete removes an IP group and all its members (cascades).
func (h *Handler) ipGroupDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	gid := pathID(r, "gid")
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM supplier_ip_group_members WHERE group_id=$1`, gid); err != nil {
		h.flashErr(r, "delete members: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM supplier_ip_groups WHERE id=$1 AND supplier_id=$2`, gid, id); err != nil {
		h.flashErr(r, "delete group: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	tx.Commit(r.Context())
	h.flashOK(r, "Group deleted")
	go h.regenSupplierIdentifies(r)
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
}

// ipMemberAddBulk accepts a textarea full of newline / comma / whitespace
// separated IPs (and optional /N CIDRs). Each line is normalized and inserted
// idempotently. Empty lines are skipped, malformed entries skipped with a
// warning row in the flash message.
func (h *Handler) ipMemberAddBulk(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	gid := pathID(r, "gid")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	raw := r.PostForm.Get("ips")
	// Split on whitespace OR comma so any pasted format works.
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' ' || r == ';'
	})
	added, skipped := 0, 0
	for _, raw := range fields {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		kind, norm := classifyMatch(raw)
		switch kind {
		case "cidr":
			if _, err := h.DB.Exec(r.Context(),
				`INSERT INTO supplier_ip_group_members (group_id, cidr) VALUES ($1, $2::cidr)
				 ON CONFLICT DO NOTHING`, gid, norm); err != nil {
				skipped++
				h.Log.Warn("bulk add skipped (cidr)", "in", raw, "err", err)
			} else {
				added++
			}
		case "hostname":
			if _, err := h.DB.Exec(r.Context(),
				`INSERT INTO supplier_ip_group_members (group_id, hostname) VALUES ($1, $2)
				 ON CONFLICT DO NOTHING`, gid, norm); err != nil {
				skipped++
				h.Log.Warn("bulk add skipped (hostname)", "in", raw, "err", err)
			} else {
				added++
			}
		default:
			skipped++
			h.Log.Warn("bulk add skipped (unrecognized)", "in", raw)
		}
	}
	if skipped > 0 {
		h.flashOK(r, fmt.Sprintf("Added %d IPs (%d skipped — bad format)", added, skipped))
	} else {
		h.flashOK(r, fmt.Sprintf("Added %d IPs", added))
	}
	if added > 0 {
		go h.regenSupplierIdentifies(r)
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
}

func (h *Handler) ipMemberAdd(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	gid := pathID(r, "gid")
	r.ParseForm()
	raw := strings.TrimSpace(r.PostForm.Get("cidr"))
	if raw == "" {
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
		return
	}
	kind, norm := classifyMatch(raw)
	var err error
	switch kind {
	case "cidr":
		_, err = h.DB.Exec(r.Context(),
			`INSERT INTO supplier_ip_group_members (group_id, cidr) VALUES ($1,$2::cidr)
			 ON CONFLICT DO NOTHING`, gid, norm)
	case "hostname":
		_, err = h.DB.Exec(r.Context(),
			`INSERT INTO supplier_ip_group_members (group_id, hostname) VALUES ($1,$2)
			 ON CONFLICT DO NOTHING`, gid, norm)
	default:
		h.flashErr(r, "not a valid IP/CIDR or hostname: "+raw)
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
		return
	}
	if err != nil {
		h.flashErr(r, "add: "+err.Error())
	} else {
		if kind == "hostname" {
			h.flashOK(r, "Hostname added (PJSIP will resolve at next reload)")
		} else {
			h.flashOK(r, "IP added")
		}
		go h.regenSupplierIdentifies(r)
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
}

// ipMemberEdit replaces a single IP member with a new value. By policy this
// is implemented as DELETE + INSERT in one transaction rather than UPDATE —
// so the regen pipeline always sees the same shape of mutation (rows leave
// and enter the table; never silently change in place) and any future
// triggers / audit observers don't need to special-case updates. Atomic
// via the transaction: if either side fails, nothing changes.
func (h *Handler) ipMemberEdit(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	gid := pathID(r, "gid")
	mid := pathID(r, "mid")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	raw := strings.TrimSpace(r.PostForm.Get("cidr"))
	if raw == "" {
		h.flashErr(r, "value required")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	kind, norm := classifyMatch(raw)
	if kind == "" {
		h.flashErr(r, "not a valid IP/CIDR or hostname: "+raw)
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())
	// Verify the member belongs to the given group (defence-in-depth against
	// a forged mid in a different supplier's group).
	var ok bool
	if err := tx.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM supplier_ip_group_members WHERE id=$1 AND group_id=$2)`,
		mid, gid).Scan(&ok); err != nil || !ok {
		h.flashErr(r, "row not found in this group")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM supplier_ip_group_members WHERE id=$1`, mid); err != nil {
		h.flashErr(r, "delete old: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	switch kind {
	case "cidr":
		_, err = tx.Exec(r.Context(),
			`INSERT INTO supplier_ip_group_members (group_id, cidr) VALUES ($1, $2::cidr)`,
			gid, norm)
	case "hostname":
		_, err = tx.Exec(r.Context(),
			`INSERT INTO supplier_ip_group_members (group_id, hostname) VALUES ($1, $2)`,
			gid, norm)
	}
	if err != nil {
		h.flashErr(r, "insert new: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "commit: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
		return
	}
	h.flashOK(r, "Match updated (re-added as "+norm+")")
	go h.regenSupplierIdentifies(r)
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#ips", id), http.StatusFound)
}

func (h *Handler) ipMemberDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	mid := pathID(r, "mid")
	_, err := h.DB.Exec(r.Context(), `DELETE FROM supplier_ip_group_members WHERE id=$1`, mid)
	if err != nil {
		h.flashErr(r, "delete: "+err.Error())
	} else {
		h.flashOK(r, "IP removed")
		go h.regenSupplierIdentifies(r)
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", id), http.StatusFound)
}

func (h *Handler) rateCardCreate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	country := strings.ToUpper(strings.TrimSpace(r.PostForm.Get("country_iso")))
	did := strings.TrimSpace(r.PostForm.Get("did_type"))
	nrc := dollarsToCents(r.PostForm.Get("nrc"))
	mrc := dollarsToCents(r.PostForm.Get("mrc"))
	ch := dollarsToCents(r.PostForm.Get("channel"))
	pmin := atof(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("per_minute")), "$")) * 100
	// Customer-side billing increment ("60/60" by default — i.e. 60s
	// connection minimum, 60s round-up). Blank = legacy default.
	custBillMin := atoi(r.PostForm.Get("bill_min_seconds"))
	if custBillMin == 0 {
		custBillMin = 60
	}
	custBillInc := atoi(r.PostForm.Get("bill_increment_seconds"))
	if custBillInc == 0 {
		custBillInc = 60
	}
	// Supplier-side cost picture (optional — defaults to 0 if blank).
	supPMin := atof(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("supplier_per_minute")), "$")) * 100
	supNRC := dollarsToCents(r.PostForm.Get("supplier_nrc"))
	supMRC := dollarsToCents(r.PostForm.Get("supplier_mrc"))
	billMin := atoi(r.PostForm.Get("supplier_bill_min_seconds"))
	if billMin == 0 {
		billMin = 60
	}
	billInc := atoi(r.PostForm.Get("supplier_bill_increment_seconds"))
	if billInc == 0 {
		billInc = 60
	}
	// Defensive validation. The DB has a CHECK >0, but a friendlier message
	// here saves the user a round trip + opaque Postgres error.
	allowed := map[int]bool{1: true, 6: true, 15: true, 30: true, 60: true}
	if !allowed[custBillMin] || !allowed[custBillInc] {
		h.flashErr(r, "customer bill cycle must be 1/1, 6/6, 15/15, 30/30, or 60/60")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#rates", id), http.StatusFound)
		return
	}

	if country == "" || did == "" {
		h.flashErr(r, "country and did_type required")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#rates", id), http.StatusFound)
		return
	}

	// Refuse duplicate active rate (enforced by the unique partial index in
	// migration 0011 too; check here so we can give a friendly message
	// instead of "duplicate key value violates unique constraint …").
	var dupes int
	_ = h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM rate_cards
		  WHERE supplier_id=$1 AND country_iso=$2 AND did_type=$3 AND valid_to IS NULL`,
		id, country, did).Scan(&dupes)
	if dupes > 0 {
		h.flashErr(r, country+" / "+did+" already has an active rate card — expire it first.")
		http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#rates", id), http.StatusFound)
		return
	}

	_, err := h.DB.Exec(r.Context(), `
		INSERT INTO rate_cards (supplier_id, country_iso, did_type, nrc_cents, mrc_cents,
		                        channel_monthly_cents, per_minute_cents,
		                        bill_min_seconds, bill_increment_seconds,
		                        supplier_per_minute_cents, supplier_nrc_cents, supplier_mrc_cents,
		                        supplier_bill_min_seconds, supplier_bill_increment_seconds)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		id, country, did, int(nrc), int(mrc), int(ch), pmin,
		custBillMin, custBillInc,
		supPMin, int(supNRC), int(supMRC), billMin, billInc)
	if err != nil {
		h.flashErr(r, "rate card create: "+err.Error())
	} else {
		h.flashOK(r, "Rate card added")
	}
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d#rates", id), http.StatusFound)
}

// rateCardCreateBulk accepts a JSON array of rate-card rows in one POST so
// admins can copy-paste a price list (e.g. from a supplier email) and create
// many cards at once. Body shape:
//
//	{"rows":[{"country":"US","did_type":"local","nrc":1.0,"mrc":1.5,
//	          "channel":0.5,"per_minute":1.5,
//	          "bill_min_seconds":60,"bill_increment_seconds":60,
//	          "supplier_per_minute":0.4,"supplier_nrc":0.5,"supplier_mrc":0.8,
//	          "supplier_bill_min_seconds":6,"supplier_bill_increment_seconds":6},
//	         …]}
//
// Both *_bill_* pairs default to 60/60 (legacy behaviour) when omitted, so
// existing CSVs keep working without modification.
func (h *Handler) rateCardCreateBulk(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var body struct {
		Rows []struct {
			Country                       string  `json:"country"`
			DIDType                       string  `json:"did_type"`
			NRC, MRC, Channel, PerMinute  float64 `json:"nrc"          form:"nrc"`
			MRC2                          float64 `json:"mrc"`
			Channel2                      float64 `json:"channel"`
			PerMin                        float64 `json:"per_minute"`
			CustBillMin                   int     `json:"bill_min_seconds"`
			CustBillInc                   int     `json:"bill_increment_seconds"`
			SupPerMin                     float64 `json:"supplier_per_minute"`
			SupNRC                        float64 `json:"supplier_nrc"`
			SupMRC                        float64 `json:"supplier_mrc"`
			BillMin                       int     `json:"supplier_bill_min_seconds"`
			BillInc                       int     `json:"supplier_bill_increment_seconds"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback(r.Context())
	created := 0
	for i, row := range body.Rows {
		country := strings.ToUpper(strings.TrimSpace(row.Country))
		did := strings.TrimSpace(row.DIDType)
		if country == "" || did == "" {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("row %d: country and did_type required", i)})
			return
		}
		custBillMin := row.CustBillMin
		if custBillMin == 0 {
			custBillMin = 60
		}
		custBillInc := row.CustBillInc
		if custBillInc == 0 {
			custBillInc = 60
		}
		billMin := row.BillMin
		if billMin == 0 {
			billMin = 60
		}
		billInc := row.BillInc
		if billInc == 0 {
			billInc = 60
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO rate_cards (supplier_id, country_iso, did_type,
			                        nrc_cents, mrc_cents, channel_monthly_cents, per_minute_cents,
			                        bill_min_seconds, bill_increment_seconds,
			                        supplier_per_minute_cents, supplier_nrc_cents, supplier_mrc_cents,
			                        supplier_bill_min_seconds, supplier_bill_increment_seconds)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			id, country, did,
			int(row.NRC*100), int(row.MRC2*100), int(row.Channel2*100), row.PerMin*100,
			custBillMin, custBillInc,
			row.SupPerMin*100, int(row.SupNRC*100), int(row.SupMRC*100),
			billMin, billInc); err != nil {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("row %d: %s", i, err.Error())})
			return
		}
		created++
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]any{"created": created})
}

// writeJSON is a thin helper so this file isn't dependent on the resellerapi
// helper of the same name.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// rateCardUpdate edits an active rate card in place — every cents column
// plus the supplier billing cycle. The (supplier, country, did_type) slot
// keys are locked: changing them would either violate the unique-active
// index or change which orders the card applies to. To replace those, the
// admin should expire and re-create.
//
// CDRs already snapshot the rate at call time (migration 0011), so an
// in-place edit doesn't disturb historical billing.
func (h *Handler) rateCardUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/suppliers", http.StatusFound)
		return
	}

	// Look up the supplier so we can redirect back to its #rates tab.
	var supID int64
	var validTo *time.Time
	if err := h.DB.QueryRow(r.Context(),
		`SELECT supplier_id, valid_to FROM rate_cards WHERE id=$1`, id,
	).Scan(&supID, &validTo); err != nil {
		h.flashErr(r, "rate card not found")
		http.Redirect(w, r, "/suppliers", http.StatusFound)
		return
	}
	back := fmt.Sprintf("/suppliers/%d#rates", supID)
	if validTo != nil {
		h.flashErr(r, "expired rate cards are read-only — create a new one instead")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	// Customer side.
	nrc := dollarsToCents(r.PostForm.Get("nrc"))
	mrc := dollarsToCents(r.PostForm.Get("mrc"))
	ch := dollarsToCents(r.PostForm.Get("channel"))
	pmin := atof(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("per_minute")), "$")) * 100
	custBillMin := atoi(r.PostForm.Get("bill_min_seconds"))
	if custBillMin == 0 {
		custBillMin = 60
	}
	custBillInc := atoi(r.PostForm.Get("bill_increment_seconds"))
	if custBillInc == 0 {
		custBillInc = 60
	}

	// Supplier side.
	supPMin := atof(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("supplier_per_minute")), "$")) * 100
	supNRC := dollarsToCents(r.PostForm.Get("supplier_nrc"))
	supMRC := dollarsToCents(r.PostForm.Get("supplier_mrc"))

	billMin := atoi(r.PostForm.Get("supplier_bill_min_seconds"))
	if billMin == 0 {
		billMin = 60
	}
	billInc := atoi(r.PostForm.Get("supplier_bill_increment_seconds"))
	if billInc == 0 {
		billInc = 60
	}
	// Defensive validation — the DB has a CHECK on these, but a friendlier
	// message here saves a round trip and a generic Postgres error popup.
	allowed := map[int]bool{1: true, 6: true, 15: true, 30: true, 60: true}
	if !allowed[billMin] || !allowed[billInc] {
		h.flashErr(r, "supplier bill cycle must be 1/1, 6/6, 15/15, 30/30, or 60/60")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	if !allowed[custBillMin] || !allowed[custBillInc] {
		h.flashErr(r, "customer bill cycle must be 1/1, 6/6, 15/15, 30/30, or 60/60")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	if pmin < 0 || nrc < 0 || mrc < 0 || ch < 0 || supPMin < 0 || supNRC < 0 || supMRC < 0 {
		h.flashErr(r, "rate values must be non-negative")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	if _, err := h.DB.Exec(r.Context(), `
		UPDATE rate_cards
		   SET nrc_cents=$1, mrc_cents=$2, channel_monthly_cents=$3, per_minute_cents=$4,
		       bill_min_seconds=$5, bill_increment_seconds=$6,
		       supplier_per_minute_cents=$7, supplier_nrc_cents=$8, supplier_mrc_cents=$9,
		       supplier_bill_min_seconds=$10, supplier_bill_increment_seconds=$11
		 WHERE id=$12`,
		int(nrc), int(mrc), int(ch), pmin,
		custBillMin, custBillInc,
		supPMin, int(supNRC), int(supMRC), billMin, billInc, id); err != nil {
		h.flashErr(r, "save: "+err.Error())
	} else {
		h.flashOK(r, "Rate card saved.")
	}
	http.Redirect(w, r, back, http.StatusFound)
}

func (h *Handler) rateCardExpire(w http.ResponseWriter, r *http.Request) {
	rcID := pathID(r, "id")
	var supID int64
	h.DB.QueryRow(r.Context(), `UPDATE rate_cards SET valid_to = now() WHERE id=$1 RETURNING supplier_id`, rcID).Scan(&supID)
	h.flashOK(r, "Rate card expired")
	http.Redirect(w, r, fmt.Sprintf("/suppliers/%d", supID), http.StatusFound)
}

// =====================================================================
// DIDS
// =====================================================================

func (h *Handler) didImport(w http.ResponseWriter, r *http.Request) {
	type sup struct {
		ID   int64
		Name string
	}
	var sups []sup
	rows, _ := h.DB.Query(r.Context(), `SELECT id, name FROM suppliers ORDER BY name`)
	for rows.Next() {
		var s sup
		rows.Scan(&s.ID, &s.Name)
		sups = append(sups, s)
	}
	rows.Close()
	ok, em := h.popFlashes(r)
	h.render(w, "did_import", map[string]any{
		"Title":     "Import DIDs",
		"Section":   "dids",
		"FlashOK":   ok,
		"FlashErr":  em,
		"Suppliers": sups,
		"Countries": h.listCountries(r),
	})
}

func (h *Handler) didImportSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	supID := atoi64(r.PostForm.Get("supplier_id"))
	country := strings.ToUpper(r.PostForm.Get("country_iso"))
	dtype := r.PostForm.Get("did_type")
	startStr := strings.TrimSpace(r.PostForm.Get("start_e164"))
	endStr := strings.TrimSpace(r.PostForm.Get("end_e164"))
	// Same -1 sentinel convention as users.global_channel_cap.
	supplierCap := -1
	if c := strings.TrimSpace(r.PostForm.Get("supplier_channel_cap")); c != "" {
		supplierCap = atoi(c)
		if supplierCap < 0 {
			supplierCap = -1
		}
	}

	start := atoi64(startStr)
	end := start
	if endStr != "" {
		end = atoi64(endStr)
	}
	if start <= 0 || end < start {
		h.flashErr(r, "bad range")
		http.Redirect(w, r, "/dids/import", http.StatusFound)
		return
	}
	if end-start > 10000 {
		h.flashErr(r, "range too large (max 10,000)")
		http.Redirect(w, r, "/dids/import", http.StatusFound)
		return
	}

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, "/dids/import", http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	inserted := 0
	for n := start; n <= end; n++ {
		_, err := tx.Exec(r.Context(),
			`INSERT INTO dids (e164, supplier_id, country_iso, did_type, supplier_channel_cap, status)
			 VALUES ($1,$2,$3,$4,$5,'available') ON CONFLICT (e164) DO NOTHING`,
			fmt.Sprintf("%d", n), supID, country, dtype, supplierCap)
		if err != nil {
			h.flashErr(r, "import: "+err.Error())
			http.Redirect(w, r, "/dids/import", http.StatusFound)
			return
		}
		inserted++
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, err.Error())
	} else {
		h.flashOK(r, fmt.Sprintf("Imported %d DIDs (existing skipped)", inserted))
	}
	http.Redirect(w, r, "/dids", http.StatusFound)
}

func (h *Handler) didRetire(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	back := safeReturnTo(r, "/dids")
	var assigned int
	h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM orders WHERE did_id=$1 AND status IN ('active','kyc_pending','quarantined')`, id).Scan(&assigned)
	if assigned > 0 {
		h.flashErr(r, "DID has an order — cancel the order first")
	} else {
		_, err := h.DB.Exec(r.Context(), `UPDATE dids SET status='retired' WHERE id=$1`, id)
		if err != nil {
			h.flashErr(r, "retire: "+err.Error())
		} else {
			h.flashOK(r, "DID retired")
		}
	}
	http.Redirect(w, r, back, http.StatusFound)
}

// =====================================================================
// USERS  (customer-level: balance, KYC, channel cap, sip accounts)
// =====================================================================

func (h *Handler) userNew(w http.ResponseWriter, r *http.Request) {
	type res struct {
		ID   int64
		Name string
	}
	var resellers []res
	rows, _ := h.DB.Query(r.Context(), `SELECT id, name FROM resellers ORDER BY name`)
	for rows.Next() {
		var x res
		rows.Scan(&x.ID, &x.Name)
		resellers = append(resellers, x)
	}
	rows.Close()
	ok, em := h.popFlashes(r)
	h.render(w, "user_edit", map[string]any{
		"Title":     "New user",
		"Section":   "users",
		"FlashOK":   ok,
		"FlashErr":  em,
		"IsNew":     true,
		"User":      map[string]any{"ExternalID": "", "Label": "", "ContactEmail": "", "Status": "active"},
		"Resellers": resellers,
	})
}

func (h *Handler) userCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/users/new", http.StatusFound)
		return
	}
	extID := strings.TrimSpace(r.PostForm.Get("external_id"))
	label := strings.TrimSpace(r.PostForm.Get("label"))
	contact := strings.TrimSpace(r.PostForm.Get("contact_email"))
	var resID *int64
	if rid := atoi64(r.PostForm.Get("reseller_id")); rid > 0 {
		resID = &rid
	}
	// Channel cap convention: -1 (or blank) means uncapped. Any other negative
	// value normalizes to -1.
	cap := -1
	if c := strings.TrimSpace(r.PostForm.Get("global_channel_cap")); c != "" {
		cap = atoi(c)
		if cap < 0 {
			cap = -1
		}
	}

	if extID == "" && label == "" && contact == "" {
		h.flashErr(r, "at least one of external_id, label, or contact_email is required")
		http.Redirect(w, r, "/users/new", http.StatusFound)
		return
	}

	var extPtr, labelPtr, contactPtr *string
	if extID != "" {
		extPtr = &extID
	}
	if label != "" {
		labelPtr = &label
	}
	if contact != "" {
		contactPtr = &contact
	}

	var id int64
	err := h.DB.QueryRow(r.Context(), `
		INSERT INTO users (external_id, label, contact_email, balance_cents, reseller_id,
		                   global_channel_cap, status)
		VALUES ($1,$2,$3,0,$4,$5,'active') RETURNING id`,
		extPtr, labelPtr, contactPtr, resID, cap,
	).Scan(&id)
	if err != nil {
		h.flashErr(r, "create: "+err.Error())
		http.Redirect(w, r, "/users/new", http.StatusFound)
		return
	}
	h.flashOK(r, "User created")
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

func (h *Handler) userDetail(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var u struct {
		ID                                 int64
		ExternalID, Label, ContactEmail    string
		Status, Reseller, GlobalChannelCap string
		Created                            string
		BalanceCents                       int64
		ResellerID                         int64
	}
	var capN int
	err := h.DB.QueryRow(r.Context(), `
		SELECT u.id,
		       COALESCE(u.external_id,''),
		       COALESCE(u.label,''),
		       COALESCE(u.contact_email,''),
		       u.status,
		       COALESCE(re.name,''),
		       COALESCE(u.reseller_id, 0),
		       u.balance_cents,
		       u.global_channel_cap,
		       to_char(u.created_at,'YYYY-MM-DD HH24:MI')
		  FROM users u LEFT JOIN resellers re ON re.id=u.reseller_id
		 WHERE u.id=$1`, id,
	).Scan(&u.ID, &u.ExternalID, &u.Label, &u.ContactEmail, &u.Status, &u.Reseller, &u.ResellerID, &u.BalanceCents, &capN, &u.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}
	if capN < 0 {
		u.GlobalChannelCap = "" // template renders blank → "∞"
	} else {
		u.GlobalChannelCap = strconv.Itoa(capN)
	}

	type orderRow struct {
		ID                                   int64
		E164, Status, RouteKind, RouteTarget string
		ChannelCount                         int
		AnniversaryDay                       int
		KycBundleID                          int64
		NextBillingAt                        string
		KycStatus                            string
	}
	var orders []orderRow
	orows, _ := h.DB.Query(r.Context(), `
		SELECT o.id, d.e164, o.status::text, o.route_kind::text, o.route_target,
		       o.channel_count, o.anniversary_day, COALESCE(o.kyc_bundle_id, 0),
		       to_char(o.next_billing_at,'YYYY-MM-DD'),
		       COALESCE((SELECT b.status::text FROM kyc_bundles b WHERE b.id = o.kyc_bundle_id), '')
		  FROM orders o JOIN dids d ON d.id=o.did_id
		 WHERE o.user_id=$1 ORDER BY o.id DESC`, id)
	for orows.Next() {
		var x orderRow
		orows.Scan(&x.ID, &x.E164, &x.Status, &x.RouteKind, &x.RouteTarget,
			&x.ChannelCount, &x.AnniversaryDay, &x.KycBundleID,
			&x.NextBillingAt, &x.KycStatus)
		orders = append(orders, x)
	}
	orows.Close()

	type ledgerRow struct {
		Created    string
		Delta      int64
		Kind       string
		Balance    int64
		RefTable   string // 'cdrs' | 'orders' | 'billing_runs' | ''  (source table of ref_id)
		RefID      int64  // row id in RefTable
		RefDID     string // E.164 resolved from the join (when available)
		RefCallID  string // call_id for cdr-backed ledger entries
		RefOrderID int64  // order_id resolved for cdrs / billing_runs entries (links back to /orders/{id})
	}
	var ledger []ledgerRow
	lrows, _ := h.DB.Query(r.Context(), `
		SELECT to_char(l.created_at,'YYYY-MM-DD HH24:MI'),
		       l.delta_cents, l.kind::text, l.balance_after,
		       COALESCE(l.ref_table, ''),
		       COALESCE(l.ref_id, 0),
		       CASE l.ref_table
		         WHEN 'cdrs'         THEN COALESCE((SELECT d.e164
		                                              FROM cdrs c LEFT JOIN dids d ON d.id = c.did_id
		                                             WHERE c.id = l.ref_id), '')
		         WHEN 'orders'       THEN COALESCE((SELECT d.e164
		                                              FROM orders o JOIN dids d ON d.id = o.did_id
		                                             WHERE o.id = l.ref_id), '')
		         WHEN 'billing_runs' THEN COALESCE((SELECT d.e164
		                                              FROM billing_runs br
		                                              JOIN orders o ON o.id = br.order_id
		                                              JOIN dids d ON d.id = o.did_id
		                                             WHERE br.id = l.ref_id), '')
		         ELSE ''
		       END AS ref_did,
		       CASE l.ref_table
		         WHEN 'cdrs' THEN COALESCE((SELECT c.call_id FROM cdrs c WHERE c.id = l.ref_id), '')
		         ELSE ''
		       END AS ref_call_id,
		       CASE l.ref_table
		         WHEN 'orders'       THEN COALESCE(l.ref_id, 0)
		         WHEN 'billing_runs' THEN COALESCE((SELECT br.order_id FROM billing_runs br WHERE br.id = l.ref_id), 0)
		         WHEN 'cdrs'         THEN COALESCE((SELECT c.order_id FROM cdrs c WHERE c.id = l.ref_id), 0)
		         ELSE 0
		       END AS ref_order_id
		  FROM balance_ledger l
		 WHERE l.user_id = $1
		 ORDER BY l.id DESC
		 LIMIT 50`, id)
	for lrows.Next() {
		var x ledgerRow
		lrows.Scan(&x.Created, &x.Delta, &x.Kind, &x.Balance,
			&x.RefTable, &x.RefID, &x.RefDID, &x.RefCallID, &x.RefOrderID)
		ledger = append(ledger, x)
	}
	lrows.Close()

	type sipAcct struct {
		ID       int64
		Username string
		Realm    string
		Created  string
	}
	var sipAccts []sipAcct
	srows, _ := h.DB.Query(r.Context(), `
		SELECT id, username, realm, to_char(created_at,'YYYY-MM-DD HH24:MI')
		  FROM sip_accounts WHERE user_id=$1 ORDER BY id`, id)
	for srows.Next() {
		var s sipAcct
		srows.Scan(&s.ID, &s.Username, &s.Realm, &s.Created)
		sipAccts = append(sipAccts, s)
	}
	srows.Close()

	type kycRow struct {
		ID                int64
		Type, Status      string
		Created, Approved string
		DocCount          int
	}
	var kyc []kycRow
	krows, _ := h.DB.Query(r.Context(), `
		SELECT b.id, b.type::text, b.status::text,
		       to_char(b.created_at,'YYYY-MM-DD HH24:MI'),
		       COALESCE(to_char(b.approved_at,'YYYY-MM-DD HH24:MI'),''),
		       (SELECT count(*) FROM kyc_documents WHERE bundle_id = b.id)
		  FROM kyc_bundles b WHERE b.user_id=$1 ORDER BY b.id DESC`, id)
	for krows.Next() {
		var x kycRow
		krows.Scan(&x.ID, &x.Type, &x.Status, &x.Created, &x.Approved, &x.DocCount)
		kyc = append(kyc, x)
	}
	krows.Close()

	type did struct {
		ID                  int64
		E164, Country, Type string
	}
	var availDIDs []did
	drows, _ := h.DB.Query(r.Context(), `
		SELECT id, e164, country_iso, did_type::text FROM dids
		 WHERE status='available' ORDER BY e164 LIMIT 200`)
	for drows.Next() {
		var x did
		drows.Scan(&x.ID, &x.E164, &x.Country, &x.Type)
		availDIDs = append(availDIDs, x)
	}
	drows.Close()

	heading := u.ExternalID
	if heading == "" {
		heading = u.Label
	}
	if heading == "" {
		heading = u.ContactEmail
	}
	if heading == "" {
		heading = fmt.Sprintf("#%d", u.ID)
	}

	blocks := h.blockHistoryFor(r.Context(), id)

	// CDR list embedded in the #cdrs tab. Filter params are prefixed with
	// `cdr_` so they don't collide with anything else on the page.
	type cdrRow struct {
		CallID                                       string
		Started, RoutedKind, RoutedTarget, SrcURI    string
		DID                                          string
		HangupCause, State                           string
		Billsec, ChargedMinutes                      int
		ChargeDollars                                float64
		OrderID                                      int64
	}
	cdrFrom := r.URL.Query().Get("cdr_from")
	cdrTo := r.URL.Query().Get("cdr_to")
	cdrState := r.URL.Query().Get("cdr_state")
	cdrLimit := 100
	if v := r.URL.Query().Get("cdr_limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			cdrLimit = n
		}
	}
	cargs := []any{id}
	cwhere := " WHERE c.user_id = $1"
	if cdrFrom != "" {
		if t, err := time.Parse("2006-01-02", cdrFrom); err == nil {
			cargs = append(cargs, t)
			cwhere += fmt.Sprintf(" AND c.started_at >= $%d", len(cargs))
		}
	}
	if cdrTo != "" {
		if t, err := time.Parse("2006-01-02", cdrTo); err == nil {
			cargs = append(cargs, t.Add(24*time.Hour))
			cwhere += fmt.Sprintf(" AND c.started_at < $%d", len(cargs))
		}
	}
	switch cdrState {
	case "answered":
		cwhere += " AND c.billsec > 0"
	case "failed":
		cwhere += " AND c.billsec = 0"
	}
	cargs = append(cargs, cdrLimit)
	cdrSQL := `
		SELECT c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       COALESCE(c.routed_kind::text, COALESCE(o.route_kind::text,'')),
		       COALESCE(c.routed_target,    COALESCE(o.route_target,'')),
		       COALESCE(c.src_uri,''),
		       COALESCE(d.e164,''),
		       COALESCE(c.hangup_cause,''),
		       c.billsec, c.charged_minutes, c.charge_cents,
		       COALESCE(c.order_id, 0)
		  FROM cdrs c
		  LEFT JOIN orders o ON o.id = c.order_id
		  LEFT JOIN dids   d ON d.id = COALESCE(c.did_id, o.did_id)` +
		cwhere + fmt.Sprintf(" ORDER BY c.started_at DESC LIMIT $%d", len(cargs))
	var cdrs []cdrRow
	if rows, err := h.DB.Query(r.Context(), cdrSQL, cargs...); err == nil {
		for rows.Next() {
			var x cdrRow
			var cents int
			if err := rows.Scan(&x.CallID, &x.Started, &x.RoutedKind, &x.RoutedTarget,
				&x.SrcURI, &x.DID, &x.HangupCause, &x.Billsec, &x.ChargedMinutes, &cents, &x.OrderID); err == nil {
				x.ChargeDollars = float64(cents) / 100
				x.State = domain.CallState(x.Billsec, x.HangupCause)
				cdrs = append(cdrs, x)
			}
		}
		rows.Close()
	}

	ok, em := h.popFlashes(r)
	h.render(w, "user_detail", map[string]any{
		"Title":          "User · " + heading,
		"Section":        "users",
		"FlashOK":        ok,
		"FlashErr":       em,
		"User":           u,
		"Heading":        heading,
		"BalanceDollars": float64(u.BalanceCents) / 100,
		"Orders":         orders,
		"Ledger":         ledger,
		"AvailableDIDs":  availDIDs,
		"SIPAccounts":    sipAccts,
		"KYCBundles":     kyc,
		"BlockHistory":   blocks,
		"CDRs":           cdrs,
		"CDRFrom":        cdrFrom,
		"CDRTo":          cdrTo,
		"CDRState":       cdrState,
		"CDRLimit":       cdrLimit,
		"SIPRealm":       sipRealm,
	})
}

func (h *Handler) userUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	status := nonempty(r.PostForm.Get("status"), "active")
	extID := strings.TrimSpace(r.PostForm.Get("external_id"))
	label := strings.TrimSpace(r.PostForm.Get("label"))
	contact := strings.TrimSpace(r.PostForm.Get("contact_email"))
	// Channel cap convention: -1 (or blank) means uncapped. Any other negative
	// value normalizes to -1.
	cap := -1
	if c := strings.TrimSpace(r.PostForm.Get("global_channel_cap")); c != "" {
		cap = atoi(c)
		if cap < 0 {
			cap = -1
		}
	}

	var extPtr, labelPtr, contactPtr *string
	if extID != "" {
		extPtr = &extID
	}
	if label != "" {
		labelPtr = &label
	}
	if contact != "" {
		contactPtr = &contact
	}

	_, err := h.DB.Exec(r.Context(), `
		UPDATE users
		   SET status=$1, external_id=$2, label=$3, contact_email=$4, global_channel_cap=$5
		 WHERE id=$6`, status, extPtr, labelPtr, contactPtr, cap, id)
	if err != nil {
		h.flashErr(r, err.Error())
	} else {
		h.flashOK(r, "User saved")
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// userDelete is a destructive admin action. It cancels every order (returning
// DIDs to the pool), then deletes the user. Cascade FKs on sip_accounts,
// kyc_bundles (with their kyc_documents), balance_ledger and user_block_log
// drop the dependent rows automatically; cdrs.user_id / cdrs.order_id are
// SET NULL so traffic history survives unlinked.
//
// Disk-side KYC files are removed best-effort after the DB commit.
func (h *Handler) userDelete(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	r.ParseForm()
	confirm := strings.TrimSpace(r.PostForm.Get("confirm_id"))
	if confirm != fmt.Sprintf("%d", uid) {
		h.flashErr(r, "delete confirmation didn't match user id")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	// 1) Cancel every active/quarantined order, capturing did ids so we can
	// flip them back to 'available'.
	rows, err := tx.Query(r.Context(), `
		UPDATE orders SET status='cancelled', ended_at=now()
		 WHERE user_id=$1 AND status IN ('active','kyc_pending','quarantined','suspended')
		 RETURNING did_id`, uid)
	if err != nil {
		h.flashErr(r, "cancel orders: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	var didIDs []int64
	for rows.Next() {
		var d int64
		rows.Scan(&d)
		didIDs = append(didIDs, d)
	}
	rows.Close()
	for _, d := range didIDs {
		if _, err := tx.Exec(r.Context(),
			`UPDATE dids SET status='available' WHERE id=$1`, d); err != nil {
			h.flashErr(r, "release did "+fmt.Sprintf("%d", d)+": "+err.Error())
			http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
			return
		}
	}

	// 2) Delete the user. FK cascades / SET NULLs handle the rest.
	tag, err := tx.Exec(r.Context(), `DELETE FROM users WHERE id=$1`, uid)
	if err != nil {
		h.flashErr(r, "delete user: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "user not found")
		http.Redirect(w, r, "/users", http.StatusFound)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "commit: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	// 3) Best-effort: remove the on-disk KYC tree for this user.
	_ = os.RemoveAll(fmt.Sprintf("/var/lib/didstorage/kyc/%d", uid))

	// 4) PJSIP needs a reload — the user's sip_accounts are gone.
	go h.regenPJSIPUsers(r)

	h.flashOK(r, fmt.Sprintf("Deleted user #%d (cancelled %d order(s); DIDs returned to pool)", uid, len(didIDs)))
	http.Redirect(w, r, "/users", http.StatusFound)
}

func (h *Handler) userTopup(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	cents := dollarsToCents(r.PostForm.Get("amount"))
	if cents == 0 {
		h.flashErr(r, "amount > 0 required")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())
	var newBal int64
	err = tx.QueryRow(r.Context(),
		`UPDATE users SET balance_cents = balance_cents + $1 WHERE id=$2 RETURNING balance_cents`,
		cents, id).Scan(&newBal)
	if err == nil {
		_, err = tx.Exec(r.Context(),
			`INSERT INTO balance_ledger (user_id, delta_cents, kind, balance_after) VALUES ($1,$2,'topup',$3)`,
			id, cents, newBal)
	}
	if err != nil {
		h.flashErr(r, err.Error())
	} else {
		tx.Commit(r.Context())
		h.flashOK(r, fmt.Sprintf("Topped up $%.2f", float64(cents)/100))
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// =====================================================================
// ORDERS  (per-DID rentals)
// =====================================================================

// orderCreate creates a new per-DID order for a user. Form-posted from the
// user detail page. NRC charges immediately to the user's balance.
//
// If the user has an approved KYC bundle and the form supplies kyc_bundle_id,
// the order goes to 'active'. Otherwise it goes to 'kyc_pending' and stays
// there until an admin approves a bundle and attaches it.
func (h *Handler) orderCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	userID := atoi64(r.PostForm.Get("user_id"))
	didID := atoi64(r.PostForm.Get("did_id"))
	channels := atoi(r.PostForm.Get("channel_count"))
	if channels < 0 {
		channels = 0
	}
	routeKind := r.PostForm.Get("route_kind")
	routeTarget := normalizeRouteTarget(routeKind, r.PostForm.Get("route_target"))
	annDay := atoi(r.PostForm.Get("anniversary_day"))
	if annDay < 1 || annDay > 28 {
		annDay = time.Now().UTC().Day()
		if annDay > 28 {
			annDay = 28
		}
	}
	var kycBundleID *int64
	if v := atoi64(r.PostForm.Get("kyc_bundle_id")); v > 0 {
		kycBundleID = &v
	}

	if userID == 0 || didID == 0 || routeKind == "" || routeTarget == "" {
		h.flashErr(r, "user, did, route_kind and route_target are required")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
		return
	}

	// route_kind=sip_account → the target must be one of this user's peers.
	if routeKind == "sip_account" {
		var ok bool
		_ = h.DB.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM sip_accounts WHERE user_id=$1 AND username=$2)`,
			userID, routeTarget).Scan(&ok)
		if !ok {
			h.flashErr(r, "sip_account target must be one of this user's SIP peers — create one in the Peers tab first")
			http.Redirect(w, r, fmt.Sprintf("/users/%d#peers", userID), http.StatusFound)
			return
		}
	}

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	var rateCardID int64
	var nrc int
	err = tx.QueryRow(r.Context(), `
		SELECT rc.id, rc.nrc_cents
		  FROM dids d
		  JOIN rate_cards rc ON rc.supplier_id=d.supplier_id
		                    AND rc.country_iso=d.country_iso
		                    AND rc.did_type=d.did_type
		                    AND rc.valid_to IS NULL
		 WHERE d.id=$1
		 ORDER BY rc.valid_from DESC LIMIT 1`, didID).Scan(&rateCardID, &nrc)
	if err != nil {
		h.flashErr(r, "no active rate card for this DID's (supplier,country,type)")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
		return
	}

	// Lock and read the user's balance.
	var bal int64
	tx.QueryRow(r.Context(), `SELECT balance_cents FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&bal)
	if bal < int64(nrc) {
		h.flashErr(r, fmt.Sprintf("insufficient balance: NRC is $%.2f, balance is $%.2f", float64(nrc)/100, float64(bal)/100))
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
		return
	}

	// If a KYC bundle is supplied, verify it belongs to the user and is approved.
	startStatus := "kyc_pending"
	if kycBundleID != nil {
		var bUser int64
		var bStatus string
		err := tx.QueryRow(r.Context(),
			`SELECT user_id, status::text FROM kyc_bundles WHERE id=$1`, *kycBundleID,
		).Scan(&bUser, &bStatus)
		if err != nil || bUser != userID {
			h.flashErr(r, "invalid KYC bundle")
			http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
			return
		}
		if bStatus == "approved" {
			startStatus = "active"
		}
	}

	now := time.Now().UTC()
	next := domain.NextAnniversary(now, annDay)
	var oid int64
	err = tx.QueryRow(r.Context(), `
		INSERT INTO orders (did_id, user_id, channel_count, route_kind, route_target,
		                    rate_card_id, anniversary_day, next_billing_at, status, kyc_bundle_id)
		VALUES ($1,$2,$3,$4::route_kind,$5,$6,$7,$8,$9::assignment_status,$10) RETURNING id`,
		didID, userID, channels, routeKind, routeTarget, rateCardID, annDay, next, startStatus, kycBundleID,
	).Scan(&oid)
	if err != nil {
		h.flashErr(r, "create order: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE dids SET status='assigned' WHERE id=$1`, didID); err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
		return
	}

	if nrc > 0 {
		newBal := bal - int64(nrc)
		tx.Exec(r.Context(), `UPDATE users SET balance_cents=$1 WHERE id=$2`, newBal, userID)
		tx.Exec(r.Context(),
			`INSERT INTO balance_ledger (user_id, delta_cents, kind, ref_table, ref_id, balance_after)
			 VALUES ($1, $2, 'nrc', 'orders', $3, $4)`,
			userID, -int64(nrc), oid, newBal)
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, err.Error())
	} else {
		msg := fmt.Sprintf("DID assigned (NRC $%.2f charged)", float64(nrc)/100)
		if startStatus == "kyc_pending" {
			msg += " — order is in KYC-pending state until a KYC bundle is approved"
		}
		h.flashOK(r, msg)
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
}

// orderUpdate is the back end of the per-order edit modal. It accepts the
// full set of editable fields and validates each before writing. Any field
// the form sends as empty string is treated as "unchanged" (the modal
// pre-fills with current values, so the user only sees blanks when nothing
// is set, e.g. kyc_bundle_id).
//
// Only orders in active / kyc_pending / quarantined are editable.
// Suspended / cancelled orders are read-only.
func (h *Handler) orderUpdate(w http.ResponseWriter, r *http.Request) {
	oid := pathID(r, "id")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/orders", http.StatusFound)
		return
	}

	// --- pull form values ---
	rk := strings.TrimSpace(r.PostForm.Get("route_kind"))
	rtRaw := strings.TrimSpace(r.PostForm.Get("route_target"))
	afRaw := strings.TrimSpace(r.PostForm.Get("audio_file_id"))   // route_kind=audio
	agRaw := strings.TrimSpace(r.PostForm.Get("audio_group_id"))  // route_kind=audio_group
	chRaw := strings.TrimSpace(r.PostForm.Get("channel_count"))
	annRaw := strings.TrimSpace(r.PostForm.Get("anniversary_day"))
	kycRaw := strings.TrimSpace(r.PostForm.Get("kyc_bundle_id"))
	clearKyc := r.PostForm.Get("kyc_bundle_clear") == "1"

	// --- load current state so unchanged fields can stay as-is ---
	var (
		curUserID, curRateCardID                int64
		curStatus                               string
		curRouteKind, curRouteTarget            string
		curChannels, curAnnDay                  int
		curKycBundleID                          *int64
		curAudioGroupID                         *int64
	)
	err := h.DB.QueryRow(r.Context(), `
		SELECT o.user_id, o.rate_card_id, o.status::text,
		       o.route_kind::text, o.route_target,
		       o.channel_count, o.anniversary_day, o.kyc_bundle_id,
		       o.audio_group_id
		  FROM orders o WHERE o.id = $1`, oid,
	).Scan(&curUserID, &curRateCardID, &curStatus,
		&curRouteKind, &curRouteTarget,
		&curChannels, &curAnnDay, &curKycBundleID,
		&curAudioGroupID)
	if err != nil {
		h.flashErr(r, "order not found")
		http.Redirect(w, r, "/orders", http.StatusFound)
		return
	}

	// return_to lets the order-detail Route/KYC tabs round-trip back to
	// themselves after a save. Default keeps the old behaviour (user-page
	// orders tab) so existing callers don't change. safeReturnTo enforces
	// the same-origin guard used by /dids/{id}/release.
	back := safeReturnTo(r, fmt.Sprintf("/users/%d#orders", curUserID))
	if curStatus != "active" && curStatus != "kyc_pending" && curStatus != "quarantined" {
		h.flashErr(r, "order is "+curStatus+" and can't be edited (cancel and create a new one)")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	// --- validate + decide each field ---
	// route_kind: must be one of the enum values; default to current.
	if rk == "" {
		rk = curRouteKind
	}
	switch rk {
	case "sip_uri", "ip", "sip_account", "audio", "audio_group":
		// ok
	default:
		h.flashErr(r, "invalid route_kind: "+rk)
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	// route_target derivation depends on route_kind. SIP kinds normalize
	// the raw form input; audio kinds derive route_target from a picker
	// (audio_file_id → "didstorage/<basename>") or, for audio_group, leave
	// route_target empty and use the FK on audio_group_id.
	rt := curRouteTarget
	var nextAudioGroupID *int64
	switch rk {
	case "audio":
		// audio kind: form posts audio_file_id; we look up the file's
		// on-disk basename and bake the "didstorage/<basename>" target
		// the AGI expects. Same shape sipctl uses for reservations.
		// If the form omits audio_file_id (operator editing channels-only),
		// keep the current route_target.
		afID, _ := strconv.ParseInt(afRaw, 10, 64)
		if afID > 0 {
			var filename string
			if err := h.DB.QueryRow(r.Context(),
				`SELECT filename FROM audio_files WHERE id = $1`, afID).Scan(&filename); err != nil {
				h.flashErr(r, "That audio clip no longer exists. Pick another.")
				http.Redirect(w, r, back, http.StatusFound)
				return
			}
			rt = "didstorage/" + filename
		}
		if rt == "" {
			h.flashErr(r, "Pick an audio clip from the dropdown.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		nextAudioGroupID = nil
	case "audio_group":
		// audio_group kind: store the FK; route_target stays empty (sipctl
		// resolves to a concrete clip per INVITE). If the form omits
		// audio_group_id, keep the existing FK.
		agID, _ := strconv.ParseInt(agRaw, 10, 64)
		if agID > 0 {
			var members int
			if err := h.DB.QueryRow(r.Context(),
				`SELECT count(*) FROM audio_group_members WHERE group_id = $1`, agID,
			).Scan(&members); err != nil {
				h.flashErr(r, "Audio group lookup failed.")
				http.Redirect(w, r, back, http.StatusFound)
				return
			}
			if members == 0 {
				h.flashErr(r, "That audio group is empty. Add clips to it first.")
				http.Redirect(w, r, back, http.StatusFound)
				return
			}
			nextAudioGroupID = &agID
		} else {
			nextAudioGroupID = curAudioGroupID
		}
		if nextAudioGroupID == nil {
			h.flashErr(r, "Pick an audio group from the dropdown.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		rt = ""
	default:
		// SIP kinds: normalize whatever the form posted.
		if rtRaw != "" {
			rt = normalizeRouteTarget(rk, rtRaw)
		}
		if rt == "" {
			h.flashErr(r, "route_target is required")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		nextAudioGroupID = nil
	}

	// channel_count: 0..8192. 0 is intentional (= deny all calls, useful for
	// testing and for temporarily pausing a DID without cancelling.)
	ch := curChannels
	if chRaw != "" {
		v, err := strconv.Atoi(chRaw)
		if err != nil || v < 0 || v > 8192 {
			h.flashErr(r, "channel_count must be an integer between 0 and 8192")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		ch = v
	}

	// anniversary_day: 1..28 (PG-month-safe).
	annDay := curAnnDay
	if annRaw != "" {
		v, err := strconv.Atoi(annRaw)
		if err != nil || v < 1 || v > 28 {
			h.flashErr(r, "anniversary_day must be 1–28")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		annDay = v
	}

	// route_kind=sip_account: target must be a SIP peer that belongs to the
	// order's user. We never let one user route to another user's peer.
	if rk == "sip_account" {
		var ok bool
		if err := h.DB.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM sip_accounts WHERE user_id=$1 AND username=$2)`,
			curUserID, rt).Scan(&ok); err != nil || !ok {
			h.flashErr(r, "sip_account target must be one of this user's SIP peers (create one in the Peers tab)")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
	}

	// kyc_bundle_id: must belong to this order's user. Either keep current,
	// switch to a different bundle, or explicitly clear (kyc_bundle_clear=1).
	var nextKyc *int64
	switch {
	case clearKyc:
		nextKyc = nil
	case kycRaw != "":
		v, err := strconv.ParseInt(kycRaw, 10, 64)
		if err != nil || v <= 0 {
			h.flashErr(r, "invalid kyc_bundle_id")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		var owner int64
		if err := h.DB.QueryRow(r.Context(),
			`SELECT user_id FROM kyc_bundles WHERE id=$1`, v).Scan(&owner); err != nil {
			h.flashErr(r, "kyc bundle not found")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		if owner != curUserID {
			h.flashErr(r, "kyc bundle does not belong to this user")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		nextKyc = &v
	default:
		nextKyc = curKycBundleID
	}

	// --- apply ---
	// If the admin attached an already-approved bundle to a kyc_pending
	// order, the order should flip to active inside the same UPDATE. This
	// mirrors what orderCreate does at INSERT time; without it, /sipctl/
	// authorize never sees an active order for the DID and inbound calls
	// land on the did_not_assigned deny path (which sends SIP 403 and a
	// "could not connect" prompt from the upstream carrier).
	//
	// The decision uses a subquery against kyc_bundles so we don't need a
	// second roundtrip — the CASE evaluates per-row against current state.
	// route_target is NULLable for audio_group (the FK on audio_group_id
	// carries the routing info; route_target stays empty). For SIP kinds
	// and audio_clip, route_target IS the target and must be set.
	var rtArg any = rt
	if rk == "audio_group" {
		rtArg = nil
	}
	if _, err := h.DB.Exec(r.Context(), `
		UPDATE orders o
		   SET route_kind     = $1::route_kind,
		       route_target   = $2,
		       channel_count  = $3,
		       anniversary_day= $4,
		       kyc_bundle_id  = $5,
		       audio_group_id = $7,
		       status = CASE
		         WHEN o.status = 'kyc_pending'::assignment_status
		              AND $5::bigint IS NOT NULL
		              AND EXISTS (SELECT 1 FROM kyc_bundles b
		                           WHERE b.id = $5 AND b.status = 'approved')
		           THEN 'active'::assignment_status
		         ELSE o.status
		       END
		 WHERE o.id = $6`,
		rk, rtArg, ch, annDay, nextKyc, oid, nextAudioGroupID); err != nil {
		h.flashErr(r, "save: "+err.Error())
	} else {
		h.flashOK(r, fmt.Sprintf("Order #%d saved", oid))
	}
	http.Redirect(w, r, back, http.StatusFound)
}

func (h *Handler) orderCancel(w http.ResponseWriter, r *http.Request) {
	oid := pathID(r, "id")
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, "/orders", http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())
	var userID, didID int64
	err = tx.QueryRow(r.Context(), `
		UPDATE orders SET status='cancelled', ended_at=now()
		 WHERE id=$1 AND status IN ('active','kyc_pending','quarantined','suspended')
		 RETURNING user_id, did_id`, oid).Scan(&userID, &didID)
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE dids SET status='available' WHERE id=$1`, didID)
	}
	if err != nil {
		h.flashErr(r, err.Error())
	} else {
		tx.Commit(r.Context())
		h.flashOK(r, "Order cancelled, DID returned to pool")
	}
	if userID > 0 {
		http.Redirect(w, r, fmt.Sprintf("/users/%d", userID), http.StatusFound)
	} else {
		http.Redirect(w, r, "/orders", http.StatusFound)
	}
}

// =====================================================================
// RESELLERS + API KEYS
// =====================================================================

func (h *Handler) resellers(w http.ResponseWriter, r *http.Request) {
	type rsl struct {
		ID                                                                          int64
		Name, BrandName, Hostname                                                   string
		Status                                                                      string
		Created                                                                     string
		UserCount, ActiveOrders                                                     int
		ActiveAPIKeys                                                               int
		BalanceDollars                                                              float64
		Revenue30dDollars, Profit30dDollars, RevenueLifeDollars, ProfitLifeDollars  float64
		HasProfit30d, HasProfitLife                                                 bool
	}
	allowedSorts := map[string]string{
		"id":     "re.id",
		"name":   "re.name",
		"status": "re.status",
		"users":  "(SELECT count(*) FROM users WHERE reseller_id = re.id)",
	}
	pg := readPagination(r, "/resellers", allowedSorts, "id")
	pg.Dir = nonempty(r.URL.Query().Get("dir"), "asc")

	filterQ := pg.Q
	filterStatus := strings.TrimSpace(r.URL.Query().Get("status"))

	where := " WHERE 1=1"
	var args []any
	if filterQ != "" {
		args = append(args, "%"+filterQ+"%")
		where += fmt.Sprintf(" AND (re.name ILIKE $%d OR re.brand_name ILIKE $%[1]d OR re.portal_hostname ILIKE $%[1]d)", len(args))
	}
	if filterStatus != "" {
		args = append(args, filterStatus)
		where += fmt.Sprintf(" AND re.status = $%d", len(args))
	}

	var total int
	h.DB.QueryRow(r.Context(), `SELECT count(*) FROM resellers re`+where, args...).Scan(&total)
	pg.Total = total

	args = append(args, pg.Limit(), pg.Offset())
	// Revenue + profit aggregated across all of the reseller's users' orders.
	// Same four sources as /orders (CDR + MRC + channel + NRC); supplier cost
	// = CDR snapshot + supplier MRC × billing-runs-in-window + supplier NRC
	// for orders created in window. See orders() docs for the snapshot caveat.
	sql := `
		SELECT re.id, re.name, COALESCE(re.brand_name,''), COALESCE(re.portal_hostname,''), re.status,
		       to_char(re.created_at,'YYYY-MM-DD'),
		       (SELECT count(*) FROM users WHERE reseller_id = re.id),
		       (SELECT count(*) FROM orders o JOIN users u ON u.id=o.user_id
		         WHERE u.reseller_id = re.id AND o.status='active'),
		       (SELECT count(*) FROM api_keys WHERE reseller_id = re.id AND revoked_at IS NULL),
		       -- Aggregate balance EXCLUDES blocked users — admin policy:
		       -- their balance shouldn't count as available to the reseller
		       -- since the customer isn't actively trading on it.
		       COALESCE((SELECT SUM(balance_cents) FROM users
		                  WHERE reseller_id = re.id AND status = 'active'), 0),

		       -- 30d revenue (CDR + MRC + channel + NRC)
		       (COALESCE((SELECT SUM(c.charge_cents) FROM cdrs c JOIN users u ON u.id=c.user_id
		                   WHERE u.reseller_id = re.id
		                     AND c.started_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(br.mrc_charged_cents + br.channel_charged_cents)
		                      FROM billing_runs br
		                      JOIN orders o ON o.id = br.order_id
		                      JOIN users  u ON u.id = o.user_id
		                     WHERE u.reseller_id = re.id
		                       AND br.ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT -SUM(bl.delta_cents) FROM balance_ledger bl
		                      JOIN users u ON u.id = bl.user_id
		                     WHERE u.reseller_id = re.id AND bl.kind = 'nrc'
		                       AND bl.created_at > now() - interval '30 days'), 0)
		       ) AS revenue_30d_cents,

		       -- 30d supplier cost (CDR snapshot + supplier MRC × runs + supplier NRC × new orders)
		       (COALESCE((SELECT SUM(c.supplier_charge_cents) FROM cdrs c JOIN users u ON u.id=c.user_id
		                   WHERE u.reseller_id = re.id
		                     AND c.started_at > now() - interval '30 days'
		                     AND c.supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE((SELECT SUM(rc.supplier_mrc_cents)
		                      FROM billing_runs br
		                      JOIN orders o    ON o.id  = br.order_id
		                      JOIN users  u    ON u.id  = o.user_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE u.reseller_id = re.id
		                       AND br.ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(rc.supplier_nrc_cents)
		                      FROM orders o
		                      JOIN users u ON u.id = o.user_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE u.reseller_id = re.id
		                       AND o.assigned_at > now() - interval '30 days'), 0)
		       ) AS supplier_cost_30d_cents,

		       -- Lifetime revenue
		       (COALESCE((SELECT SUM(c.charge_cents) FROM cdrs c JOIN users u ON u.id=c.user_id
		                   WHERE u.reseller_id = re.id), 0)
		        + COALESCE((SELECT SUM(br.mrc_charged_cents + br.channel_charged_cents)
		                      FROM billing_runs br
		                      JOIN orders o ON o.id = br.order_id
		                      JOIN users  u ON u.id = o.user_id
		                     WHERE u.reseller_id = re.id), 0)
		        + COALESCE((SELECT -SUM(bl.delta_cents) FROM balance_ledger bl
		                      JOIN users u ON u.id = bl.user_id
		                     WHERE u.reseller_id = re.id AND bl.kind = 'nrc'), 0)
		       ) AS revenue_life_cents,

		       -- Lifetime supplier cost
		       (COALESCE((SELECT SUM(c.supplier_charge_cents) FROM cdrs c JOIN users u ON u.id=c.user_id
		                   WHERE u.reseller_id = re.id
		                     AND c.supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE((SELECT SUM(rc.supplier_mrc_cents)
		                      FROM billing_runs br
		                      JOIN orders o    ON o.id  = br.order_id
		                      JOIN users  u    ON u.id  = o.user_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE u.reseller_id = re.id), 0)
		        + COALESCE((SELECT SUM(rc.supplier_nrc_cents)
		                      FROM orders o
		                      JOIN users u ON u.id = o.user_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE u.reseller_id = re.id), 0)
		       ) AS supplier_cost_life_cents,

		       EXISTS(SELECT 1 FROM cdrs c JOIN users u ON u.id=c.user_id
		              WHERE u.reseller_id = re.id AND c.supplier_charge_cents IS NOT NULL
		                AND c.started_at > now() - interval '30 days') AS has_profit_30d,
		       EXISTS(SELECT 1 FROM cdrs c JOIN users u ON u.id=c.user_id
		              WHERE u.reseller_id = re.id AND c.supplier_charge_cents IS NOT NULL) AS has_profit_life

		  FROM resellers re` + where +
		orderByClause(allowedSorts, pg) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	var rows []rsl
	q, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		h.Log.Error("resellers query", "err", err, "sql", sql)
		http.Error(w, "internal", 500)
		return
	}
	for q.Next() {
		var x rsl
		var balCents, rev30, sup30, revLife, supLife int64
		if err := q.Scan(&x.ID, &x.Name, &x.BrandName, &x.Hostname, &x.Status, &x.Created,
			&x.UserCount, &x.ActiveOrders, &x.ActiveAPIKeys,
			&balCents, &rev30, &sup30, &revLife, &supLife,
			&x.HasProfit30d, &x.HasProfitLife); err == nil {
			x.BalanceDollars = float64(balCents) / 100
			x.Revenue30dDollars = float64(rev30) / 100
			x.Profit30dDollars = float64(rev30-sup30) / 100
			x.RevenueLifeDollars = float64(revLife) / 100
			x.ProfitLifeDollars = float64(revLife-supLife) / 100
			rows = append(rows, x)
		}
	}
	q.Close()

	type apiKey struct {
		ID         int64
		ResellerID int64
		Name       string
		LastUsed   string
		Created    string
	}
	var keys []apiKey
	kq, _ := h.DB.Query(r.Context(), `
		SELECT id, reseller_id, name,
		       COALESCE(to_char(last_used_at,'YYYY-MM-DD HH24:MI'),''),
		       to_char(created_at,'YYYY-MM-DD')
		  FROM api_keys WHERE revoked_at IS NULL ORDER BY id DESC`)
	for kq.Next() {
		var k apiKey
		kq.Scan(&k.ID, &k.ResellerID, &k.Name, &k.LastUsed, &k.Created)
		keys = append(keys, k)
	}
	kq.Close()

	ok, em := h.popFlashes(r)
	newKey := h.Session.PopString(r.Context(), "newkey")
	h.render(w, "resellers", map[string]any{
		"Title":        "Resellers",
		"Section":      "resellers",
		"FlashOK":      ok,
		"FlashErr":     em,
		"Resellers":    rows,
		"APIKeys":      keys,
		"NewKey":       newKey,
		"Total":        total,
		"Pg":           pg,
		"FilterQ":      filterQ,
		"FilterStatus": filterStatus,
	})
}

func (h *Handler) resellerDetail(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var re struct {
		ID                          int64
		Name, BrandName, Hostname   string
		Status, Created             string
	}
	err := h.DB.QueryRow(r.Context(), `
		SELECT id, name, COALESCE(brand_name,''), COALESCE(portal_hostname,''), status,
		       to_char(created_at,'YYYY-MM-DD HH24:MI')
		  FROM resellers WHERE id=$1`, id,
	).Scan(&re.ID, &re.Name, &re.BrandName, &re.Hostname, &re.Status, &re.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}

	type stats struct {
		Users            int
		ActiveUsers      int
		TotalBalanceDoll float64
		ActiveOrders     int
		CDRs30d          int
		Revenue30dDoll   float64
	}
	var s stats
	q := func(out any, sql string, args ...any) {
		_ = h.DB.QueryRow(r.Context(), sql, args...).Scan(out)
	}
	q(&s.Users, `SELECT count(*) FROM users WHERE reseller_id=$1`, id)
	q(&s.ActiveUsers, `SELECT count(*) FROM users WHERE reseller_id=$1 AND status='active'`, id)
	var balCents int64
	q(&balCents, `SELECT COALESCE(SUM(balance_cents),0) FROM users WHERE reseller_id=$1`, id)
	s.TotalBalanceDoll = float64(balCents) / 100
	q(&s.ActiveOrders, `
		SELECT count(*) FROM orders o JOIN users u ON u.id=o.user_id
		 WHERE u.reseller_id=$1 AND o.status='active'`, id)
	q(&s.CDRs30d, `
		SELECT count(*) FROM cdrs c JOIN users u ON u.id=c.user_id
		 WHERE u.reseller_id=$1 AND c.started_at > now() - interval '30 days'`, id)
	var rev30 int64
	q(&rev30, `
		SELECT COALESCE(SUM(c.charge_cents),0) FROM cdrs c JOIN users u ON u.id=c.user_id
		 WHERE u.reseller_id=$1 AND c.started_at > now() - interval '30 days'`, id)
	s.Revenue30dDoll = float64(rev30) / 100

	type userRow struct {
		ID                        int64
		ExternalID, Label, Status string
		BalanceDollars            float64
		OrderCount                int
	}
	var users []userRow
	urows, _ := h.DB.Query(r.Context(), `
		SELECT u.id, COALESCE(u.external_id,''), COALESCE(u.label, COALESCE(u.contact_email,'')),
		       u.status, u.balance_cents,
		       (SELECT count(*) FROM orders WHERE user_id=u.id AND status='active')
		  FROM users u WHERE u.reseller_id=$1 ORDER BY u.id DESC LIMIT 200`, id)
	for urows.Next() {
		var x userRow
		var bal int64
		if err := urows.Scan(&x.ID, &x.ExternalID, &x.Label, &x.Status, &bal, &x.OrderCount); err == nil {
			x.BalanceDollars = float64(bal) / 100
			users = append(users, x)
		}
	}
	urows.Close()

	type didRow struct {
		E164, Country, Type, RouteKind, RouteTarget string
		UserID                                      int64
		UserRef                                     string
		Channels                                    int
	}
	var dids []didRow
	drows, _ := h.DB.Query(r.Context(), `
		SELECT d.e164, d.country_iso, d.did_type::text, o.route_kind::text, o.route_target,
		       u.id, COALESCE(u.external_id, u.label, ''), o.channel_count
		  FROM orders o
		  JOIN dids  d ON d.id = o.did_id
		  JOIN users u ON u.id = o.user_id
		 WHERE u.reseller_id=$1 AND o.status='active'
		 ORDER BY d.e164`, id)
	for drows.Next() {
		var x didRow
		drows.Scan(&x.E164, &x.Country, &x.Type, &x.RouteKind, &x.RouteTarget, &x.UserID, &x.UserRef, &x.Channels)
		dids = append(dids, x)
	}
	drows.Close()

	type cdrRow struct {
		Started, DID, UserRef, SrcURI, HangupCause string
		Billsec                                    int
		ChargeDollars                              float64
	}
	var cdrs []cdrRow
	crows, _ := h.DB.Query(r.Context(), `
		SELECT to_char(c.started_at,'MM-DD HH24:MI'), d.e164,
		       COALESCE(u.external_id, u.label, ''),
		       COALESCE(c.src_uri,''), c.billsec, c.charge_cents,
		       COALESCE(c.hangup_cause,'')
		  FROM cdrs c
		  JOIN users  u ON u.id=c.user_id
		  JOIN orders o ON o.id=c.order_id
		  JOIN dids   d ON d.id=o.did_id
		 WHERE u.reseller_id=$1
		 ORDER BY c.started_at DESC LIMIT 25`, id)
	for crows.Next() {
		var x cdrRow
		var cents int
		if err := crows.Scan(&x.Started, &x.DID, &x.UserRef, &x.SrcURI, &x.Billsec, &cents, &x.HangupCause); err == nil {
			x.ChargeDollars = float64(cents) / 100
			cdrs = append(cdrs, x)
		}
	}
	crows.Close()

	type apiKey struct {
		ID       int64
		Name     string
		LastUsed string
		Created  string
	}
	var keys []apiKey
	kq, _ := h.DB.Query(r.Context(), `
		SELECT id, name,
		       COALESCE(to_char(last_used_at,'YYYY-MM-DD HH24:MI'),''),
		       to_char(created_at,'YYYY-MM-DD')
		  FROM api_keys WHERE reseller_id=$1 AND revoked_at IS NULL
		 ORDER BY id DESC`, id)
	for kq.Next() {
		var k apiKey
		kq.Scan(&k.ID, &k.Name, &k.LastUsed, &k.Created)
		keys = append(keys, k)
	}
	kq.Close()

	ok, em := h.popFlashes(r)
	newKey := h.Session.PopString(r.Context(), "newkey")
	h.render(w, "reseller_detail", map[string]any{
		"Title":    "Reseller · " + re.Name,
		"Section":  "resellers",
		"FlashOK":  ok,
		"FlashErr": em,
		"NewKey":   newKey,
		"Reseller": re,
		"Stats":    s,
		"Users":    users,
		"DIDs":     dids,
		"CDRs":     cdrs,
		"APIKeys":  keys,
	})
}

// resellerUpdate edits an existing reseller's name / brand_name /
// portal_hostname / status. Posted from the edit modal in /resellers.
func (h *Handler) resellerUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		h.flashErr(r, "name is required")
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	brand := strings.TrimSpace(r.PostForm.Get("brand_name"))
	host := strings.TrimSpace(r.PostForm.Get("portal_hostname"))
	status := nonempty(r.PostForm.Get("status"), "active")
	var brandPtr, hostPtr *string
	if brand != "" {
		brandPtr = &brand
	}
	if host != "" {
		hostPtr = &host
	}
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE resellers
		   SET name=$1, brand_name=$2, portal_hostname=$3, status=$4
		 WHERE id=$5`, name, brandPtr, hostPtr, status, id)
	if err != nil {
		h.flashErr(r, "save: "+err.Error())
	} else if tag.RowsAffected() == 0 {
		h.flashErr(r, "reseller not found")
	} else {
		h.flashOK(r, "Reseller saved")
	}
	http.Redirect(w, r, "/resellers", http.StatusFound)
}

// resellerDelete removes a reseller, but only if no users still reference
// it. Users have ON DELETE SET NULL on reseller_id but we don't want a
// silent reassignment to 'direct' — if there are still users under this
// reseller the admin should explicitly reassign or delete them first.
func (h *Handler) resellerDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var users int
	_ = h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM users WHERE reseller_id=$1`, id).Scan(&users)
	if users > 0 {
		h.flashErr(r, fmt.Sprintf("can't delete — %d user(s) still under this reseller. Reassign / delete them first.", users))
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM api_keys WHERE reseller_id=$1`, id); err != nil {
		h.flashErr(r, "drop api keys: "+err.Error())
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM resellers WHERE id=$1`, id)
	if err != nil {
		h.flashErr(r, "delete: "+err.Error())
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "reseller not found")
		http.Redirect(w, r, "/resellers", http.StatusFound)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "commit: "+err.Error())
	} else {
		h.flashOK(r, "Reseller deleted")
	}
	http.Redirect(w, r, "/resellers", http.StatusFound)
}

func (h *Handler) resellerCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	brand := strings.TrimSpace(r.PostForm.Get("brand_name"))
	host := strings.TrimSpace(r.PostForm.Get("portal_hostname"))
	if name == "" {
		h.flashErr(r, "name required")
	} else {
		var brandPtr, hostPtr *string
		if brand != "" {
			brandPtr = &brand
		}
		if host != "" {
			hostPtr = &host
		}
		_, err := h.DB.Exec(r.Context(),
			`INSERT INTO resellers (name, brand_name, portal_hostname, status) VALUES ($1,$2,$3,'active')`,
			name, brandPtr, hostPtr)
		if err != nil {
			h.flashErr(r, err.Error())
		} else {
			h.flashOK(r, "Reseller created")
		}
	}
	http.Redirect(w, r, "/resellers", http.StatusFound)
}

func (h *Handler) apiKeyCreate(w http.ResponseWriter, r *http.Request) {
	rid := pathID(r, "id")
	r.ParseForm()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		name = "key-" + time.Now().UTC().Format("20060102-150405")
	}
	secret := genSecret(24)
	hashed := hashAPIKey(secret)
	_, err := h.DB.Exec(r.Context(),
		`INSERT INTO api_keys (reseller_id, name, key_hash) VALUES ($1,$2,$3)`,
		rid, name, hashed)
	if err != nil {
		h.flashErr(r, err.Error())
	} else {
		h.Session.Put(r.Context(), "newkey",
			fmt.Sprintf("%s|%s", name, secret))
		h.flashOK(r, "API key created — copy the secret now, it won't be shown again")
	}
	http.Redirect(w, r, fmt.Sprintf("/resellers/%d", rid), http.StatusFound)
}

func (h *Handler) apiKeyRevoke(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	_, err := h.DB.Exec(r.Context(),
		`UPDATE api_keys SET revoked_at = now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		h.flashErr(r, err.Error())
	} else {
		h.flashOK(r, "API key revoked")
	}
	http.Redirect(w, r, "/resellers", http.StatusFound)
}

func hashAPIKey(secret string) string {
	hash, _ := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	return string(hash)
}

// =====================================================================
// shared helpers
// =====================================================================

func nonempty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func normalizeRouteTarget(kind, target string) string {
	return domain.NormalizeRouteTarget(kind, target)
}

type country struct {
	ISO, Name string
}

func (h *Handler) listCountries(r *http.Request) []country {
	var out []country
	rows, _ := h.DB.Query(r.Context(), `SELECT iso, name FROM countries ORDER BY name`)
	for rows.Next() {
		var c country
		rows.Scan(&c.ISO, &c.Name)
		out = append(out, c)
	}
	rows.Close()
	return out
}

// =====================================================================
// SIP ACCOUNT CRUD  (admin GUI). Path {id} = user id.
// =====================================================================

var reservedSipUsernames = map[string]bool{
	"outbound":         true,
	"anonymous":        true,
	"global":           true,
	"system":           true,
	"transport-udp":    true,
	"transport-tcp":    true,
	"transport-tls":    true,
	"globetelecom":     true,
	"globetelecom-aor": true,
	"globetelecom-id":  true,
}

func (h *Handler) sipAccountCreate(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	r.ParseForm()
	username := strings.TrimSpace(r.PostForm.Get("username"))
	pass := r.PostForm.Get("password")
	if username == "" || len(pass) < 6 {
		h.flashErr(r, "username and 6+ char password required")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	if !isValidSIPUsername(username) {
		h.flashErr(r, "username must be 1–32 chars, alphanumeric / underscore / hyphen only")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	if reservedSipUsernames[strings.ToLower(username)] ||
		strings.HasSuffix(username, "-auth") || strings.HasSuffix(username, "-aor") {
		h.flashErr(r, "username collides with a reserved Asterisk name")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	hashed := ha1(username, sipRealm, pass)
	_, err := h.DB.Exec(r.Context(), `
		INSERT INTO sip_accounts (user_id, username, realm, ha1) VALUES ($1,$2,$3,$4)`,
		uid, username, sipRealm, hashed)
	if err != nil {
		h.flashErr(r, "create: "+err.Error())
	} else {
		h.flashOK(r, fmt.Sprintf("SIP account %s@%s created — register with this username and password", username, sipRealm))
		go h.regenPJSIPUsers(r)
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
}

func (h *Handler) sipAccountDelete(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	said := pathID(r, "said")
	_, err := h.DB.Exec(r.Context(),
		`DELETE FROM sip_accounts WHERE id=$1 AND user_id=$2`, said, uid)
	if err != nil {
		h.flashErr(r, "delete: "+err.Error())
	} else {
		h.flashOK(r, "SIP account deleted")
		go h.regenPJSIPUsers(r)
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
}

// =====================================================================
// PJSIP user-endpoint regen for route_kind=sip_account
// =====================================================================

const pjsipUsersPath = "/etc/asterisk/pjsip_users.conf"

// pjsipSuppliersPath holds one PJSIP identify block per supplier mapping
// every IP in that supplier's groups to the shared [supplier-trunk] endpoint.
// Generated from supplier_ip_groups + supplier_ip_group_members. Asterisk
// includes it from /etc/asterisk/pjsip.conf via `#include pjsip_suppliers.conf`.
// Hot-regenerated by didapi on every supplier/group/IP create/edit/delete
// followed by `asterisk -rx "pjsip reload"`.
const pjsipSuppliersPath = "/etc/asterisk/pjsip_suppliers.conf"

// regenSupplierIdentifies writes /etc/asterisk/pjsip_suppliers.conf from the
// current DB state and reloads PJSIP. Called as a goroutine after every
// supplier / IP group / IP member mutation. Best-effort: errors are logged
// but don't bubble up to the user — the DB is the source of truth, and a
// subsequent successful mutation will regenerate the file.
//
// The generated file has one identify block per supplier (keyed by
// supplier.id so renames don't break Asterisk references), aggregating
// every IP across all of that supplier's groups. Duplicate IPs across
// groups are deduped — Asterisk would otherwise refuse the duplicate
// match line on reload.
//
// Suppliers with zero IPs are omitted (an identify block with no match
// lines is a config error in PJSIP).
// RegenSupplierIdentifiesStartup is the exported entry point cmd/didapi
// calls once on boot to ensure pjsip_suppliers.conf reflects DB state.
// Delegates to the same private function the IP-mutation handlers use.
func (h *Handler) RegenSupplierIdentifiesStartup() {
	h.regenSupplierIdentifies(nil)
}

// regenSupplierIdentifies delegates to the shared internal/asteriskcfg package
// (see regenPJSIPUsers above for the same delegation pattern).
func (h *Handler) regenSupplierIdentifies(_ *http.Request) {
	_ = asteriskcfg.RegenSupplierIdentifies(h.DB, h.Log)
}

// classifyMatch decides whether a user-supplied string is an IP/CIDR or a
// hostname so the right column gets populated. The rules are intentionally
// loose:
//   - Anything that parses cleanly as an IP or CIDR is treated as a CIDR.
//   - Otherwise, if it looks like a hostname (contains a dot, all chars
//     are alphanumeric / dot / hyphen, length <= 253, each label <= 63)
//     it's treated as a hostname.
//   - Anything else is rejected with kind="" so callers can flash an error.
//
// Returns (kind, normalized). kind ∈ {"cidr","hostname",""}. For CIDR the
// normalized value always carries a /N suffix (defaults to /32 if missing,
// or /128 for IPv6). For hostname it's the lowercased input. For "" the
// normalized value is the original (helpful for an error message).
func classifyMatch(s string) (kind, normalized string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", s
	}
	// CIDR path. If no /N was supplied, add one based on address family.
	candidate := s
	if !strings.Contains(candidate, "/") {
		// netip.ParseAddr is strict — only accepts a bare address.
		if addr, err := netip.ParseAddr(candidate); err == nil {
			if addr.Is4() {
				candidate = candidate + "/32"
			} else {
				candidate = candidate + "/128"
			}
		}
	}
	if _, err := netip.ParsePrefix(candidate); err == nil {
		return "cidr", candidate
	}
	// Hostname path. RFC 1123-ish.
	lc := strings.ToLower(s)
	if !strings.Contains(lc, ".") {
		return "", s
	}
	if len(lc) > 253 {
		return "", s
	}
	for _, label := range strings.Split(lc, ".") {
		if label == "" || len(label) > 63 {
			return "", s
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", s
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')) {
				return "", s
			}
		}
	}
	return "hostname", lc
}

// regenPJSIPUsers delegates to the shared internal/asteriskcfg package so the
// reseller API (which can't import internal/web) can run the same regen path
// for its own SIP-peer mutations. Errors are swallowed here because callers
// invoke this as `go h.regenPJSIPUsers(r)` — best-effort.
func (h *Handler) regenPJSIPUsers(_ *http.Request) {
	_ = asteriskcfg.RegenPJSIPUsers(h.DB, h.Log)
}

func isValidSIPUsername(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// writeFileAtomic delegates to asteriskcfg.WriteFileAtomic. Kept as a thin
// wrapper for places in this file that still reference the local name.
func writeFileAtomic(path string, data []byte) error {
	return asteriskcfg.WriteFileAtomic(path, data)
}
