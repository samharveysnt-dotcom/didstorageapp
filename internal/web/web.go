// Package web is the admin GUI: server-rendered HTML over chi, sessions via
// alexedwards/scs.
//
// Domain model after the 0004 migration:
//   - users   = customer-level entity (login-less; balance, KYC, channel cap)
//   - orders  = per-DID rental (one DID, channels, route, anniversary billing)
//   - kyc_*   = user-owned identity bundles, admin-approved, attached to orders
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"didstorage/internal/auth"
	"didstorage/internal/callquality"
	"didstorage/internal/causes"
	"didstorage/internal/db"
	"didstorage/internal/didsimport"
	"didstorage/internal/domain"
	"didstorage/internal/settings"
	"didstorage/internal/siptrace"
	"didstorage/internal/sslmgr"
)

// audioNameCache memoises audio_files.filename → display-name so the
// siptarget template helper can render "audio af_2f616162f0e9b007" as
// "audio nexgenvoip-intro-1" without doing a per-row DB lookup at render
// time. Refreshed wholesale every refreshTTL; new clips uploaded since
// then surface within that window. Process-scope is fine — the set is
// small (admin-curated, typically <100 rows) and the cache key is
// immutable per audio_files row (filename never changes after insert).
type audioNameCacheT struct {
	mu          sync.RWMutex
	byFilename  map[string]string // filename → name
	loadedAt    time.Time
}

func (c *audioNameCacheT) lookup(filename string) string {
	c.mu.RLock()
	v := c.byFilename[filename]
	c.mu.RUnlock()
	return v
}

func (c *audioNameCacheT) needsRefresh(now time.Time, ttl time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return now.Sub(c.loadedAt) > ttl
}

func (c *audioNameCacheT) refresh(ctx context.Context, db *db.DB) {
	rows, err := db.Query(ctx, `SELECT filename, name FROM audio_files`)
	if err != nil {
		return
	}
	defer rows.Close()
	m := make(map[string]string, 64)
	for rows.Next() {
		var f, n string
		if err := rows.Scan(&f, &n); err == nil {
			m[f] = n
		}
	}
	c.mu.Lock()
	c.byFilename = m
	c.loadedAt = time.Now()
	c.mu.Unlock()
}

//go:embed templates/*.html
var templatesFS embed.FS

type Handler struct {
	DB       *db.DB
	Redis    *redis.Client
	Session  *scs.SessionManager
	Log      *slog.Logger
	PublicIP string

	// SSL is set when the HTTPS listener is enabled, so the /settings page
	// can list configured domains and force a reload after a cert update.
	SSL *sslmgr.Manager

	// Imports tracks in-flight DID-import jobs so SSE consumers can
	// subscribe to per-row progress. One registry per Handler; lives for
	// the process lifetime; jobs are reaped 10 minutes after they end.
	Imports *didsimport.Registry

	tmpls map[string]*template.Template
}

func New(pg *db.DB, rdb *redis.Client, sm *scs.SessionManager, log *slog.Logger, publicIP string) (*Handler, error) {
	h := &Handler{
		DB:       pg,
		Redis:    rdb,
		Session:  sm,
		Log:      log,
		PublicIP: publicIP,
		Imports:  didsimport.NewRegistry(),
		tmpls:    map[string]*template.Template{},
	}
	audioCache := &audioNameCacheT{}
	// Warm once at startup so the first page render has something to work
	// with. Refreshes lazily inside siptarget on a 60s TTL.
	go audioCache.refresh(context.Background(), pg)
	funcs := template.FuncMap{
		"split":       strings.Split,
		"hangupcause": humanizeHangupCause,
		"dollars": func(v any) float64 {
			switch x := v.(type) {
			case int:
				return float64(x) / 100
			case int32:
				return float64(x) / 100
			case int64:
				return float64(x) / 100
			case float64:
				return x / 100
			}
			return 0
		},
		"absdollars": func(v any) float64 {
			switch x := v.(type) {
			case int:
				if x < 0 {
					x = -x
				}
				return float64(x) / 100
			case int64:
				if x < 0 {
					x = -x
				}
				return float64(x) / 100
			}
			return 0
		},
		"add":      func(a, b int) int { return a + b },
		"sub":      func(a, b int) int { return a - b },
		"min":      func(a, b int) int { if a < b { return a }; return b },
		"max":      func(a, b int) int { if a > b { return a }; return b },
		"div":      func(a, b int) int { if b == 0 { return 0 }; return a / b },
		"mod":      func(a, b int) int { if b == 0 { return 0 }; return a % b },
		"midpoint": func(a, b int) int { return (a + b) / 2 },
		// labelWidth approximates the rendered width of a SIP method/status
		// label in the sequence-diagram label-bg rect. ~7 px per char for the
		// 11.5px ui-sans-serif font, with sane min/max so 1-char labels still
		// have a readable badge and very long lines don't blow out the page.
		"labelWidth": func(s string) int {
			n := len(s)*7 + 8
			if n < 36 {
				n = 36
			}
			if n > 220 {
				n = 220
			}
			return n
		},
		// sipcaller cleans a SIP-style caller URI for display: strips angle
		// brackets, scheme prefix, params, and trailing port. So
		//   "<447956816884>"          → "447956816884"
		//   "<sip:bob@1.2.3.4:5060>"  → "bob@1.2.3.4"
		"sipcaller": domain.CleanCallerURI,
		// compactDuration: "5m 30s" / "1h 23m" / "45s"; preciseDuration:
		// "00:05:30:000" (HH:MM:SS:ms — ms is always 000 since billsec is
		// integer seconds, kept for tooltip consistency). Together they
		// drive the CDR Duration column.
		"compactDuration": compactDuration,
		"preciseDuration": preciseDuration,
		// didtype renders the DID-type enum value in Title Case for display:
		//   "local"    → "Local"
		//   "national" → "National"
		//   "mobile"   → "Mobile"
		//   "tollfree" → "Toll Free" (two-word special case)
		// Form values stay lowercase (they have to match the enum) — only the
		// human-visible text changes.
		"didtype": func(s string) string {
			switch s {
			case "":
				return ""
			case "tollfree":
				return "Toll Free"
			default:
				if len(s) == 1 {
					return strings.ToUpper(s)
				}
				return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
			}
		},
		"slice": func(items ...any) []any { return items },
		"icon":  iconSVG,
		// dict lets templates pass an inline map to a sub-template, e.g.
		//   {{template "daterange" dict "FromID" "cdr-from" "ToID" "cdr-to"}}
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict: odd argument count")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: non-string key at %d", i)
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		// siptarget renders route_kind + route_target as a single human-
		// readable label for the /cdrs TO column and similar tables.
		// Rendering rules:
		//   sip_account "x9WvT2mL"             → "sip_account x9WvT2mL"   (no @host suffix; the operator already knows the platform owns it)
		//   sip_uri     "sip:foo@bar:5060"     → "sip_uri sip:foo@bar:5060"
		//   ip          "1.2.3.4:5060"         → "ip 1.2.3.4:5060"
		//   audio       "didstorage/af_xxx"    → "audio nexgenvoip-intro-1" (resolved via audioCache; falls back to "audio af_xxx" if not found)
		//   audio_group "didstorage/af_xxx"    → "audio_group nexgenvoip-intro-2" (the actual clip that played for this call; the group itself is whatever the DID/order is currently configured with)
		"siptarget": func(kind, target string) template.HTML {
			if target == "" {
				return template.HTML(`<span class="muted">—</span>`)
			}
			label := template.HTMLEscapeString(target)
			switch kind {
			case "sip_account":
				// Bare username — drop any @host that might be there.
				bare := target
				if i := strings.Index(bare, "@"); i >= 0 {
					bare = bare[:i]
				}
				label = template.HTMLEscapeString(bare)
			case "audio", "audio_group":
				// Strip the "didstorage/" Asterisk-path prefix to get the
				// bare basename, then look up its operator-friendly display
				// name from the audio_files cache. Refresh the cache lazily
				// on a 60s TTL so newly-uploaded clips surface promptly.
				basename := target
				if i := strings.Index(target, "/"); i >= 0 {
					basename = target[i+1:]
				}
				if audioCache.needsRefresh(time.Now(), 60*time.Second) {
					// Synchronous so the first hit after expiry sees fresh
					// data. ~10ms typical on a small library.
					audioCache.refresh(context.Background(), pg)
				}
				if name := audioCache.lookup(basename); name != "" {
					label = template.HTMLEscapeString(name)
				} else {
					label = template.HTMLEscapeString(basename)
				}
			}
			k := template.HTMLEscapeString(kind)
			return template.HTML(`<span class="route-kind">` + k + `</span> <code>` + label + `</code>`)
		},
	}
	pages := []string{
		"login", "setup", "dashboard",
		"suppliers", "supplier_edit", "supplier_detail",
		"dids", "did_import",
		"users", "user_edit", "user_detail",
		"orders",
		"resellers", "reseller_detail",
		"cdrs", "cdr_trace",
		"denied_calls",
		"live",
		"order_detail",
		"kyc_detail",
		"search",
		"did_cdrs",
		"cause_codes",
		"settings",
		// audio library backs the "play audio then hang up" reservation
		// route — admin uploads + converts clips here, then picks them
		// from a dropdown in the DID reserve modal.
		"audio_files",
		// Audio groups: named bundles of clips that the audio_group
		// reserved-route picks one of (random-no-repeat) per call.
		"audio_groups",
		"audio_group_detail",
		// reseller-API endpoint reference rendered as its own page under
		// the Settings group in the sidebar.
		"api_docs",
	}
	for _, name := range pages {
		t, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		h.tmpls[name] = t
	}
	return h, nil
}

// Mount attaches all GUI routes. /login is public; the rest require admin.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/login", h.getLogin)
	r.Post("/login", h.postLogin)
	// First-run admin creation. Only reachable while the admins table is
	// empty; once the first admin exists, both GETs and POSTs 302 to
	// /login. See internal/web/setup_handlers.go.
	r.Get("/setup", h.setup)
	r.Post("/setup", h.setupSubmit)
	r.Get("/logout", h.logout)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin(h.Session))
		r.Get("/", h.dashboard)

		// Suppliers + IP groups + rate cards
		r.Get("/suppliers", h.suppliers)
		r.Get("/suppliers/new", h.supplierNew)
		r.Post("/suppliers", h.supplierCreate)
		r.Get("/suppliers/{id}", h.supplierDetail)
		r.Post("/suppliers/{id}", h.supplierUpdate)
		r.Post("/suppliers/{id}/ip-groups", h.ipGroupCreate)
		r.Post("/suppliers/{id}/ip-groups/{gid}", h.ipGroupUpdate)
		r.Post("/suppliers/{id}/ip-groups/{gid}/delete", h.ipGroupDelete)
		r.Post("/suppliers/{id}/ip-groups/{gid}/ips", h.ipMemberAdd)
		r.Post("/suppliers/{id}/ip-groups/{gid}/ips/bulk", h.ipMemberAddBulk)
		r.Post("/suppliers/{id}/ip-groups/{gid}/ips/{mid}/edit", h.ipMemberEdit)
		r.Post("/suppliers/{id}/ip-groups/{gid}/ips/{mid}/delete", h.ipMemberDelete)
		r.Post("/suppliers/{id}/rate-cards", h.rateCardCreate)
		r.Post("/suppliers/{id}/rate-cards/bulk", h.rateCardCreateBulk)
		r.Post("/rate-cards/{id}/update", h.rateCardUpdate)
		r.Post("/rate-cards/{id}/expire", h.rateCardExpire)

		// DIDs
		r.Get("/dids", h.dids)
		r.Get("/dids/import", h.didImport)
		// Legacy single-shot import endpoint kept for back-compat with any
		// scripts that POST directly; the GUI now uses /start + SSE.
		r.Post("/dids/import", h.didImportSubmit)
		// New job-based import surface: start returns an import_id, the
		// browser opens stream to watch progress, status is a JSON fallback
		// after the stream closes, example serves the CSV template.
		r.Post("/dids/import/start", h.didImportStart)
		r.Get("/dids/import/{id}/stream", h.didImportStream)
		r.Get("/dids/import/{id}/status", h.didImportStatus)
		r.Get("/dids/import/example.csv", h.didImportExample)
		r.Post("/dids/{id}/retire", h.didRetire)
		r.Post("/dids/{id}/reserve", h.didReserve)
		r.Post("/dids/{id}/release", h.didRelease)
		r.Get("/dids/{id}/cdrs", h.didCDRs)

		// Audio library — clips that back the "play audio then hang up"
		// DID reservation route. /options.json feeds the reserve modal's
		// dropdown; /{id}/play streams a WAV-wrapped rendition so the
		// admin browser can preview the clip inline.
		r.Get("/audio-files", h.audioFiles)
		r.Post("/audio-files", h.audioFileUpload)
		r.Post("/audio-files/bulk", h.audioFileBulkUpload)
		r.Post("/audio-files/{id}/rename", h.audioFileRename)
		r.Post("/audio-files/{id}/delete", h.audioFileDelete)
		r.Get("/audio-files/{id}/play", h.audioFilePlay)
		r.Get("/audio-files/options.json", h.audioFileOptions)

		// Audio groups — named bundles of clips that the audio_group
		// reserved-route picks one of (random-no-repeat) per call.
		r.Get("/audio-groups", h.audioGroups)
		r.Post("/audio-groups", h.audioGroupCreate)
		r.Get("/audio-groups/{id}", h.audioGroupDetail)
		r.Post("/audio-groups/{id}", h.audioGroupUpdate)
		r.Post("/audio-groups/{id}/delete", h.audioGroupDelete)
		r.Post("/audio-groups/{id}/members", h.audioGroupAddMember)
		r.Post("/audio-groups/{id}/members/{afid}/delete", h.audioGroupRemoveMember)
		r.Get("/audio-groups/options.json", h.audioGroupOptions)

		// Users (customer-level)
		r.Get("/users", h.users)
		r.Get("/users/new", h.userNew)
		r.Post("/users", h.userCreate)
		r.Get("/users/{id}", h.userDetail)
		r.Post("/users/{id}", h.userUpdate)
		r.Post("/users/{id}/topup", h.userTopup)
		r.Post("/users/{id}/delete", h.userDelete)
		r.Post("/users/{id}/sip-accounts", h.sipAccountCreate)
		r.Post("/users/{id}/sip-accounts/{said}/delete", h.sipAccountDelete)
		r.Post("/users/{id}/kyc-bundles", h.kycBundleCreate)
		r.Post("/users/{id}/block", h.userBlock)
		r.Post("/users/{id}/unblock", h.userUnblock)
		r.Get("/users/{id}/export/cdrs.csv", h.userExportCDRs)
		r.Get("/users/{id}/export/ledger.csv", h.userExportLedger)
		r.Get("/users/{id}/export/blocks.csv", h.userExportBlocks)

		// KYC bundle detail / docs / approve
		r.Get("/kyc-bundles/{bid}", h.kycBundleDetail)
		r.Post("/kyc-bundles/{bid}/documents", h.kycDocUpload)
		r.Get("/kyc-bundles/{bid}/documents/{did}/download", h.kycDocDownload)
		r.Post("/kyc-bundles/{bid}/documents/{did}/delete", h.kycDocDelete)
		r.Post("/kyc-bundles/{bid}/approve", h.kycApprove)
		r.Post("/kyc-bundles/{bid}/reject", h.kycReject)

		// Orders (per-DID rental). Created from the user detail page.
		r.Get("/orders", h.orders)
		r.Post("/orders", h.orderCreate)
		// /route is the legacy URL — kept so old bookmarks/links still work.
		// /update is the new full-edit endpoint targeted by the order-edit modal.
		r.Post("/orders/{id}/route", h.orderUpdate)
		r.Post("/orders/{id}/update", h.orderUpdate)
		r.Post("/orders/{id}/cancel", h.orderCancel)
		r.Get("/orders/{id}/export/cdrs.csv", h.orderExportCDRs)
		r.Get("/orders/{id}", h.orderDetail)

		// Resellers + API keys
		r.Get("/resellers", h.resellers)
		r.Post("/resellers", h.resellerCreate)
		r.Get("/resellers/{id}", h.resellerDetail)
		r.Post("/resellers/{id}", h.resellerUpdate)
		r.Post("/resellers/{id}/delete", h.resellerDelete)
		r.Post("/resellers/{id}/api-keys", h.apiKeyCreate)
		r.Post("/api-keys/{id}/revoke", h.apiKeyRevoke)

		r.Get("/cdrs", h.cdrs)
		r.Get("/cdrs/export.csv", h.globalExportCDRs)
		r.Get("/cdrs/{call_id}/sip-trace", h.cdrSipTrace)
		r.Get("/denied-calls", h.deniedCalls)

		// Live calls — Phase 1: read-only list + hangup action.
		// Phase 2 adds redirect + warn-and-hangup (audio prompt → BYE).
		// /live/stream is an SSE feed; the page subscribes on load and
		// receives JSON snapshots once per second, no more meta-refresh.
		r.Get("/live", h.liveCalls)
		r.Get("/live/stream", h.liveStream)
		r.Post("/live/{call_id}/hangup", h.liveHangup)
		r.Post("/live/{call_id}/redirect", h.liveRedirect)
		// Force-cleanup: evict ghost rows whose Asterisk channel either
		// doesn't exist or has died — see liveForceCleanup for details.
		r.Post("/live/force-cleanup", h.liveForceCleanup)

		// Global search bar in the layout posts here.
		r.Get("/search", h.globalSearch)

		// Hangup-cause editor (admin-only configuration; backed by the
		// hangup_causes table; in-memory map reloaded after every write).
		r.Get("/cause-codes", h.causeCodes)
		r.Post("/cause-codes", h.causeCodeUpsert)
		r.Post("/cause-codes/{code}/delete", h.causeCodeDelete)

		// Site / company / admin / domain configuration. Tabs render in one
		// page; each form posts to its own endpoint.
		r.Get("/settings", h.settingsPage)
		r.Post("/settings", h.settingsBulkUpdate) // ?tab=company|site
		r.Post("/settings/admin/password", h.adminPasswordChange)
		r.Post("/settings/domains", h.domainCreate)
		r.Post("/settings/domains/{id}", h.domainUpdate)
		r.Post("/settings/domains/{id}/delete", h.domainDelete)

		// Reseller API reference (static template, no inputs).
		r.Get("/settings/api-docs", h.apiDocs)
	})
}

// apiDocs serves the static reseller-API endpoint reference. Pure documentation:
// no DB query, no user input. Lives under /settings/ so it inherits the
// Settings-group sidebar highlight.
func (h *Handler) apiDocs(w http.ResponseWriter, r *http.Request) {
	h.render(w, "api_docs", map[string]any{
		"Title":   "API reference",
		"Section": "api",
	})
}

func (h *Handler) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["ShowChrome"]; !ok {
		data["ShowChrome"] = true
	}
	// Inject the configurable company / brand strings on every render so the
	// sidebar, page <title>, and the login screen all pick up edits made in
	// /settings without a deploy. Falls back to "DIDStorage" when the
	// setting is empty or the in-memory cache hasn't loaded yet.
	if _, ok := data["CompanyName"]; !ok {
		data["CompanyName"] = settings.GetWithDefault("company.name", "DIDStorage")
	}
	if _, ok := data["CompanyBrand"]; !ok {
		data["CompanyBrand"] = settings.GetWithDefault("company.brand", "DIDStorage")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpls[name].ExecuteTemplate(w, "layout", data); err != nil {
		h.Log.Error("template render failed", "name", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (h *Handler) getLogin(w http.ResponseWriter, r *http.Request) {
	if auth.IsLoggedIn(h.Session, r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// Fresh-install redirect: if no admin has been created yet, land the
	// operator on /setup so they can pick a password in-browser instead
	// of hitting a login form that would reject everything they type.
	// A DB error here is logged but not fatal; we fall through to the
	// normal login page so a transient outage doesn't lock the admin out.
	if open, err := h.setupIsOpen(r.Context()); err == nil && open {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	} else if err != nil {
		h.Log.Warn("login page: setup-gate check failed", "err", err)
	}
	h.render(w, "login", map[string]any{
		"Title":      "Sign in",
		"ShowChrome": false,
		"Err":        "",
	})
}

func (h *Handler) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pw := strings.TrimSpace(r.PostForm.Get("password"))
	id, err := auth.Verify(r.Context(), h.DB.Pool, pw)
	if errors.Is(err, auth.ErrInvalid) {
		h.render(w, "login", map[string]any{
			"Title":      "Sign in",
			"ShowChrome": false,
			"Err":        "Wrong password.",
		})
		return
	}
	if err != nil {
		h.Log.Error("login error", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if err := h.Session.RenewToken(r.Context()); err != nil {
		h.Log.Error("renew session failed", "err", err)
	}
	auth.SetSession(h.Session, r, id)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	auth.ClearSession(h.Session, r)
	_ = h.Session.Destroy(r.Context())
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- pages ----

type Stats struct {
	Suppliers           int
	IPs                 int
	DIDs                int
	AssignedDIDs        int
	AvailableDIDs       int
	Users               int
	ActiveOrders        int
	KycPending          int
	Quarantined         int
	TotalBalanceDollars float64
	CDRs24h             int
	Revenue24hDollars   float64
	ActiveCalls         int
	DeniedCalls24h      int
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var s Stats
	q := func(out any, sql string, args ...any) {
		_ = h.DB.QueryRow(ctx, sql, args...).Scan(out)
	}
	q(&s.Suppliers, `SELECT count(*) FROM suppliers`)
	q(&s.IPs, `SELECT count(*) FROM supplier_ip_group_members`)
	q(&s.DIDs, `SELECT count(*) FROM dids`)
	q(&s.AssignedDIDs, `SELECT count(*) FROM dids WHERE status='assigned'`)
	q(&s.AvailableDIDs, `SELECT count(*) FROM dids WHERE status='available'`)
	q(&s.Users, `SELECT count(*) FROM users`)
	q(&s.ActiveOrders, `SELECT count(*) FROM orders WHERE status='active'`)
	q(&s.KycPending, `SELECT count(*) FROM orders WHERE status='kyc_pending'`)
	q(&s.Quarantined, `SELECT count(*) FROM orders WHERE status='quarantined'`)

	var totalCents int64
	q(&totalCents, `SELECT COALESCE(SUM(balance_cents),0) FROM users`)
	s.TotalBalanceDollars = float64(totalCents) / 100.0

	q(&s.CDRs24h, `SELECT count(*) FROM cdrs WHERE started_at > now() - interval '24 hours'`)
	var rev24 int64
	q(&rev24, `SELECT COALESCE(SUM(charge_cents),0) FROM cdrs WHERE started_at > now() - interval '24 hours'`)
	s.Revenue24hDollars = float64(rev24) / 100.0
	q(&s.DeniedCalls24h, `SELECT count(*) FROM denied_calls WHERE created_at > now() - interval '24 hours'`)

	s.ActiveCalls = countActiveCalls(ctx, h.Redis)

	type cdrRow struct {
		Started        string
		DID            string
		SrcURI         string
		Billsec        int
		ChargedMinutes int
		ChargeDollars  float64
		HangupCause    string
	}
	var recent []cdrRow
	rows, err := h.DB.Query(ctx, `
		SELECT to_char(c.started_at,'MM-DD HH24:MI'), d.e164, c.src_uri,
		       c.billsec, c.charged_minutes, c.charge_cents, COALESCE(c.hangup_cause,'')
		  FROM cdrs c
		  JOIN orders o ON o.id = c.order_id
		  JOIN dids   d ON d.id = o.did_id
		 ORDER BY c.started_at DESC
		 LIMIT 20
	`)
	if err == nil {
		for rows.Next() {
			var x cdrRow
			var cents int
			if err := rows.Scan(&x.Started, &x.DID, &x.SrcURI, &x.Billsec, &x.ChargedMinutes, &cents, &x.HangupCause); err == nil {
				x.ChargeDollars = float64(cents) / 100.0
				recent = append(recent, x)
			}
		}
		rows.Close()
	}

	h.render(w, "dashboard", map[string]any{
		"Title":      "Dashboard",
		"Section":    "dashboard",
		"Stats":      s,
		"RecentCDRs": recent,
	})
}

func (h *Handler) suppliers(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID                                                                         int64
		Name                                                                       string
		Status                                                                     string
		GroupCount                                                                 int
		IPList                                                                     string
		RateCount                                                                  int
		DIDCount                                                                   int
		Revenue30dDollars, Profit30dDollars, RevenueLifeDollars, ProfitLifeDollars float64
		HasProfit30d, HasProfitLife                                                bool
	}
	// For a SUPPLIER, "revenue" is what we billed the customer on traffic
	// going through that supplier's DIDs, and "supplier cost" is what we owe
	// that specific supplier (CDR snapshot per call + supplier MRC × billing
	// runs + supplier NRC for orders created in the window). Profit is the
	// margin platform keeps on this supplier's traffic.
	var out []row
	rows, err := h.DB.Query(r.Context(), `
		SELECT s.id, s.name, s.status,
		       (SELECT count(*) FROM supplier_ip_groups WHERE supplier_id = s.id),
		       COALESCE((SELECT string_agg(host(m.cidr), ', ' ORDER BY m.id)
		                   FROM supplier_ip_group_members m
		                   JOIN supplier_ip_groups g ON g.id = m.group_id
		                  WHERE g.supplier_id = s.id), ''),
		       (SELECT count(*) FROM rate_cards WHERE supplier_id = s.id AND valid_to IS NULL),
		       (SELECT count(*) FROM dids WHERE supplier_id = s.id),

		       -- 30d revenue: customer charges on traffic through this supplier
		       (COALESCE((SELECT SUM(charge_cents) FROM cdrs
		                   WHERE supplier_id = s.id
		                     AND started_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(br.mrc_charged_cents + br.channel_charged_cents)
		                      FROM billing_runs br
		                      JOIN orders o ON o.id = br.order_id
		                      JOIN dids   d ON d.id = o.did_id
		                     WHERE d.supplier_id = s.id
		                       AND br.ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT -SUM(bl.delta_cents) FROM balance_ledger bl
		                      JOIN orders o ON o.id = bl.ref_id::bigint
		                      JOIN dids   d ON d.id = o.did_id
		                     WHERE bl.ref_table = 'orders' AND bl.kind = 'nrc'
		                       AND d.supplier_id = s.id
		                       AND bl.created_at > now() - interval '30 days'), 0)
		       ) AS revenue_30d_cents,

		       -- 30d supplier-side cost (what we owe this supplier)
		       (COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		                   WHERE supplier_id = s.id
		                     AND started_at > now() - interval '30 days'
		                     AND supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE((SELECT SUM(rc.supplier_mrc_cents)
		                      FROM billing_runs br
		                      JOIN orders o    ON o.id  = br.order_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE rc.supplier_id = s.id
		                       AND br.ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(rc.supplier_nrc_cents)
		                      FROM orders o
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE rc.supplier_id = s.id
		                       AND o.assigned_at > now() - interval '30 days'), 0)
		       ) AS supplier_cost_30d_cents,

		       -- Lifetime revenue
		       (COALESCE((SELECT SUM(charge_cents) FROM cdrs WHERE supplier_id = s.id), 0)
		        + COALESCE((SELECT SUM(br.mrc_charged_cents + br.channel_charged_cents)
		                      FROM billing_runs br
		                      JOIN orders o ON o.id = br.order_id
		                      JOIN dids   d ON d.id = o.did_id
		                     WHERE d.supplier_id = s.id), 0)
		        + COALESCE((SELECT -SUM(bl.delta_cents) FROM balance_ledger bl
		                      JOIN orders o ON o.id = bl.ref_id::bigint
		                      JOIN dids   d ON d.id = o.did_id
		                     WHERE bl.ref_table = 'orders' AND bl.kind = 'nrc'
		                       AND d.supplier_id = s.id), 0)
		       ) AS revenue_life_cents,

		       -- Lifetime supplier-side cost
		       (COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		                   WHERE supplier_id = s.id
		                     AND supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE((SELECT SUM(rc.supplier_mrc_cents)
		                      FROM billing_runs br
		                      JOIN orders o    ON o.id  = br.order_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE rc.supplier_id = s.id), 0)
		        + COALESCE((SELECT SUM(rc.supplier_nrc_cents)
		                      FROM orders o
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE rc.supplier_id = s.id), 0)
		       ) AS supplier_cost_life_cents,

		       EXISTS(SELECT 1 FROM cdrs
		              WHERE supplier_id = s.id AND supplier_charge_cents IS NOT NULL
		                AND started_at > now() - interval '30 days') AS has_profit_30d,
		       EXISTS(SELECT 1 FROM cdrs
		              WHERE supplier_id = s.id AND supplier_charge_cents IS NOT NULL) AS has_profit_life

		  FROM suppliers s
		 ORDER BY s.name
	`)
	if err != nil {
		h.Log.Error("suppliers query", "err", err)
		http.Error(w, "internal", 500)
		return
	}
	for rows.Next() {
		var x row
		var rev30, sup30, revLife, supLife int64
		if err := rows.Scan(&x.ID, &x.Name, &x.Status, &x.GroupCount, &x.IPList, &x.RateCount, &x.DIDCount,
			&rev30, &sup30, &revLife, &supLife,
			&x.HasProfit30d, &x.HasProfitLife); err == nil {
			x.Revenue30dDollars = float64(rev30) / 100
			x.Profit30dDollars = float64(rev30-sup30) / 100
			x.RevenueLifeDollars = float64(revLife) / 100
			x.ProfitLifeDollars = float64(revLife-supLife) / 100
			out = append(out, x)
		}
	}
	rows.Close()
	ok, em := h.popFlashes(r)
	h.render(w, "suppliers", map[string]any{
		"Title":     "Suppliers",
		"Section":   "suppliers",
		"FlashOK":   ok,
		"FlashErr":  em,
		"Suppliers": out,
	})
}

func (h *Handler) dids(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID                                          int64
		E164, Country, Type, Supplier, Status       string
		// SupplierID feeds the Supplier-column link to /suppliers/{id}.
		SupplierID                                  int64
		AssignedTo                                  string
		AssignedUserID                              int64
		AssignedOrderID                             int64
		Channels                                    string
		RouteKind, RouteTarget                      string
		OrderStatus                                 string
		ReservedKind, ReservedTarget, ReservedNote string
		// ReservedAudioName is the human-friendly display name of the
		// audio_files row a DID is reserved to (when ReservedKind='audio').
		// Blank for non-audio reservations.
		ReservedAudioName string
	}
	allowedSorts := map[string]string{
		"id":       "d.id",
		"e164":     "d.e164",
		"country":  "d.country_iso",
		"status":   "d.status",
		"supplier": "s.name",
	}
	pg := readPagination(r, "/dids", allowedSorts, "e164")
	pg.Dir = nonempty(r.URL.Query().Get("dir"), "asc")

	where := " WHERE 1=1"
	var args []any
	if pg.Q != "" {
		args = append(args, "%"+pg.Q+"%")
		where += fmt.Sprintf(" AND (d.e164 ILIKE $%d OR s.name ILIKE $%[1]d)", len(args))
	}
	if v := r.URL.Query().Get("status"); v != "" {
		args = append(args, v)
		where += fmt.Sprintf(" AND d.status = $%d", len(args))
		pg.BaseArgs = "status=" + v
	}

	var total int
	h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM dids d JOIN suppliers s ON s.id=d.supplier_id`+where, args...).Scan(&total)
	pg.Total = total

	args = append(args, pg.Limit(), pg.Offset())
	sql := `
		SELECT d.id, d.e164, d.country_iso, d.did_type::text, s.name, s.id, d.status,
		       COALESCE(u.external_id, u.label, u.contact_email, ''),
		       COALESCE(u.id, 0),
		       COALESCE(o.id, 0),
		       COALESCE(o.channel_count::text, ''),
		       COALESCE(o.route_kind::text, ''),
		       COALESCE(o.route_target, ''),
		       COALESCE(o.status::text, ''),
		       COALESCE(d.reserved_route_kind::text, ''),
		       COALESCE(d.reserved_route_target, ''),
		       COALESCE(d.reserved_note, ''),
		       COALESCE(af.name, '')
		  FROM dids d
		  JOIN suppliers s ON s.id = d.supplier_id
		  LEFT JOIN orders o ON o.did_id = d.id AND o.status IN ('active','kyc_pending','quarantined')
		  LEFT JOIN users  u ON u.id = o.user_id
		  LEFT JOIN audio_files af ON af.id = d.reserved_audio_file_id` + where +
		orderByClause(allowedSorts, pg) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	var out []row
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		h.Log.Error("dids query", "err", err)
		http.Error(w, "internal", 500)
		return
	}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ID, &x.E164, &x.Country, &x.Type, &x.Supplier, &x.SupplierID, &x.Status,
			&x.AssignedTo, &x.AssignedUserID, &x.AssignedOrderID,
			&x.Channels, &x.RouteKind, &x.RouteTarget, &x.OrderStatus,
			&x.ReservedKind, &x.ReservedTarget, &x.ReservedNote,
			&x.ReservedAudioName); err == nil {
			out = append(out, x)
		}
	}
	rows.Close()
	ok, em := h.popFlashes(r)
	h.render(w, "dids", map[string]any{
		"Title":    "DIDs",
		"Section":  "dids",
		"FlashOK":  ok,
		"FlashErr": em,
		"DIDs":     out,
		"Total":    total,
		"Pg":       pg,
		// ReturnTo lets reserve/release POSTs redirect back to the same
		// filtered view the user was on. Validated server-side in
		// safeReturnTo before being applied.
		"ReturnTo": r.URL.RequestURI(),
	})
}

// users lists customer-level users with pagination, sort, and search.
//
// Query params: page, per_page (10..1000), sort, dir, q, reseller_id.
func (h *Handler) users(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID                                                                         int64
		ExternalID, Label, ContactEmail, Status                                    string
		Reseller, Created                                                          string
		ResellerID                                                                 int64
		BalanceDollars                                                             float64
		ChannelCap                                                                 string
		TotalOrders, ActiveOrders                                                  int
		KycApproved, KycPending, KycRejected                                       int
		Revenue30dDollars, Profit30dDollars, RevenueLifeDollars, ProfitLifeDollars float64
		HasProfit30d, HasProfitLife                                                bool
	}

	allowedSorts := map[string]string{
		"id":           "u.id",
		"external_id":  "u.external_id",
		"label":        "u.label",
		"status":       "u.status",
		"balance":      "u.balance_cents",
		"created":      "u.created_at",
	}
	pg := readPagination(r, "/users", allowedSorts, "id")

	rid := r.URL.Query().Get("reseller_id")
	filterMode := "all"
	var filterID int64
	if rid != "" {
		filterMode = "specific"
		if rid == "0" {
			filterMode = "direct"
		} else {
			filterID, _ = strconv.ParseInt(rid, 10, 64)
			if filterID == 0 {
				filterMode = "all"
			}
		}
	}

	// view=active (default) shows only status='active' users; view=archive
	// shows the rest (blocked / inactive). Inactive users are preserved
	// indefinitely — admins move between views via the tabs, not by filtering.
	view := r.URL.Query().Get("view")
	if view != "archive" && view != "all" {
		view = "active"
	}

	where := " WHERE 1=1"
	var args []any
	switch filterMode {
	case "direct":
		where += " AND u.reseller_id IS NULL"
	case "specific":
		args = append(args, filterID)
		where += fmt.Sprintf(" AND u.reseller_id = $%d", len(args))
	}
	switch view {
	case "active":
		where += " AND u.status = 'active'"
	case "archive":
		where += " AND u.status <> 'active'"
	}
	if pg.Q != "" {
		args = append(args, "%"+pg.Q+"%")
		where += fmt.Sprintf(" AND (u.external_id ILIKE $%d OR u.label ILIKE $%[1]d OR u.contact_email ILIKE $%[1]d)", len(args))
	}

	// COUNT for pagination
	var total int
	if err := h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM users u`+where, args...).Scan(&total); err != nil {
		h.Log.Error("users count", "err", err)
		http.Error(w, "internal", 500)
		return
	}
	pg.Total = total

	args = append(args, pg.Limit(), pg.Offset())
	// Per-user revenue/profit (30d + lifetime). Aggregated across all of the
	// user's orders. Same four customer-charge sources as /orders + /resellers
	// (CDR + MRC + channel + NRC); supplier-cost approximation same caveat.
	sql := `
		SELECT u.id,
		       COALESCE(u.external_id, ''),
		       COALESCE(u.label, ''),
		       COALESCE(u.contact_email, ''),
		       u.status,
		       COALESCE(re.name, ''),
		       COALESCE(u.reseller_id, 0),
		       to_char(u.created_at, 'YYYY-MM-DD'),
		       u.balance_cents,
		       CASE WHEN u.global_channel_cap < 0 THEN '∞'::text
		            ELSE u.global_channel_cap::text END,
		       (SELECT count(*) FROM orders WHERE user_id = u.id),
		       (SELECT count(*) FROM orders WHERE user_id = u.id AND status = 'active'),
		       (SELECT count(*) FROM kyc_bundles WHERE user_id = u.id AND status = 'approved'),
		       (SELECT count(*) FROM kyc_bundles WHERE user_id = u.id AND status = 'pending'),
		       (SELECT count(*) FROM kyc_bundles WHERE user_id = u.id AND status = 'rejected'),

		       -- 30d revenue
		       (COALESCE((SELECT SUM(charge_cents) FROM cdrs
		                   WHERE user_id = u.id
		                     AND started_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(br.mrc_charged_cents + br.channel_charged_cents)
		                      FROM billing_runs br JOIN orders o ON o.id = br.order_id
		                     WHERE o.user_id = u.id
		                       AND br.ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT -SUM(delta_cents) FROM balance_ledger
		                     WHERE user_id = u.id AND kind = 'nrc'
		                       AND created_at > now() - interval '30 days'), 0)
		       ) AS revenue_30d_cents,

		       -- 30d supplier cost
		       (COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		                   WHERE user_id = u.id
		                     AND started_at > now() - interval '30 days'
		                     AND supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE((SELECT SUM(rc.supplier_mrc_cents)
		                      FROM billing_runs br
		                      JOIN orders o    ON o.id  = br.order_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE o.user_id = u.id
		                       AND br.ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(rc.supplier_nrc_cents)
		                      FROM orders o
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE o.user_id = u.id
		                       AND o.assigned_at > now() - interval '30 days'), 0)
		       ) AS supplier_cost_30d_cents,

		       -- Lifetime revenue
		       (COALESCE((SELECT SUM(charge_cents) FROM cdrs WHERE user_id = u.id), 0)
		        + COALESCE((SELECT SUM(br.mrc_charged_cents + br.channel_charged_cents)
		                      FROM billing_runs br JOIN orders o ON o.id = br.order_id
		                     WHERE o.user_id = u.id), 0)
		        + COALESCE((SELECT -SUM(delta_cents) FROM balance_ledger
		                     WHERE user_id = u.id AND kind = 'nrc'), 0)
		       ) AS revenue_life_cents,

		       -- Lifetime supplier cost
		       (COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		                   WHERE user_id = u.id AND supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE((SELECT SUM(rc.supplier_mrc_cents)
		                      FROM billing_runs br
		                      JOIN orders o    ON o.id  = br.order_id
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE o.user_id = u.id), 0)
		        + COALESCE((SELECT SUM(rc.supplier_nrc_cents)
		                      FROM orders o
		                      JOIN rate_cards rc ON rc.id = o.rate_card_id
		                     WHERE o.user_id = u.id), 0)
		       ) AS supplier_cost_life_cents,

		       EXISTS(SELECT 1 FROM cdrs
		              WHERE user_id = u.id AND supplier_charge_cents IS NOT NULL
		                AND started_at > now() - interval '30 days') AS has_profit_30d,
		       EXISTS(SELECT 1 FROM cdrs
		              WHERE user_id = u.id AND supplier_charge_cents IS NOT NULL) AS has_profit_life

		  FROM users u
		  LEFT JOIN resellers re ON re.id = u.reseller_id` + where +
		orderByClause(allowedSorts, pg) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	var out []row
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		h.Log.Error("users query", "err", err, "sql", sql)
		http.Error(w, "internal", 500)
		return
	}
	for rows.Next() {
		var x row
		var bal, rev30, sup30, revLife, supLife int64
		if err := rows.Scan(&x.ID, &x.ExternalID, &x.Label, &x.ContactEmail, &x.Status,
			&x.Reseller, &x.ResellerID, &x.Created, &bal, &x.ChannelCap,
			&x.TotalOrders, &x.ActiveOrders,
			&x.KycApproved, &x.KycPending, &x.KycRejected,
			&rev30, &sup30, &revLife, &supLife,
			&x.HasProfit30d, &x.HasProfitLife); err == nil {
			x.BalanceDollars = float64(bal) / 100.0
			x.Revenue30dDollars = float64(rev30) / 100
			x.Profit30dDollars = float64(rev30-sup30) / 100
			x.RevenueLifeDollars = float64(revLife) / 100
			x.ProfitLifeDollars = float64(revLife-supLife) / 100
			out = append(out, x)
		}
	}
	rows.Close()

	if filterMode != "all" {
		pg.BaseArgs = "reseller_id=" + rid
	}
	if view != "active" {
		if pg.BaseArgs != "" {
			pg.BaseArgs += "&"
		}
		pg.BaseArgs += "view=" + view
	}

	// Tab counts: active vs archive across the same reseller filter so
	// switching views feels meaningful. Counts ignore the search query so
	// the tab badges show the population, not the filtered subset.
	var activeCount, archiveCount int
	scopeWhere := ""
	scopeArgs := []any{}
	switch filterMode {
	case "direct":
		scopeWhere = " AND reseller_id IS NULL"
	case "specific":
		scopeWhere = " AND reseller_id = $1"
		scopeArgs = []any{filterID}
	}
	_ = h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM users WHERE status = 'active'`+scopeWhere, scopeArgs...).Scan(&activeCount)
	_ = h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM users WHERE status <> 'active'`+scopeWhere, scopeArgs...).Scan(&archiveCount)

	type rr struct {
		ID   int64
		Name string
	}
	var resellers []rr
	rrows, _ := h.DB.Query(r.Context(), `SELECT id, name FROM resellers ORDER BY name`)
	for rrows.Next() {
		var x rr
		rrows.Scan(&x.ID, &x.Name)
		resellers = append(resellers, x)
	}
	rrows.Close()

	ok, em := h.popFlashes(r)
	h.render(w, "users", map[string]any{
		"Title":        "Users",
		"Section":      "users",
		"FlashOK":      ok,
		"FlashErr":     em,
		"Users":        out,
		"Total":        total,
		"Resellers":    resellers,
		"FilterMode":   filterMode,
		"FilterID":     filterID,
		"Pg":           pg,
		"View":         view,
		"ActiveCount":  activeCount,
		"ArchiveCount": archiveCount,
	})
}

// orders lists per-DID rentals globally, joining the user + reseller for
// quick context links and revenue/profit numbers. The revenue + profit
// columns sum ALL customer-charged sources, not just CDR minutes:
//
//   revenue = CDR minutes  (cdrs.charge_cents)
//           + MRC          (billing_runs.mrc_charged_cents)
//           + channels     (billing_runs.channel_charged_cents)
//           + NRC          (balance_ledger kind='nrc', ref_table='orders')
//
//   supplier cost = CDR minutes (cdrs.supplier_charge_cents — per-call snapshot)
//                 + supplier MRC (rate_cards.supplier_mrc_cents × # billing runs)
//                 + supplier NRC (rate_cards.supplier_nrc_cents, if order new in window)
//
//   profit = revenue - supplier cost
//
// Caveat: supplier MRC/NRC aren't snapshotted per-event the way CDR supplier
// charges are, so a rate-card edit will retroactively change historical
// supplier-cost numbers. The CDR portion is always accurate. Phase-followup
// would add per-billing_run supplier_cost_cents columns to snapshot at
// charge time.
//
// Two windows are computed in one query: 30-day and lifetime.
func (h *Handler) orders(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID, UserID, ResellerID, KycBundleID                                int64
		DID, UserRef, Reseller                                             string
		Status, RouteKind, RouteTarget                                     string
		ChannelCount, AnniversaryDay                                       int
		NextBilling                                                        string
		KycStatus                                                          string
		Revenue30dDollars, Profit30dDollars, RevenueLifeDollars, ProfitLifeDollars float64
		HasProfit30d, HasProfitLife                                        bool
	}
	allowedSorts := map[string]string{
		"id":     "o.id",
		"did":    "d.e164",
		"status": "o.status",
		"next":   "o.next_billing_at",
	}
	pg := readPagination(r, "/orders", allowedSorts, "id")

	// Live = active / kyc_pending / quarantined (in-flight rentals).
	// Archive = suspended / cancelled (no longer operationally relevant
	// but preserved for audit + revenue history). view=all bypasses both.
	view := r.URL.Query().Get("view")
	if view != "archive" && view != "all" {
		view = "active"
	}

	where := " WHERE 1=1"
	var args []any
	if pg.Q != "" {
		args = append(args, "%"+pg.Q+"%")
		where += fmt.Sprintf(" AND (d.e164 ILIKE $%d OR u.external_id ILIKE $%[1]d OR u.label ILIKE $%[1]d OR cast(o.id as text) = $%d)", len(args), len(args)+1)
		args = append(args, pg.Q)
	}
	if v := r.URL.Query().Get("status"); v != "" {
		args = append(args, v)
		where += fmt.Sprintf(" AND o.status = $%d::assignment_status", len(args))
	} else {
		// No explicit status filter → use the view tab to decide.
		switch view {
		case "active":
			where += " AND o.status IN ('active','kyc_pending','quarantined')"
		case "archive":
			where += " AND o.status IN ('suspended','cancelled')"
		}
	}
	if v := r.URL.Query().Get("reseller_id"); v != "" {
		if v == "0" {
			where += " AND u.reseller_id IS NULL"
		} else if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			args = append(args, n)
			where += fmt.Sprintf(" AND u.reseller_id = $%d", len(args))
		}
	}

	var total int
	h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM orders o JOIN dids d ON d.id=o.did_id JOIN users u ON u.id=o.user_id`+where,
		args...).Scan(&total)
	pg.Total = total

	var activeCount, archiveCount int
	_ = h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM orders WHERE status IN ('active','kyc_pending','quarantined')`).Scan(&activeCount)
	_ = h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM orders WHERE status IN ('suspended','cancelled')`).Scan(&archiveCount)
	if view != "active" {
		pg.BaseArgs = "view=" + view
	}

	args = append(args, pg.Limit(), pg.Offset())
	// Per-order revenue/profit (30d + lifetime). Each money source is a
	// scalar subquery — verbose but readable, and Postgres caches them per
	// row so the cost stays proportional to result set size, not table size.
	//
	// Revenue sources:
	//   cdr_min       cdrs.charge_cents       (per-CDR, snapshotted at billing)
	//   bill_recur    billing_runs (mrc + channel)  (per-anniversary)
	//   nrc           balance_ledger kind='nrc' ref_table='orders'
	//                                             (one-time, at order creation)
	//
	// Supplier cost: CDR portion is snapshotted on the cdrs row; the
	// MRC/NRC portion is approximated from the order's current rate card
	// (no per-event snapshot exists today). Acceptable for live profit at
	// a glance; flagged in the function-level comment for future hardening.
	sql := `
		SELECT o.id, o.user_id, d.e164,
		       COALESCE(u.external_id, u.label, u.contact_email, ''),
		       COALESCE(u.reseller_id, 0),
		       COALESCE(re.name, ''),
		       o.status::text, o.route_kind::text, o.route_target,
		       o.channel_count, o.anniversary_day,
		       COALESCE(o.kyc_bundle_id, 0),
		       to_char(o.next_billing_at,'YYYY-MM-DD'),
		       COALESCE((SELECT b.status::text FROM kyc_bundles b WHERE b.id = o.kyc_bundle_id), ''),

		       -- 30d revenue: CDR minutes + recurring (MRC+channel) + NRC
		       (COALESCE((SELECT SUM(charge_cents) FROM cdrs
		                   WHERE order_id = o.id
		                     AND started_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT SUM(mrc_charged_cents + channel_charged_cents)
		                      FROM billing_runs
		                     WHERE order_id = o.id
		                       AND ran_at > now() - interval '30 days'), 0)
		        + COALESCE((SELECT -SUM(delta_cents) FROM balance_ledger
		                     WHERE ref_table = 'orders' AND ref_id = o.id AND kind = 'nrc'
		                       AND created_at > now() - interval '30 days'), 0)
		       ) AS revenue_30d_cents,

		       -- 30d supplier cost: CDR per-call snapshot
		       --                  + supplier_mrc × # billing runs in window
		       --                  + supplier_nrc if order was created in window
		       (COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		                   WHERE order_id = o.id
		                     AND started_at > now() - interval '30 days'
		                     AND supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE(rc.supplier_mrc_cents, 0) *
		          (SELECT COUNT(*) FROM billing_runs
		            WHERE order_id = o.id
		              AND ran_at > now() - interval '30 days')
		        + CASE WHEN o.assigned_at > now() - interval '30 days'
		               THEN COALESCE(rc.supplier_nrc_cents, 0) ELSE 0 END
		       ) AS supplier_cost_30d_cents,

		       -- Lifetime revenue (same shape, no time filter)
		       (COALESCE((SELECT SUM(charge_cents) FROM cdrs WHERE order_id = o.id), 0)
		        + COALESCE((SELECT SUM(mrc_charged_cents + channel_charged_cents)
		                      FROM billing_runs WHERE order_id = o.id), 0)
		        + COALESCE((SELECT -SUM(delta_cents) FROM balance_ledger
		                     WHERE ref_table = 'orders' AND ref_id = o.id AND kind = 'nrc'), 0)
		       ) AS revenue_life_cents,

		       -- Lifetime supplier cost
		       (COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		                   WHERE order_id = o.id AND supplier_charge_cents IS NOT NULL), 0)
		        + COALESCE(rc.supplier_mrc_cents, 0) *
		          (SELECT COUNT(*) FROM billing_runs WHERE order_id = o.id)
		        + COALESCE(rc.supplier_nrc_cents, 0)
		       ) AS supplier_cost_life_cents,

		       -- has_profit flags: only show profit when we have at least
		       -- one CDR supplier snapshot in scope (i.e. real cost data).
		       EXISTS(SELECT 1 FROM cdrs
		              WHERE order_id = o.id AND started_at > now() - interval '30 days'
		                AND supplier_charge_cents IS NOT NULL) AS has_profit_30d,
		       EXISTS(SELECT 1 FROM cdrs
		              WHERE order_id = o.id AND supplier_charge_cents IS NOT NULL) AS has_profit_life

		  FROM orders o
		  JOIN dids        d  ON d.id  = o.did_id
		  JOIN users       u  ON u.id  = o.user_id
		  LEFT JOIN resellers  re ON re.id = u.reseller_id
		  LEFT JOIN rate_cards rc ON rc.id = o.rate_card_id` + where +
		orderByClause(allowedSorts, pg) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	var out []row
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		h.Log.Error("orders query", "err", err, "sql", sql)
		http.Error(w, "internal", 500)
		return
	}
	for rows.Next() {
		var x row
		var rev30Cents, sup30Cents, revLifeCents, supLifeCents int64
		if err := rows.Scan(&x.ID, &x.UserID, &x.DID, &x.UserRef,
			&x.ResellerID, &x.Reseller,
			&x.Status, &x.RouteKind, &x.RouteTarget,
			&x.ChannelCount, &x.AnniversaryDay, &x.KycBundleID,
			&x.NextBilling, &x.KycStatus,
			&rev30Cents, &sup30Cents, &revLifeCents, &supLifeCents,
			&x.HasProfit30d, &x.HasProfitLife); err == nil {
			x.Revenue30dDollars = float64(rev30Cents) / 100
			x.Profit30dDollars = float64(rev30Cents-sup30Cents) / 100
			x.RevenueLifeDollars = float64(revLifeCents) / 100
			x.ProfitLifeDollars = float64(revLifeCents-supLifeCents) / 100
			out = append(out, x)
		}
	}
	rows.Close()

	// Reseller dropdown for the toolbar.
	type rr struct {
		ID   int64
		Name string
	}
	var resellers []rr
	rrows, _ := h.DB.Query(r.Context(), `SELECT id, name FROM resellers ORDER BY name`)
	for rrows.Next() {
		var x rr
		rrows.Scan(&x.ID, &x.Name)
		resellers = append(resellers, x)
	}
	rrows.Close()

	ok, em := h.popFlashes(r)
	h.render(w, "orders", map[string]any{
		"Title":          "Orders",
		"Section":        "orders",
		"FlashOK":        ok,
		"FlashErr":       em,
		"Orders":         out,
		"Total":          total,
		"Pg":             pg,
		"Resellers":      resellers,
		"FilterReseller": r.URL.Query().Get("reseller_id"),
		"FilterStatus":   r.URL.Query().Get("status"),
		"View":           view,
		"ActiveCount":    activeCount,
		"ArchiveCount":   archiveCount,
	})
}

func (h *Handler) cdrs(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID                                                   int64
		CallID                                               string
		Started, DID, UserRef, Supplier, SrcURI, HangupCause string
		State                                                string
		OrderID                                              int64
		// IDs for the linked-detail-page versions of the corresponding text
		// cells. 0 = no link; the template falls back to bare text.
		DIDID, UserID, SupplierID int64
		Billsec, ChargedMinutes                              int
		ChargeDollars, SupplierChargeDollars, ProfitDollars  float64
		HasProfit                                            bool
		RoutedKind, RoutedTarget                             string
		AdminAction, AdminActionReason                       string // live_hangup | live_warn | live_redirect
	}

	q := r.URL.Query()
	// COALESCE the call's routed_* snapshot with the order's current route
	// so older CDRs (pre-0005 migration) still show *something* in the TO
	// column rather than blank. supplier_charge_cents is post-0011; older
	// rows are NULL → profit column shows "—".
	sql := `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       COALESCE(d.e164, ''),
		       COALESCE(u.external_id, u.label, u.contact_email, ''),
		       COALESCE(s.name, ''), COALESCE(c.src_uri,''),
		       c.billsec, c.charged_minutes, c.charge_cents,
		       c.supplier_charge_cents,
		       COALESCE(c.hangup_cause,''),
		       COALESCE(c.order_id, 0),
		       COALESCE(c.routed_kind::text, COALESCE(o.route_kind::text, '')),
		       COALESCE(c.routed_target,    COALESCE(o.route_target, '')),
		       COALESCE(c.admin_action::text, ''),
		       COALESCE(c.admin_action_reason, ''),
		       COALESCE(d.id, 0),
		       COALESCE(u.id, 0),
		       COALESCE(s.id, 0)
		  FROM cdrs c
		  LEFT JOIN orders    o ON o.id = c.order_id
		  LEFT JOIN dids      d ON d.id = COALESCE(c.did_id, o.did_id)
		  LEFT JOIN users     u ON u.id = c.user_id
		  LEFT JOIN suppliers s ON s.id = c.supplier_id
		 WHERE 1=1`
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}
	if v := q.Get("reseller_id"); v != "" {
		if v == "0" {
			sql += " AND u.reseller_id IS NULL"
		} else if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			add("u.reseller_id =", n)
		}
	}
	if v := q.Get("user_id"); v != "" {
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			add("c.user_id =", n)
		}
	}
	if v := q.Get("order_id"); v != "" {
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			add("c.order_id =", n)
		}
	}
	if v := strings.TrimSpace(q.Get("external_id")); v != "" {
		add("u.external_id =", v)
	}
	if v := strings.TrimSpace(q.Get("did")); v != "" {
		add("d.e164 LIKE", "%"+v+"%")
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			add("c.started_at >=", t)
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			add("c.started_at <", t.Add(24*time.Hour))
		}
	}
	switch q.Get("state") {
	case "answered":
		sql += " AND c.billsec > 0"
	case "failed":
		sql += " AND c.billsec = 0"
	}
	if free := strings.TrimSpace(q.Get("q")); free != "" {
		args = append(args, "%"+free+"%")
		sql += fmt.Sprintf(" AND (c.call_id ILIKE $%d OR c.src_uri ILIKE $%[1]d OR c.dst_uri ILIKE $%[1]d)", len(args))
	}

	allowedSorts := map[string]string{
		"started":  "c.started_at",
		"did":      "d.e164",
		"billsec":  "c.billsec",
		"charge":   "c.charge_cents",
	}
	pg := readPagination(r, "/cdrs", allowedSorts, "started")

	// COUNT
	countSQL := `SELECT count(*) FROM cdrs c LEFT JOIN orders o ON o.id=c.order_id LEFT JOIN dids d ON d.id=COALESCE(c.did_id, o.did_id) LEFT JOIN users u ON u.id=c.user_id LEFT JOIN suppliers s ON s.id=c.supplier_id WHERE 1=1` +
		strings.SplitN(sql, " WHERE 1=1", 2)[1]
	var total int
	h.DB.QueryRow(r.Context(), countSQL, args...).Scan(&total)
	pg.Total = total

	args = append(args, pg.Limit(), pg.Offset())
	sql += orderByClause(allowedSorts, pg) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	var out []row
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		h.Log.Error("cdrs query", "err", err, "sql", sql)
		http.Error(w, "internal", 500)
		return
	}
	for rows.Next() {
		var x row
		var cents int
		var supCents *int
		if err := rows.Scan(&x.ID, &x.CallID, &x.Started, &x.DID, &x.UserRef, &x.Supplier, &x.SrcURI,
			&x.Billsec, &x.ChargedMinutes, &cents, &supCents, &x.HangupCause, &x.OrderID,
			&x.RoutedKind, &x.RoutedTarget, &x.AdminAction, &x.AdminActionReason,
			&x.DIDID, &x.UserID, &x.SupplierID); err == nil {
			x.ChargeDollars = float64(cents) / 100.0
			if supCents != nil {
				x.SupplierChargeDollars = float64(*supCents) / 100.0
				x.ProfitDollars = x.ChargeDollars - x.SupplierChargeDollars
				x.HasProfit = true
			}
			x.State = domain.CallState(x.Billsec, x.HangupCause)
			out = append(out, x)
		}
	}
	rows.Close()

	type rr struct {
		ID   int64
		Name string
	}
	var resellers []rr
	rrows, _ := h.DB.Query(r.Context(), `SELECT id, name FROM resellers ORDER BY name`)
	for rrows.Next() {
		var x rr
		rrows.Scan(&x.ID, &x.Name)
		resellers = append(resellers, x)
	}
	rrows.Close()

	h.render(w, "cdrs", map[string]any{
		"Title":            "CDRs",
		"Section":          "cdrs",
		"CDRs":             out,
		"Total":            total,
		"Resellers":        resellers,
		"FilterReseller":   q.Get("reseller_id"),
		"FilterUserID":     q.Get("user_id"),
		"FilterOrderID":    q.Get("order_id"),
		"FilterExternalID": q.Get("external_id"),
		"FilterDID":        q.Get("did"),
		"FilterFrom":       q.Get("from"),
		"FilterTo":         q.Get("to"),
		"FilterState":      q.Get("state"),
		"Pg":               pg,
	})
}

// deniedCalls is a separate admin-only view for unauthorized_ip / unknown_did
// traffic. These never get billed and never enter the customer-visible CDR
// list, but keeping a record helps spot attack patterns.
func (h *Handler) deniedCalls(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Created, CallID, SrcIP, ToURI, FromURI, Reason string
	}
	var out []row
	rows, err := h.DB.Query(r.Context(), `
		SELECT to_char(created_at,'YYYY-MM-DD HH24:MI:SS'),
		       call_id, host(src_ip), to_uri, COALESCE(from_uri,''), reason
		  FROM denied_calls
		 ORDER BY created_at DESC
		 LIMIT 500`)
	if err != nil {
		h.Log.Error("denied_calls query", "err", err)
		http.Error(w, "internal", 500)
		return
	}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.Created, &x.CallID, &x.SrcIP, &x.ToURI, &x.FromURI, &x.Reason); err == nil {
			out = append(out, x)
		}
	}
	rows.Close()
	h.render(w, "denied_calls", map[string]any{
		"Title":   "Denied calls",
		"Section": "denied",
		"Calls":   out,
		"Total":   len(out),
	})
}

// cdrSipTrace renders the SIP trace for a single call_id. Looks up cdrs
// first; if no CDR row exists (the call was rejected at the supplier-IP-ACL
// stage and only landed in denied_calls), falls back to that table and
// renders with whatever metadata we have. Admin sees the raw trace — no
// IP-rewriting (that's a reseller-API-only sanitization).
func (h *Handler) cdrSipTrace(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "call_id")
	var cdrInfo struct {
		ID                int64
		Started, DID, Ref string
		Billsec, Cents    int
		// RouteKind + RouteTarget = where the call was destined
		// (audio file / SIP peer / forwarded number / IP endpoint).
		// Falls back to the order's configured route when the CDR row
		// doesn't have its own routed_kind yet (denial paths).
		RouteKind, RouteTarget string
		Supplier               string
	}
	// StartedAt / EndedAt as time.Time bound the RTP window when we ask
	// callquality to look at the pcaps.
	var startedAt, endedAt time.Time
	// LEFT JOIN dids/users so denial-style rows (no order or user link)
	// still produce a row.
	err := h.DB.QueryRow(r.Context(), `
		SELECT c.id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       c.started_at, COALESCE(c.ended_at, c.started_at + interval '1 hour'),
		       COALESCE(d.e164,''),
		       COALESCE(u.external_id, u.label, u.contact_email, ''),
		       c.billsec, c.charge_cents,
		       COALESCE(c.routed_kind::text, COALESCE(o.route_kind::text, '')),
		       COALESCE(c.routed_target,    COALESCE(o.route_target, '')),
		       COALESCE(s.name, '')
		  FROM cdrs c
		  LEFT JOIN orders     o ON o.id = c.order_id
		  LEFT JOIN dids       d ON d.id = COALESCE(c.did_id, o.did_id)
		  LEFT JOIN users      u ON u.id = c.user_id
		  LEFT JOIN suppliers  s ON s.id = c.supplier_id
		 WHERE c.call_id = $1`, callID,
	).Scan(&cdrInfo.ID, &cdrInfo.Started, &startedAt, &endedAt, &cdrInfo.DID, &cdrInfo.Ref, &cdrInfo.Billsec, &cdrInfo.Cents,
		&cdrInfo.RouteKind, &cdrInfo.RouteTarget, &cdrInfo.Supplier)
	if errors.Is(err, pgx.ErrNoRows) {
		// Maybe it's a denied_calls row (unauthorized_ip / unknown_did) —
		// they share a call_id namespace and admins still want a trace.
		err = h.DB.QueryRow(r.Context(), `
			SELECT id, to_char(created_at,'YYYY-MM-DD HH24:MI:SS'), to_uri, from_uri
			  FROM denied_calls WHERE call_id = $1 LIMIT 1`, callID,
		).Scan(&cdrInfo.ID, &cdrInfo.Started, &cdrInfo.DID, &cdrInfo.Ref)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "internal", 500)
			return
		}
		cdrInfo.DID = domain.CleanCallerURI(cdrInfo.DID)
		cdrInfo.Ref = domain.CleanCallerURI(cdrInfo.Ref)
	} else if err != nil {
		http.Error(w, "internal", 500)
		return
	}

	// Try the persisted trace blob first — sipctl precomputes & stores it
	// 5s after every call ends, so the common path is a single SELECT
	// (sub-millisecond). Fall through to live tshark on miss / never-
	// computed rows / in-flight calls; persist the result back so the next
	// visitor gets the fast path.
	var tr *siptrace.Trace
	var blob []byte
	var hangupCause string
	_ = h.DB.QueryRow(r.Context(),
		`SELECT siptrace_json, COALESCE(hangup_cause,'') FROM cdrs WHERE call_id = $1`,
		callID).Scan(&blob, &hangupCause)
	if len(blob) > 0 {
		var cached siptrace.Trace
		if err := json.Unmarshal(blob, &cached); err == nil {
			tr = &cached
		}
	}
	if tr == nil {
		var err error
		tr, err = siptrace.Lookup(r.Context(), callID, h.PublicIP)
		if err != nil {
			h.Log.Error("siptrace lookup", "err", err)
			http.Error(w, "trace lookup failed: "+err.Error(), 500)
			return
		}
		// Best-effort: persist for next time if we have a CDR row to attach to.
		if newBlob, err := json.Marshal(tr); err == nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				h.DB.Exec(ctx,
					`UPDATE cdrs SET siptrace_json=$1::jsonb, siptrace_computed_at=now() WHERE call_id=$2`,
					newBlob, callID)
			}()
		}
	}

	supplierIPs := h.loadSupplierIPSet(r)
	// Admin live-action events scoped to this call's order, in the call's
	// time window. We pass these to the diagram builder so it can stamp
	// marker rows onto the ladder right where each action happened.
	adminEvents := h.loadAdminActionEvents(r.Context(), callID)
	diagram := BuildSequenceDiagram(tr, h.PublicIP, supplierIPs, adminEvents)
	endResult := EndResultLabel(cdrInfo.Billsec, hangupCause, tr.FinalSIPCode, tr.FinalSIPReason, len(tr.Messages) > 0)
	findings := BuildFindings(tr, hangupCause)

	// Call quality: run tshark's rtp,streams tap on the same pcaps, scoped
	// to IPs seen in the SIP dialog and the call's time window. Best-effort
	// — if it errors we still render the trace with an empty quality panel.
	dialogIPs := make([]string, 0, len(tr.Endpoints))
	dialogIPs = append(dialogIPs, tr.Endpoints...)
	qualityCtx, qualityCancel := context.WithTimeout(r.Context(), 20*time.Second)
	quality, qErr := callquality.Analyze(qualityCtx, h.PublicIP, dialogIPs, startedAt, endedAt)
	qualityCancel()
	if qErr != nil {
		h.Log.Warn("callquality.Analyze", "err", qErr, "call_id", callID)
		quality = &callquality.Report{
			Verdict: callquality.Verdict{Level: "unknown", Summary: "Quality analysis failed: " + qErr.Error()},
		}
	}

	// Per-message raw frame text — feeds the side panel's "raw" view. JSON-
	// embedded in the page so JS can pull chunk[i] on arrow click without
	// another round-trip. Best-effort: if tshark didn't produce 1:1 frame
	// blocks the JS falls back to showing the whole raw dump.
	frameChunks := SplitRawByFrame(tr.Raw)
	framesJSON, _ := json.Marshal(frameChunks)
	// Messages-as-JSON powers the same panel for items the diagram skipped
	// (self-loops, mid-call info messages) and the click-from-timeline path.
	msgsJSON, _ := json.Marshal(tr.Messages)

	h.render(w, "cdr_trace", map[string]any{
		"Title":         "SIP trace · " + cdrInfo.DID,
		"Section":       "cdrs",
		"CallID":        callID,
		"CDRInfo":       cdrInfo,
		"ChargeDollars": float64(cdrInfo.Cents) / 100.0,
		"HangupCause":   hangupCause,
		"Trace":         tr,
		"Diagram":       diagram,
		"EndResult":     endResult,
		"Findings":      findings,
		"Quality":       quality,
		"FramesJSON":    template.JS(framesJSON),
		"MessagesJSON":  template.JS(msgsJSON),
	})
}

// loadSupplierIPSet returns the set of all supplier-IP literals on file. Used
// by the trace sequence diagram to colour lanes (supplier vs platform vs peer).
func (h *Handler) loadSupplierIPSet(r *http.Request) map[string]bool {
	out := map[string]bool{}
	rows, err := h.DB.Query(r.Context(), `SELECT host(cidr) FROM supplier_ip_group_members`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out[s] = true
		}
	}
	return out
}

// loadAdminActionEvents returns the live-call admin actions logged for the
// CDR's order, bounded to the call's wall-clock window. The trace diagram
// uses these to drop a horizontal marker on the ladder at the moment the
// admin acted, with the action type, admin email and reason.
//
// We scope by order_id + the call's [started_at, ended_at] window because
// a single order can host many calls — we don't want a hangup that
// happened on call #1 showing up on call #2's trace.
func (h *Handler) loadAdminActionEvents(ctx context.Context, callID string) []AdminActionEvent {
	rows, err := h.DB.Query(ctx, `
		SELECT bl.action::text,
		       COALESCE(ad.email,''),
		       COALESCE(bl.reason,''),
		       EXTRACT(EPOCH FROM bl.created_at)::float8,
		       to_char(bl.created_at AT TIME ZONE 'UTC','HH24:MI:SS')
		  FROM user_block_log bl
		  LEFT JOIN admins   ad ON ad.id = bl.blocked_by
		  JOIN cdrs c            ON c.order_id = bl.order_id
		 WHERE c.call_id = $1
		   AND bl.action::text IN ('live_hangup','live_warn','live_redirect')
		   AND bl.created_at BETWEEN c.started_at - interval '5 seconds'
		                         AND c.ended_at   + interval '5 seconds'
		 ORDER BY bl.created_at ASC`, callID)
	if err != nil {
		h.Log.Warn("loadAdminActionEvents query", "err", err, "call_id", callID)
		return nil
	}
	defer rows.Close()
	var out []AdminActionEvent
	for rows.Next() {
		var ev AdminActionEvent
		if err := rows.Scan(&ev.Action, &ev.AdminEmail, &ev.Reason, &ev.UnixTime, &ev.TimeLabel); err == nil {
			out = append(out, ev)
		}
	}
	return out
}

// compactDuration renders a billsec count as a one-glance label:
//
//	0           → "0s"
//	45          → "45s"
//	125         → "2m 5s"
//	5400        → "1h 30m"
//
// We never lose information — the precise form lives in the tooltip.
func compactDuration(billsec int) string {
	if billsec <= 0 {
		return "0s"
	}
	h := billsec / 3600
	m := (billsec % 3600) / 60
	s := billsec % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// preciseDuration is the HH:MM:SS form for the tooltip / CSV. We don't have
// millisecond precision — Asterisk's CDR(billsec) is integer seconds and the
// /cdr wire format we receive is also int64 unix-seconds — so the ms field
// would be a lie. Drop it.
func preciseDuration(billsec int) string {
	if billsec < 0 {
		billsec = 0
	}
	h := billsec / 3600
	m := (billsec % 3600) / 60
	s := billsec % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// causeDescribe returns (short label, long detail) for any known cause code.
// Source of truth is the hangup_causes DB table, mirrored into an in-memory
// map by the causes package. Admins edit the table via /cause-codes; the
// table is reloaded after every write.
func causeDescribe(s string) (string, string) {
	if s == "" {
		return "", ""
	}
	return causes.Describe(s)
}

// humanizeHangupCause returns the rendered <span>label</span> + info-icon
// tooltip suitable for any CDR-list cell. The actual code (16, 17,
// "insufficient_channels", …) lives in the tooltip detail; the visible label
// is the human-readable phrase.
func humanizeHangupCause(s string) template.HTML {
	if s == "" {
		return ""
	}
	label, detail := causeDescribe(s)
	if detail == "" {
		// Unknown code — render verbatim, no tooltip.
		return template.HTML(template.HTMLEscapeString(label))
	}
	return template.HTML(
		`<span class="cause-label">` + template.HTMLEscapeString(label) + `</span>` +
			` <span class="tip" data-tip="` + template.HTMLEscapeString(detail) + `">` +
			string(iconSVG("info")) +
			`</span>`,
	)
}

// countActiveCalls scans Redis act:user:* keys (renamed from act:order:*) and
// sums up SCARDs for a quick "calls in flight" gauge.
func countActiveCalls(ctx context.Context, rdb *redis.Client) int {
	var cursor uint64
	total := 0
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "act:user:*", 200).Result()
		if err != nil {
			return total
		}
		for _, k := range keys {
			n, _ := rdb.SCard(ctx, k).Result()
			total += int(n)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return total
}
