package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"didstorage/internal/auth"
	"didstorage/internal/kyc"
)

// MountKYC attaches /kyc-bundles/* routes inside the admin group. Called from
// New() / Mount(). Kept in a separate file because there's a lot of it.
//
// Route summary:
//
//	POST /users/{id}/kyc-bundles                       (create — under user)
//	GET  /kyc-bundles/{bid}                            (detail page)
//	POST /kyc-bundles/{bid}/documents                  (upload doc, multipart)
//	GET  /kyc-bundles/{bid}/documents/{did}/download   (download a doc)
//	POST /kyc-bundles/{bid}/documents/{did}/delete     (delete a doc)
//	POST /kyc-bundles/{bid}/approve                    (admin approve)
//	POST /kyc-bundles/{bid}/reject                     (admin reject + reason)
func (h *Handler) mountKYC(parent http.Handler) {} // dummy — actual mount in web.go

// kycBundleCreate is form-posted from a user's detail page.
func (h *Handler) kycBundleCreate(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	kind := strings.TrimSpace(r.PostForm.Get("type"))
	if kind != "person" && kind != "company" {
		h.flashErr(r, "type must be 'person' or 'company'")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	// `info` is free-form JSON-ish in v1: we accept whatever the operator
	// pasted (legal name, dob, registration number, etc.) and store it as a
	// JSON object {"notes": "..."} when no JSON was supplied.
	notes := strings.TrimSpace(r.PostForm.Get("notes"))
	infoJSON := strings.TrimSpace(r.PostForm.Get("info_json"))
	if infoJSON == "" {
		infoJSON = fmt.Sprintf(`{"notes":%q}`, notes)
	}
	var bid int64
	err := h.DB.QueryRow(r.Context(), `
		INSERT INTO kyc_bundles (user_id, type, status, info, notes)
		VALUES ($1, $2::kyc_bundle_type, 'pending', $3::jsonb, $4)
		RETURNING id`,
		uid, kind, infoJSON, notes,
	).Scan(&bid)
	if err != nil {
		h.flashErr(r, "create kyc bundle: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	h.flashOK(r, "KYC bundle created — upload documents next")
	http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
}

// kycBundleDetail shows one bundle's info and documents, with admin
// approve / reject controls.
func (h *Handler) kycBundleDetail(w http.ResponseWriter, r *http.Request) {
	bid := pathID(r, "bid")
	var b struct {
		ID                int64
		UserID            int64
		Type, Status      string
		Info, Notes       string
		Created, Approved string
		Rejected, Reason  string
		ApprovedBy        string
		UserRef           string
	}
	err := h.DB.QueryRow(r.Context(), `
		SELECT b.id, b.user_id, b.type::text, b.status::text,
		       b.info::text, COALESCE(b.notes,''),
		       to_char(b.created_at,'YYYY-MM-DD HH24:MI'),
		       COALESCE(to_char(b.approved_at,'YYYY-MM-DD HH24:MI'),''),
		       COALESCE(to_char(b.rejected_at,'YYYY-MM-DD HH24:MI'),''),
		       COALESCE(b.rejection_reason,''),
		       COALESCE((SELECT a.email FROM admins a WHERE a.id=b.approved_by), ''),
		       COALESCE(u.external_id, u.label, u.contact_email, '')
		  FROM kyc_bundles b JOIN users u ON u.id=b.user_id
		 WHERE b.id=$1`, bid,
	).Scan(&b.ID, &b.UserID, &b.Type, &b.Status, &b.Info, &b.Notes,
		&b.Created, &b.Approved, &b.Rejected, &b.Reason, &b.ApprovedBy, &b.UserRef)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}

	type doc struct {
		ID         int64
		Kind       string
		Filename   string
		MimeType   string
		SizeBytes  int64
		Uploaded   string
	}
	var docs []doc
	rows, _ := h.DB.Query(r.Context(), `
		SELECT id, kind::text, filename, mime_type, size_bytes,
		       to_char(uploaded_at,'YYYY-MM-DD HH24:MI')
		  FROM kyc_documents WHERE bundle_id=$1 ORDER BY id`, bid)
	for rows.Next() {
		var d doc
		rows.Scan(&d.ID, &d.Kind, &d.Filename, &d.MimeType, &d.SizeBytes, &d.Uploaded)
		docs = append(docs, d)
	}
	rows.Close()

	ok, em := h.popFlashes(r)
	h.render(w, "kyc_detail", map[string]any{
		"Title":    "KYC bundle #" + fmt.Sprintf("%d", b.ID),
		"Section":  "users",
		"FlashOK":  ok,
		"FlashErr": em,
		"Bundle":   b,
		"Docs":     docs,
	})
}

// kycDocUpload accepts a single file via multipart/form-data and stores it
// under /var/lib/didstorage/kyc/{user}/{bundle}/{filename}.
func (h *Handler) kycDocUpload(w http.ResponseWriter, r *http.Request) {
	bid := pathID(r, "bid")
	// 25 MB cap per upload — IDs and address proofs don't need more, and
	// keeping it small limits the blast radius if an admin upload goes wrong.
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		h.flashErr(r, "upload too large or malformed: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	kind := strings.TrimSpace(r.PostForm.Get("kind"))
	if kind == "" {
		kind = "other"
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		h.flashErr(r, "no file submitted")
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	defer file.Close()

	filename, err := kyc.SanitizeFilename(hdr.Filename)
	if err != nil {
		h.flashErr(r, "filename: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}

	var userID int64
	if err := h.DB.QueryRow(r.Context(),
		`SELECT user_id FROM kyc_bundles WHERE id=$1`, bid).Scan(&userID); err != nil {
		h.flashErr(r, "bundle not found")
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}

	dir, err := kyc.BundleDir(userID, bid)
	if err != nil {
		h.flashErr(r, "storage init: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	dst, err := os.OpenFile(dir+"/"+filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		h.flashErr(r, "storage write: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	n, err := io.Copy(dst, file)
	dst.Close()
	if err != nil {
		os.Remove(dir + "/" + filename)
		h.flashErr(r, "stream: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}

	mime := hdr.Header.Get("Content-Type")
	storagePath := fmt.Sprintf("%d/%d/%s", userID, bid, filename)
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO kyc_documents (bundle_id, kind, filename, mime_type, size_bytes, storage_path)
		VALUES ($1, $2::kyc_doc_kind, $3, $4, $5, $6)`,
		bid, kind, filename, mime, n, storagePath); err != nil {
		os.Remove(dir + "/" + filename)
		h.flashErr(r, "db insert: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}

	h.flashOK(r, fmt.Sprintf("Uploaded %s (%d bytes)", filename, n))
	http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
}

// kycDocDownload streams a document file to the admin's browser.
func (h *Handler) kycDocDownload(w http.ResponseWriter, r *http.Request) {
	bid := pathID(r, "bid")
	docID := pathID(r, "did")
	var userID int64
	var filename, mime string
	err := h.DB.QueryRow(r.Context(), `
		SELECT b.user_id, d.filename, d.mime_type
		  FROM kyc_documents d JOIN kyc_bundles b ON b.id=d.bundle_id
		 WHERE d.id=$1 AND d.bundle_id=$2`, docID, bid,
	).Scan(&userID, &filename, &mime)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := kyc.DocPath(userID, bid, filename)
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file missing on disk: "+err.Error(), 500)
		return
	}
	defer f.Close()
	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	w.Header().Set("Content-Disposition", `inline; filename="`+filename+`"`)
	io.Copy(w, f)
}

func (h *Handler) kycDocDelete(w http.ResponseWriter, r *http.Request) {
	bid := pathID(r, "bid")
	docID := pathID(r, "did")
	var userID int64
	var filename string
	err := h.DB.QueryRow(r.Context(), `
		SELECT b.user_id, d.filename
		  FROM kyc_documents d JOIN kyc_bundles b ON b.id=d.bundle_id
		 WHERE d.id=$1 AND d.bundle_id=$2`, docID, bid,
	).Scan(&userID, &filename)
	if err != nil {
		h.flashErr(r, "doc not found")
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	_, err = h.DB.Exec(r.Context(), `DELETE FROM kyc_documents WHERE id=$1`, docID)
	if err != nil {
		h.flashErr(r, "delete: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	_ = os.Remove(kyc.DocPath(userID, bid, filename))
	h.flashOK(r, "Document deleted")
	http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
}

// kycApprove flips bundle to approved and activates any kyc_pending orders
// that referenced it.
func (h *Handler) kycApprove(w http.ResponseWriter, r *http.Request) {
	bid := pathID(r, "bid")
	adminID := auth.AdminIDFromSession(h.Session, r)
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())
	activated, err := kyc.ApproveBundle(r.Context(), tx, bid, adminID)
	if err != nil {
		h.flashErr(r, "approve: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "commit: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	if activated > 0 {
		h.flashOK(r, fmt.Sprintf("Bundle approved — %d order(s) activated", activated))
	} else {
		h.flashOK(r, "Bundle approved")
	}
	http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
}

func (h *Handler) kycReject(w http.ResponseWriter, r *http.Request) {
	bid := pathID(r, "bid")
	adminID := auth.AdminIDFromSession(h.Session, r)
	r.ParseForm()
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		h.flashErr(r, "rejection reason required")
		http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
		return
	}
	if err := kyc.RejectBundle(r.Context(), h.DB.Pool, bid, adminID, reason); err != nil {
		h.flashErr(r, "reject: "+err.Error())
	} else {
		h.flashOK(r, "Bundle rejected")
	}
	http.Redirect(w, r, fmt.Sprintf("/kyc-bundles/%d", bid), http.StatusFound)
}
