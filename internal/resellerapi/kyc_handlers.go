package resellerapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"didstorage/internal/kyc"
)

// KYC endpoints exposed to resellers. Resellers create bundles and upload
// documents on behalf of their users; admins approve / reject in the admin
// GUI. We never let a reseller approve their own bundles.

type kycBundleOut struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"user_id"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	ApprovedAt *string `json:"approved_at,omitempty"`
	RejectedAt *string `json:"rejected_at,omitempty"`
	Reason     string  `json:"rejection_reason,omitempty"`
	DocCount   int     `json:"document_count"`
}

const kycBundleSelectCols = `b.id, b.user_id, b.type::text, b.status::text,
    to_char(b.created_at,'YYYY-MM-DDTHH24:MI:SS'),
    to_char(b.approved_at,'YYYY-MM-DDTHH24:MI:SS'),
    to_char(b.rejected_at,'YYYY-MM-DDTHH24:MI:SS'),
    COALESCE(b.rejection_reason,''),
    (SELECT count(*) FROM kyc_documents WHERE bundle_id = b.id)`

func scanKycBundle(rows interface{ Scan(...any) error }) (kycBundleOut, error) {
	var b kycBundleOut
	var approved, rejected *string
	if err := rows.Scan(&b.ID, &b.UserID, &b.Type, &b.Status, &b.CreatedAt,
		&approved, &rejected, &b.Reason, &b.DocCount); err != nil {
		return b, err
	}
	b.ApprovedAt = approved
	b.RejectedAt = rejected
	return b, nil
}

// createKycBundle: POST /api/v1/users/{id}/kyc-bundles
//
//	body: {"type": "person"|"company", "info": {...}}
//
// Always starts as 'pending' — admins approve later.
func (h *Handler) createKycBundle(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req struct {
		Type string         `json:"type"`
		Info map[string]any `json:"info"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if req.Type != "person" && req.Type != "company" {
		writeErr(w, 400, `type must be "person" or "company"`)
		return
	}
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	infoBytes, _ := json.Marshal(req.Info)
	if len(infoBytes) == 0 || string(infoBytes) == "null" {
		infoBytes = []byte("{}")
	}
	var id int64
	err := h.DB.QueryRow(r.Context(), `
		INSERT INTO kyc_bundles (user_id, type, status, info)
		VALUES ($1, $2::kyc_bundle_type, 'pending', $3::jsonb) RETURNING id`,
		uid, req.Type, string(infoBytes),
	).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "status": "pending"})
}

func (h *Handler) listKycBundles(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	rows, err := h.DB.Query(r.Context(),
		`SELECT `+kycBundleSelectCols+` FROM kyc_bundles b WHERE b.user_id=$1 ORDER BY b.id DESC`, uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []kycBundleOut
	for rows.Next() {
		b, err := scanKycBundle(rows)
		if err == nil {
			out = append(out, b)
		}
	}
	writeJSON(w, 200, map[string]any{"bundles": out})
}

func (h *Handler) getKycBundle(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	bid, _ := strconv.ParseInt(chi.URLParam(r, "bid"), 10, 64)
	rows, err := h.DB.Query(r.Context(), `
		SELECT `+kycBundleSelectCols+`
		  FROM kyc_bundles b
		  JOIN users u ON u.id = b.user_id
		 WHERE b.id=$1 AND u.reseller_id=$2`, bid, rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	if !rows.Next() {
		writeErr(w, 404, "bundle not found")
		return
	}
	b, err := scanKycBundle(rows)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// Documents (filename + size only — never the file bytes here)
	type docOut struct {
		ID         int64  `json:"id"`
		Kind       string `json:"kind"`
		Filename   string `json:"filename"`
		MimeType   string `json:"mime_type"`
		SizeBytes  int64  `json:"size_bytes"`
		UploadedAt string `json:"uploaded_at"`
	}
	var docs []docOut
	rows.Close()
	drows, err := h.DB.Query(r.Context(), `
		SELECT id, kind::text, filename, mime_type, size_bytes,
		       to_char(uploaded_at,'YYYY-MM-DDTHH24:MI:SS')
		  FROM kyc_documents WHERE bundle_id=$1 ORDER BY id`, bid)
	if err == nil {
		for drows.Next() {
			var d docOut
			drows.Scan(&d.ID, &d.Kind, &d.Filename, &d.MimeType, &d.SizeBytes, &d.UploadedAt)
			docs = append(docs, d)
		}
		drows.Close()
	}
	writeJSON(w, 200, map[string]any{"bundle": b, "documents": docs})
}

// uploadKycDocument: POST multipart/form-data, fields: file, kind
func (h *Handler) uploadKycDocument(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	bid, _ := strconv.ParseInt(chi.URLParam(r, "bid"), 10, 64)
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		writeErr(w, 400, "upload too large or malformed: "+err.Error())
		return
	}
	kind := strings.TrimSpace(r.PostForm.Get("kind"))
	if kind == "" {
		kind = "other"
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "no file in 'file' field")
		return
	}
	defer file.Close()

	// Verify the bundle belongs to a user under this reseller.
	var userID int64
	err = h.DB.QueryRow(r.Context(), `
		SELECT b.user_id FROM kyc_bundles b
		  JOIN users u ON u.id=b.user_id
		 WHERE b.id=$1 AND u.reseller_id=$2`, bid, rid).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "bundle not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	filename, err := kyc.SanitizeFilename(hdr.Filename)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dir, err := kyc.BundleDir(userID, bid)
	if err != nil {
		writeErr(w, 500, "storage init: "+err.Error())
		return
	}
	dst, err := os.OpenFile(dir+"/"+filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		writeErr(w, 500, "storage write: "+err.Error())
		return
	}
	n, err := io.Copy(dst, file)
	dst.Close()
	if err != nil {
		os.Remove(dir + "/" + filename)
		writeErr(w, 500, "stream: "+err.Error())
		return
	}

	mime := hdr.Header.Get("Content-Type")
	storagePath := fmt.Sprintf("%d/%d/%s", userID, bid, filename)
	var id int64
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO kyc_documents (bundle_id, kind, filename, mime_type, size_bytes, storage_path)
		VALUES ($1, $2::kyc_doc_kind, $3, $4, $5, $6) RETURNING id`,
		bid, kind, filename, mime, n, storagePath,
	).Scan(&id)
	if err != nil {
		os.Remove(dir + "/" + filename)
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"id":         id,
		"filename":   filename,
		"size_bytes": n,
	})
}

func (h *Handler) downloadKycDocument(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	bid, _ := strconv.ParseInt(chi.URLParam(r, "bid"), 10, 64)
	docID, _ := strconv.ParseInt(chi.URLParam(r, "did"), 10, 64)
	var userID int64
	var filename, mime string
	err := h.DB.QueryRow(r.Context(), `
		SELECT b.user_id, d.filename, d.mime_type
		  FROM kyc_documents d
		  JOIN kyc_bundles  b ON b.id=d.bundle_id
		  JOIN users        u ON u.id=b.user_id
		 WHERE d.id=$1 AND d.bundle_id=$2 AND u.reseller_id=$3`,
		docID, bid, rid,
	).Scan(&userID, &filename, &mime)
	if err != nil {
		writeErr(w, 404, "doc not found")
		return
	}
	f, err := os.Open(kyc.DocPath(userID, bid, filename))
	if err != nil {
		writeErr(w, 500, "file missing on disk: "+err.Error())
		return
	}
	defer f.Close()
	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	w.Header().Set("Content-Disposition", `inline; filename="`+filename+`"`)
	io.Copy(w, f)
}

// userBelongsToReseller returns true if the user is under the caller's
// reseller. On false it has already written an appropriate error response —
// the caller just needs to return.
func (h *Handler) userBelongsToReseller(w http.ResponseWriter, r *http.Request, userID, rid int64) bool {
	var ownedRID *int64
	err := h.DB.QueryRow(r.Context(),
		`SELECT reseller_id FROM users WHERE id=$1`, userID).Scan(&ownedRID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "user not found")
		return false
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return false
	}
	if ownedRID == nil || *ownedRID != rid {
		writeErr(w, 403, "user not in your reseller")
		return false
	}
	return true
}
