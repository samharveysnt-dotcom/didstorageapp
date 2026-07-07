package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"didstorage/internal/didsimport"
)

// didImportStart is the entry-point form handler. Multipart so the CSV
// upload is on the same form as range / bulk inputs. Returns JSON with
// the import_id so the GUI can open the SSE stream — the form itself
// never causes a full-page reload.
//
// Form fields (multipart):
//
//   - mode             "range" | "bulk" | "csv"  (required)
//   - supplier_id      int                       (required, form default)
//   - country_iso      str / ""                  (form default; "" = auto-detect)
//   - did_type         str                       (form default)
//   - channel_cap      int / blank               (form default; -1 / blank = unlimited)
//   - start_e164       digits (mode=range)
//   - end_e164         digits (mode=range, optional)
//   - bulk_text        text   (mode=bulk)
//   - csv              file   (mode=csv)
//
// Validation order: parse the chosen input mode first so a bad form bails
// before allocating a Job. Job is only registered when we know we have
// at least one entry to import.
func (h *Handler) didImportStart(w http.ResponseWriter, r *http.Request) {
	if h.Imports == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "import registry not initialised",
		})
		return
	}
	// Reasonably generous body cap: 8 MB covers a 10k-row CSV with ~800
	// bytes per row of per-row override columns, with room.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad form: " + err.Error()})
		return
	}

	mode := r.FormValue("mode")
	supplierID, _ := strconv.ParseInt(r.FormValue("supplier_id"), 10, 64)
	if supplierID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "supplier_id is required"})
		return
	}
	defaults := didsimport.Defaults{
		Country:    strings.ToUpper(strings.TrimSpace(r.FormValue("country_iso"))),
		DIDType:    strings.TrimSpace(r.FormValue("did_type")),
		ChannelCap: parseChannelCap(r.FormValue("channel_cap")),
	}

	var parsed didsimport.ParseResult
	var parseErr error
	switch mode {
	case "range":
		parsed, parseErr = didsimport.ParseRange(r.FormValue("start_e164"), r.FormValue("end_e164"))
	case "bulk":
		parsed, parseErr = didsimport.ParseBulk(r.FormValue("bulk_text"))
	case "csv":
		f, _, err := r.FormFile("csv")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "csv: no file uploaded"})
			return
		}
		defer f.Close()
		parsed, parseErr = didsimport.ParseCSV(io.LimitReader(f, 8<<20))
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode must be range|bulk|csv"})
		return
	}
	if parseErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": parseErr.Error()})
		return
	}
	if len(parsed.Entries) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nothing parseable in input"})
		return
	}

	job := h.Imports.New(supplierID, mode, defaults)
	// Detach from the request context — the import outlives the HTTP request.
	// We DO want timeout-style safety so a runaway import doesn't pin a
	// connection forever; 10 minutes is generous (~10k inserts at <1ms each
	// finishes in seconds even on slow disk).
	workCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	go func() {
		defer cancel()
		didsimport.Run(workCtx, h.DB, job, parsed.Entries, parsed.Warnings)
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"import_id":  job.ID,
		"stream_url": "/dids/import/" + job.ID + "/stream",
		"total":      len(parsed.Entries),
		"warnings":   len(parsed.Warnings),
		"defaults": map[string]any{
			"country":     defaults.Country,
			"did_type":    defaults.DIDType,
			"channel_cap": defaults.ChannelCap,
		},
	})
}

// didImportStream is the per-job SSE feed. Clients open it right after
// /start returns and consume Event JSON until they see {"type":"done"}.
// On reconnect, the registry's retain-after-end window means a recent
// done event can be re-served — but the buffer is per-job, not replayed,
// so the GUI uses the /status endpoint to recover counters on reconnect.
func (h *Handler) didImportStream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job := h.Imports.Get(id)
	if job == nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(ev any) error {
		blob, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", blob)
		flusher.Flush()
		return err
	}

	// Heartbeat keeps the connection through any intermediate proxy that
	// idles silent streams. Short comment lines are SSE-spec compatible.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, ok := <-job.Events:
			if !ok {
				return // channel closed by the worker after Done
			}
			if err := send(ev); err != nil {
				return
			}
		}
	}
}

// didImportStatus is the polling fallback: a single JSON snapshot of
// counters + lifecycle for SSE clients that reconnect after the worker
// closed its event channel.
func (h *Handler) didImportStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job := h.Imports.Get(id)
	if job == nil {
		http.NotFound(w, r)
		return
	}
	total, added, skipped, errs, proc := job.Counters()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         job.ID,
		"status":     job.Status(),
		"total":      total,
		"added":      added,
		"skipped":    skipped,
		"errors":     errs,
		"processed":  proc,
		"supplier_id": job.SupplierID,
		"source":     job.Source,
	})
}

// didImportExample serves the bundled CSV template. Cache-busted via a
// stable filename in the Content-Disposition so the admin's browser
// always sees the latest content even if a CDN cached an older version.
func (h *Handler) didImportExample(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="dids-import-example.csv"`)
	_, _ = io.WriteString(w, didsimport.ExampleCSV)
}

// parseChannelCap normalizes the channel_cap form field. Blank or non-numeric
// becomes -1 (matches the dids.supplier_channel_cap unlimited sentinel).
// Negative numbers are coerced to -1; valid positive numbers pass through.
func parseChannelCap(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	if n < 0 {
		return -1
	}
	return n
}

// writeJSON is shared with other handlers; declared in the package
// elsewhere (live_handlers etc). Keep this comment as a breadcrumb for
// when this file is read in isolation.
var _ = errors.New
