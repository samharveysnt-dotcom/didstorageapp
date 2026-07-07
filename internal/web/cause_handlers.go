package web

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"didstorage/internal/causes"
)

// causeCodes lists every hangup-cause row in the table. Renders the admin
// edit page (/cause-codes).
func (h *Handler) causeCodes(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	familyFilter := strings.TrimSpace(r.URL.Query().Get("family"))

	all := causes.All()
	var filtered []causes.Info
	for _, c := range all {
		if familyFilter != "" && c.Family != familyFilter {
			continue
		}
		if q != "" {
			ql := strings.ToLower(q)
			if !strings.Contains(strings.ToLower(c.Code), ql) &&
				!strings.Contains(strings.ToLower(c.Label), ql) &&
				!strings.Contains(strings.ToLower(c.Detail), ql) {
				continue
			}
		}
		filtered = append(filtered, c)
	}

	ok, em := h.popFlashes(r)
	h.render(w, "cause_codes", map[string]any{
		"Title":         "Hangup causes",
		"Section":       "causes",
		"FlashOK":       ok,
		"FlashErr":      em,
		"Causes":        filtered,
		"TotalAll":      len(all),
		"FilterQ":       q,
		"FilterFamily":  familyFilter,
	})
}

// causeCodeUpsert handles BOTH "create new" (when no code exists yet) and
// "edit existing" (label / detail / family for any code, even built-ins).
func (h *Handler) causeCodeUpsert(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/cause-codes", http.StatusFound)
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	label := strings.TrimSpace(r.PostForm.Get("label"))
	detail := strings.TrimSpace(r.PostForm.Get("detail"))
	family := strings.TrimSpace(r.PostForm.Get("family"))
	if family == "" {
		family = "platform"
	}
	if err := causes.Upsert(r.Context(), h.DB.Pool, code, label, detail, family); err != nil {
		h.flashErr(r, "save: "+err.Error())
	} else {
		h.flashOK(r, "Saved cause '"+code+"'")
	}
	http.Redirect(w, r, "/cause-codes", http.StatusFound)
}

// causeCodeDelete removes a non-builtin cause. Built-ins are protected at
// the package layer (it returns an error if the row's builtin=true).
func (h *Handler) causeCodeDelete(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if err := causes.Delete(r.Context(), h.DB.Pool, code); err != nil {
		h.flashErr(r, "delete: "+err.Error())
	} else {
		h.flashOK(r, "Deleted cause '"+code+"'")
	}
	http.Redirect(w, r, "/cause-codes", http.StatusFound)
}
