package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"didstorage/internal/audio"
	"didstorage/internal/auth"
)

// audioFiles is the GET /audio-files admin page: a table of every clip in
// the library plus the upload form. Rendered with the same chrome as the
// other admin pages.
func (h *Handler) audioFiles(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID               int64
		Name             string
		Filename         string
		OriginalFilename string
		SizeBytes        int64
		Duration         string
		Format           string
		CreatedAt        string
		CreatedBy        string
		InUseByDIDs      int
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT af.id, af.name, af.filename,
		       COALESCE(af.original_filename,''),
		       af.size_bytes, af.duration_ms, af.format,
		       to_char(af.created_at,'YYYY-MM-DD HH24:MI'),
		       COALESCE(a.email,''),
		       (SELECT count(*) FROM dids d
		         WHERE d.reserved_audio_file_id = af.id)
		  FROM audio_files af
		  LEFT JOIN admins a ON a.id = af.created_by
		 ORDER BY af.created_at DESC
	`)
	if err != nil {
		h.Log.Error("audioFiles list query", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []row{}
	for rows.Next() {
		var x row
		var ms int
		if err := rows.Scan(&x.ID, &x.Name, &x.Filename, &x.OriginalFilename,
			&x.SizeBytes, &ms, &x.Format, &x.CreatedAt, &x.CreatedBy, &x.InUseByDIDs); err != nil {
			h.Log.Error("audioFiles scan", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		x.Duration = audio.FormatDuration(ms)
		out = append(out, x)
	}

	ok, em := h.popFlashes(r)
	h.render(w, "audio_files", map[string]any{
		"Title":          "Audio library",
		"Section":        "audio",
		"Files":          out,
		"MaxUploadBytes": audio.MaxUploadBytes,
		"FlashOK":        ok,
		"FlashErr":       em,
	})
}

// audioFileUpload accepts a multipart upload, converts the input to slin,
// inserts a row, and redirects back to /audio-files. The display name is
// optional — when blank we fall back to the upload filename minus extension.
// Either way it must be unique.
func (h *Handler) audioFileUpload(w http.ResponseWriter, r *http.Request) {
	// Limit the request body up front so a malicious upload can't fill the
	// disk before we get a chance to validate. MaxBytesReader wraps the
	// body so ParseMultipartForm sees an early EOF on overrun.
	r.Body = http.MaxBytesReader(w, r.Body, int64(audio.MaxUploadBytes)+1024)
	if err := r.ParseMultipartForm(int64(audio.MaxUploadBytes)); err != nil {
		h.flashErr(r, fmt.Sprintf("Upload too large or malformed. Maximum is %d MB.", audio.MaxUploadBytes/1024/1024))
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	file, hdr, err := r.FormFile("audio")
	if err != nil {
		h.flashErr(r, "No file attached. Pick an audio file to upload.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	defer file.Close()

	ext := audio.SafeOriginalExt(hdr.Filename)
	if ext == "" {
		h.flashErr(r, "Unsupported file type. Use mp3, wav, m4a, ogg, opus, flac, aac, webm, slin, ulaw, alaw, or gsm.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	// Resolve the display name. Trim whitespace; fall back to the upload
	// filename's stem. Strip any non-printable nonsense from operator input
	// since name shows up in dropdowns next to admin labels.
	displayName := strings.TrimSpace(r.FormValue("name"))
	if displayName == "" {
		base := filepath.Base(hdr.Filename)
		displayName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	displayName = sanitizeAudioName(displayName)
	if displayName == "" {
		h.flashErr(r, "Name must contain at least one printable character.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	// Stash the upload to a temp file before invoking ffmpeg — ffmpeg can't
	// read from stdin for every container (m4a needs seek), and a real
	// on-disk path also means we get free filename-based format detection.
	if err := audio.EnsureDir(); err != nil {
		h.Log.Error("audio upload mkdir", "err", err)
		h.flashErr(r, "server error preparing sounds directory")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	tmp, err := os.CreateTemp("", "didstorage-audio-*"+ext)
	if err != nil {
		h.Log.Error("audio upload tempfile", "err", err)
		h.flashErr(r, "server error: tempfile")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, file); err != nil {
		_ = tmp.Close()
		h.Log.Error("audio upload copy", "err", err)
		h.flashErr(r, "upload truncated")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	if err := tmp.Close(); err != nil {
		h.Log.Error("audio upload close", "err", err)
		h.flashErr(r, "server error: close tempfile")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 70*time.Second)
	defer cancel()
	conv, err := audio.Convert(ctx, tmpPath)
	if err != nil {
		h.Log.Error("audio convert failed", "err", err, "src", hdr.Filename)
		h.flashErr(r, "Conversion failed. Confirm ffmpeg is installed on the server, then retry. Server logs hold the details.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	adminID := auth.AdminIDFromSession(h.Session, r)
	var adminPtr any
	if adminID > 0 {
		adminPtr = adminID
	}

	_, err = h.DB.Exec(r.Context(), `
		INSERT INTO audio_files
		    (name, filename, original_filename,
		     size_bytes, duration_ms, format, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, displayName, conv.Filename, hdr.Filename,
		conv.SizeBytes, conv.DurationMS, conv.Format, adminPtr)
	if err != nil {
		// Roll back the disk file — the DB row didn't take so we'd
		// otherwise orphan the conversion output. Common cause is a
		// duplicate name (UNIQUE violation, 23505).
		_ = audio.Delete(conv.Filename)
		if pgErr := pgErrCode(err); pgErr == "23505" {
			h.flashErr(r, "An audio file named "+displayName+" already exists. Pick a different name.")
		} else {
			h.Log.Error("audio insert", "err", err)
			h.flashErr(r, "Save failed. The server logs hold the details; ask the operator on call.")
		}
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	h.flashOK(r, "Uploaded "+displayName+" ("+audio.FormatDuration(conv.DurationMS)+").")
	http.Redirect(w, r, "/audio-files", http.StatusFound)
}

// audioFileBulkUpload accepts an arbitrary number of multipart files under
// the form key "audio" (browsers send each file in a multi-input under that
// same name) plus a `name_prefix` and an optional `group_id`. Each file is
// converted to slin and inserted as audio_files row, named
// "<prefix>-<N>" where N starts at 1 and skips over any (prefix, N) the
// library already has so re-uploading doesn't collide. If group_id > 0 the
// new files are also added to that audio_group as members. The whole batch
// is best-effort per file: a single conversion failure flashes a warning
// and continues with the remainder so the operator doesn't lose the rest
// of the upload to one bad mp3.
//
// Why a separate endpoint from audioFileUpload: the single-file form has a
// single `name` text input that lets the operator override the display name.
// Bulk uploads never want per-file naming — that's the whole point of the
// "<prefix>-N" convention. Keeping the routes separate keeps each form
// honest about what it does.
func (h *Handler) audioFileBulkUpload(w http.ResponseWriter, r *http.Request) {
	// Bulk can be much larger than single-upload; allow N × MaxUploadBytes.
	const maxFiles = 200
	maxBytes := int64(audio.MaxUploadBytes) * int64(maxFiles)
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024)
	if err := r.ParseMultipartForm(int64(audio.MaxUploadBytes)); err != nil {
		h.flashErr(r, "Upload too large or malformed. Try fewer files at a time.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	files := r.MultipartForm.File["audio"]
	if len(files) == 0 {
		h.flashErr(r, "No files attached.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	if len(files) > maxFiles {
		h.flashErr(r, fmt.Sprintf("Too many files in one batch — split into batches of %d or fewer.", maxFiles))
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	prefix := sanitizeAudioName(strings.TrimSpace(r.FormValue("name_prefix")))
	if prefix == "" {
		h.flashErr(r, "Name prefix is required for bulk uploads (becomes \"prefix-1\", \"prefix-2\", ...).")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	groupID := atoi64(r.FormValue("group_id"))
	// Validate group_id if present: row must exist before we drop members in.
	if groupID > 0 {
		var exists bool
		_ = h.DB.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM audio_groups WHERE id = $1)`, groupID).Scan(&exists)
		if !exists {
			h.flashErr(r, "Selected audio group no longer exists. Refresh and pick another.")
			http.Redirect(w, r, "/audio-files", http.StatusFound)
			return
		}
	}

	if err := audio.EnsureDir(); err != nil {
		h.Log.Error("bulk upload mkdir", "err", err)
		h.flashErr(r, "server error preparing sounds directory")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	// Find the next free index for this prefix so re-uploading "welcome"
	// after already having welcome-1..3 starts at welcome-4. We pull the
	// max existing N once, then increment locally per file inserted.
	var maxN int
	err := h.DB.QueryRow(r.Context(), `
		SELECT COALESCE(MAX(
		  NULLIF(regexp_replace(name, '^' || $1 || '-(\d+)$', '\1'), name)::int
		), 0)
		FROM audio_files
		WHERE name ~ ('^' || $1 || '-\d+$')`, prefix).Scan(&maxN)
	if err != nil {
		h.Log.Warn("bulk upload max-N probe", "err", err, "prefix", prefix)
		// Non-fatal: fall back to 0 and risk a unique-name collision per file.
		maxN = 0
	}

	adminID := auth.AdminIDFromSession(h.Session, r)
	var adminPtr any
	if adminID > 0 {
		adminPtr = adminID
	}

	type result struct {
		name string
		ok   bool
		err  string
	}
	results := make([]result, 0, len(files))
	for i, fh := range files {
		n := maxN + 1 + i
		displayName := fmt.Sprintf("%s-%d", prefix, n)
		res := result{name: displayName}
		if err := h.bulkUploadOne(r.Context(), fh, displayName, adminPtr, groupID); err != nil {
			res.err = err.Error()
			results = append(results, res)
			continue
		}
		res.ok = true
		results = append(results, res)
	}

	good := 0
	bad := 0
	var badMsgs []string
	for _, r := range results {
		if r.ok {
			good++
		} else {
			bad++
			badMsgs = append(badMsgs, fmt.Sprintf("%s: %s", r.name, r.err))
		}
	}
	if good > 0 {
		msg := fmt.Sprintf("Uploaded %d audio file%s.", good, plural(good))
		if groupID > 0 {
			msg += " Added to selected group."
		}
		h.flashOK(r, msg)
	}
	if bad > 0 {
		// Combine bad reasons into one flash, capped.
		msg := fmt.Sprintf("%d file%s failed: %s", bad, plural(bad), strings.Join(badMsgs, "; "))
		if len(msg) > 600 {
			msg = msg[:597] + "..."
		}
		h.flashErr(r, msg)
	}
	http.Redirect(w, r, "/audio-files", http.StatusFound)
}

// bulkUploadOne is the per-file inner loop for audioFileBulkUpload: copy
// the upload to a tempfile, convert, insert the audio_files row, optionally
// link to a group. Returns an error suitable for flashing to the operator.
func (h *Handler) bulkUploadOne(ctx context.Context, fh *multipart.FileHeader, displayName string, adminPtr any, groupID int64) error {
	ext := audio.SafeOriginalExt(fh.Filename)
	if ext == "" {
		return fmt.Errorf("unsupported extension")
	}
	src, err := fh.Open()
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "didstorage-bulk-*"+ext)
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	convCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	conv, err := audio.Convert(convCtx, tmpPath)
	if err != nil {
		return fmt.Errorf("convert: %v", err)
	}

	var newID int64
	err = h.DB.QueryRow(ctx, `
		INSERT INTO audio_files (name, filename, original_filename,
		                          size_bytes, duration_ms, format, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id
	`, displayName, conv.Filename, fh.Filename,
		conv.SizeBytes, conv.DurationMS, conv.Format, adminPtr).Scan(&newID)
	if err != nil {
		_ = audio.Delete(conv.Filename)
		if pgErrCode(err) == "23505" {
			return fmt.Errorf("name already exists")
		}
		return fmt.Errorf("db insert: %w", err)
	}

	if groupID > 0 {
		// Position = current MAX(position)+1 for the group. Best-effort:
		// a concurrent insert could race and produce equal positions,
		// which is fine — the PK is (group_id, audio_file_id), not position.
		var nextPos int
		_ = h.DB.QueryRow(ctx,
			`SELECT COALESCE(MAX(position)+1, 0) FROM audio_group_members WHERE group_id = $1`,
			groupID).Scan(&nextPos)
		if _, err := h.DB.Exec(ctx,
			`INSERT INTO audio_group_members (group_id, audio_file_id, position)
			 VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			groupID, newID, nextPos); err != nil {
			h.Log.Warn("bulk upload add-to-group", "err", err, "group_id", groupID, "file_id", newID)
			// Don't fail the row — the file is uploaded, just not in the group.
		}
	}
	return nil
}

// plural returns "s" when n != 1; helper for messages like "1 file" vs "3 files".
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// audioFileRename changes only the display name. The on-disk filename
// stays the same so any reserved DID's AGI response keeps working without
// a regen step.
func (h *Handler) audioFileRename(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	name := sanitizeAudioName(strings.TrimSpace(r.FormValue("name")))
	if name == "" {
		h.flashErr(r, "name must contain at least one printable character")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	tag, err := h.DB.Exec(r.Context(), `UPDATE audio_files SET name = $1 WHERE id = $2`, name, id)
	if err != nil {
		if pgErrCode(err) == "23505" {
			h.flashErr(r, "An audio file named "+name+" already exists. Pick a different name.")
		} else {
			h.Log.Error("audio rename", "err", err)
			h.flashErr(r, "Rename failed. Check the server logs for details.")
		}
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "Audio file not found. It may have been deleted in another tab.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	h.flashOK(r, "Renamed to "+name+".")
	http.Redirect(w, r, "/audio-files", http.StatusFound)
}

// audioFileDelete refuses to delete a file that any DID still references.
// If you really want to delete it, release the reservation first. We then
// delete both the DB row and the on-disk file in that order so a Postgres
// error doesn't leave us with a row pointing at nothing.
func (h *Handler) audioFileDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var (
		filename string
		inUse    int
		name     string
	)
	err := h.DB.QueryRow(r.Context(), `
		SELECT af.filename, af.name,
		       (SELECT count(*) FROM dids d WHERE d.reserved_audio_file_id = af.id)
		  FROM audio_files af WHERE af.id = $1
	`, id).Scan(&filename, &name, &inUse)
	if errors.Is(err, pgx.ErrNoRows) {
		h.flashErr(r, "Audio file not found. It may have been deleted in another tab.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	if err != nil {
		h.Log.Error("audio delete lookup", "err", err)
		h.flashErr(r, "Delete failed. Check the server logs for details.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	if inUse > 0 {
		h.flashErr(r, fmt.Sprintf("Cannot delete %q: still reserved on %d DID(s). Release the reservation first.", name, inUse))
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}

	if _, err := h.DB.Exec(r.Context(), `DELETE FROM audio_files WHERE id = $1`, id); err != nil {
		h.Log.Error("audio delete row", "err", err)
		h.flashErr(r, "Delete failed. Check the server logs for details.")
		http.Redirect(w, r, "/audio-files", http.StatusFound)
		return
	}
	if err := audio.Delete(filename); err != nil {
		// Row's already gone; log loudly but don't fail the request.
		h.Log.Warn("audio delete disk", "err", err, "filename", filename)
	}

	h.flashOK(r, "Deleted "+name+".")
	http.Redirect(w, r, "/audio-files", http.StatusFound)
}

// audioFilePlay streams the clip back to the browser as a WAV-wrapped
// 8kHz mono 16-bit file. The on-disk format is raw PCM (slin) which no
// browser plays directly; we prepend a 44-byte RIFF/WAVE header so the
// HTML5 <audio> element treats it as a normal WAV. Cheap and accurate
// to the byte.
func (h *Handler) audioFilePlay(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var filename, name string
	err := h.DB.QueryRow(r.Context(),
		`SELECT filename, name FROM audio_files WHERE id = $1`, id).Scan(&filename, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.Log.Error("audio play lookup", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	rc, size, err := audio.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "audio file missing on disk", http.StatusGone)
			return
		}
		h.Log.Error("audio play open", "err", err, "filename", filename)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	header := wavHeaderPCM16(size, 8000, 1)
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(header))+size))
	// Conservative caching: clip content is immutable per filename so a long
	// max-age is safe; admin browsers benefit from instant re-play.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeFilenameHeader(name)+".wav\"")
	if _, err := w.Write(header); err != nil {
		return
	}
	_, _ = io.Copy(w, rc)
}

// audioFileOptions returns JSON [{id, name, filename, duration_ms}] for
// client-side dropdown population. Listed newest-first so the most-
// recently-uploaded clip is the top suggestion. `filename` is the on-disk
// basename (no extension); the order-route edit form uses it to match
// the current route_target ("didstorage/<filename>") so the dropdown
// pre-selects the existing pick.
func (h *Handler) audioFileOptions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, name, filename, duration_ms FROM audio_files ORDER BY created_at DESC
	`)
	if err != nil {
		h.Log.Error("audio options query", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type opt struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Filename   string `json:"filename"`
		DurationMS int    `json:"duration_ms"`
		Duration   string `json:"duration"`
	}
	out := []opt{}
	for rows.Next() {
		var x opt
		if err := rows.Scan(&x.ID, &x.Name, &x.Filename, &x.DurationMS); err == nil {
			x.Duration = audio.FormatDuration(x.DurationMS)
			out = append(out, x)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// pgErrCode returns the Postgres SQLSTATE for a wrapped pgx error, or
// "" if it's not a Postgres error. Used to distinguish duplicate-name
// (23505) from other failure modes. Matches the convention used in
// internal/didsimport/worker.go.
func pgErrCode(err error) string {
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// sanitizeAudioName strips control chars and trims leading/trailing
// whitespace from operator-supplied names. Anything goes inside the
// string — including spaces, punctuation, Unicode — as long as it's
// printable. Length capped at 80 to fit nicely in dropdowns.
func sanitizeAudioName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 80 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// sanitizeFilenameHeader returns a Content-Disposition-safe rendition of
// the clip's display name: quotes are stripped, only ASCII letters /
// digits / -_. survive. Browsers fall back to the URL path filename when
// the header is missing, which is fine — this is just a nicety.
func sanitizeFilenameHeader(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "audio"
	}
	return out
}

// wavHeaderPCM16 builds a 44-byte canonical RIFF/WAVE header for
// uncompressed 16-bit signed-int PCM. dataSize is the raw byte length
// of the audio payload that will follow. Caller writes header then
// streams the payload.
func wavHeaderPCM16(dataSize int64, sampleRate uint32, channels uint16) []byte {
	const (
		headerSize     = 44
		fmtChunkSize   = 16
		audioFormatPCM = 1
		bitsPerSample  = 16
	)
	byteRate := sampleRate * uint32(channels) * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	chunkSize := uint32(36) + uint32(dataSize)

	buf := make([]byte, headerSize)
	copy(buf[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(buf[4:8], chunkSize)
	copy(buf[8:12], []byte("WAVE"))
	copy(buf[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(buf[16:20], fmtChunkSize)
	binary.LittleEndian.PutUint16(buf[20:22], audioFormatPCM)
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))
	return buf
}
