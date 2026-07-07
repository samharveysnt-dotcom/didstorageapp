package web

// Audio groups: a named set of audio_files clips that, when used as a
// reserved-DID route, plays a DIFFERENT clip on each incoming call.
//
// Selection lives in sipctl.pickAudioGroupMember (random-no-repeat, Redis
// last-played key). This file owns the admin GUI: list / detail / create /
// rename / delete groups, and add / remove individual files to/from a
// group. Bulk-upload-into-a-group is in audio_handlers.go because it
// shares the upload pipeline with single-file uploads.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"didstorage/internal/audio"
	"didstorage/internal/auth"
)

// audioGroups is the GET /audio-groups admin page: a table of every group
// with its member count, DID-usage count, and a "new group" form.
func (h *Handler) audioGroups(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID           int64
		Name         string
		Note         string
		CreatedAt    string
		CreatedBy    string
		MemberCount  int
		DIDUsage     int
		TotalSeconds int
		TotalLabel   string
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT g.id, g.name, COALESCE(g.note,''),
		       to_char(g.created_at,'YYYY-MM-DD HH24:MI'),
		       COALESCE(a.email,''),
		       (SELECT count(*)       FROM audio_group_members m WHERE m.group_id = g.id),
		       (SELECT count(*)       FROM dids d WHERE d.reserved_audio_group_id = g.id),
		       COALESCE((SELECT SUM(af.duration_ms)
		                   FROM audio_group_members m
		                   JOIN audio_files af ON af.id = m.audio_file_id
		                  WHERE m.group_id = g.id), 0) / 1000
		  FROM audio_groups g
		  LEFT JOIN admins a ON a.id = g.created_by
		 ORDER BY g.created_at DESC
	`)
	if err != nil {
		h.Log.Error("audioGroups list", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []row{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ID, &x.Name, &x.Note, &x.CreatedAt, &x.CreatedBy,
			&x.MemberCount, &x.DIDUsage, &x.TotalSeconds); err != nil {
			h.Log.Error("audioGroups scan", "err", err)
			continue
		}
		x.TotalLabel = audio.FormatDuration(x.TotalSeconds * 1000)
		out = append(out, x)
	}
	ok, em := h.popFlashes(r)
	h.render(w, "audio_groups", map[string]any{
		"Title":    "Audio groups",
		"Section":  "audio",
		"Groups":   out,
		"FlashOK":  ok,
		"FlashErr": em,
	})
}

// audioGroupCreate handles POST /audio-groups — name + optional note,
// redirects to /audio-groups/{newID} so the admin can immediately add
// members.
func (h *Handler) audioGroupCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := sanitizeAudioName(strings.TrimSpace(r.FormValue("name")))
	note := strings.TrimSpace(r.FormValue("note"))
	if name == "" {
		h.flashErr(r, "Group name is required.")
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	adminID := auth.AdminIDFromSession(h.Session, r)
	var adminPtr any
	if adminID > 0 {
		adminPtr = adminID
	}
	var newID int64
	err := h.DB.QueryRow(r.Context(),
		`INSERT INTO audio_groups (name, note, created_by)
		 VALUES ($1, NULLIF($2,''), $3) RETURNING id`,
		name, note, adminPtr).Scan(&newID)
	if err != nil {
		if pgErrCode(err) == "23505" {
			h.flashErr(r, "A group named "+name+" already exists.")
			http.Redirect(w, r, "/audio-groups", http.StatusFound)
			return
		}
		h.Log.Error("audioGroupCreate", "err", err)
		h.flashErr(r, "Create failed. Check the server logs for details.")
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	h.flashOK(r, "Group "+name+" created.")
	http.Redirect(w, r, fmt.Sprintf("/audio-groups/%d", newID), http.StatusFound)
}

// audioGroupDetail is GET /audio-groups/{id}: the group's metadata, its
// members in display order, the pool of unassigned audio files for the
// "add existing" picker, and a count of DIDs currently reserved to this
// group.
func (h *Handler) audioGroupDetail(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	type group struct {
		ID          int64
		Name        string
		Note        string
		CreatedAt   string
		CreatedBy   string
		DIDUsage    int
		MemberCount int
	}
	var g group
	g.ID = id
	err := h.DB.QueryRow(r.Context(), `
		SELECT name, COALESCE(note,''),
		       to_char(created_at,'YYYY-MM-DD HH24:MI'),
		       (SELECT COALESCE(email,'')      FROM admins a WHERE a.id = audio_groups.created_by),
		       (SELECT count(*)                FROM dids d  WHERE d.reserved_audio_group_id = audio_groups.id),
		       (SELECT count(*)                FROM audio_group_members m WHERE m.group_id = audio_groups.id)
		  FROM audio_groups WHERE id = $1`,
		id).Scan(&g.Name, &g.Note, &g.CreatedAt, &g.CreatedBy, &g.DIDUsage, &g.MemberCount)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.Log.Error("audioGroupDetail header", "err", err, "id", id)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	type member struct {
		ID         int64
		Name       string
		Filename   string
		DurationMS int
		Duration   string
		Position   int
		AddedAt    string
		SizeBytes  int64
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT af.id, af.name, af.filename, af.duration_ms, af.size_bytes,
		       m.position, to_char(m.added_at,'YYYY-MM-DD HH24:MI')
		  FROM audio_group_members m
		  JOIN audio_files af ON af.id = m.audio_file_id
		 WHERE m.group_id = $1
		 ORDER BY m.position, af.name`, id)
	if err != nil {
		h.Log.Error("audioGroupDetail members", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	members := []member{}
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.ID, &m.Name, &m.Filename, &m.DurationMS, &m.SizeBytes,
			&m.Position, &m.AddedAt); err != nil {
			h.Log.Error("audioGroupDetail members scan", "err", err)
			continue
		}
		m.Duration = audio.FormatDuration(m.DurationMS)
		members = append(members, m)
	}
	rows.Close()

	// Files NOT already in this group — feed the "Add existing" dropdown.
	type pick struct {
		ID       int64
		Name     string
		Duration string
	}
	prows, err := h.DB.Query(r.Context(), `
		SELECT af.id, af.name, af.duration_ms
		  FROM audio_files af
		 WHERE NOT EXISTS (
		   SELECT 1 FROM audio_group_members m
		    WHERE m.group_id = $1 AND m.audio_file_id = af.id)
		 ORDER BY af.name`, id)
	if err != nil {
		h.Log.Error("audioGroupDetail picker", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	picks := []pick{}
	for prows.Next() {
		var p pick
		var ms int
		if err := prows.Scan(&p.ID, &p.Name, &ms); err == nil {
			p.Duration = audio.FormatDuration(ms)
			picks = append(picks, p)
		}
	}
	prows.Close()

	ok, em := h.popFlashes(r)
	h.render(w, "audio_group_detail", map[string]any{
		"Title":         "Audio group · " + g.Name,
		"Section":       "audio",
		"Group":         g,
		"Members":       members,
		"AvailableFiles": picks,
		"MaxUploadBytes": audio.MaxUploadBytes,
		"FlashOK":        ok,
		"FlashErr":       em,
	})
}

// audioGroupUpdate handles POST /audio-groups/{id} — rename / edit note.
func (h *Handler) audioGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	name := sanitizeAudioName(strings.TrimSpace(r.FormValue("name")))
	note := strings.TrimSpace(r.FormValue("note"))
	back := fmt.Sprintf("/audio-groups/%d", id)
	if name == "" {
		h.flashErr(r, "Group name is required.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	tag, err := h.DB.Exec(r.Context(),
		`UPDATE audio_groups SET name = $1, note = NULLIF($2,'') WHERE id = $3`,
		name, note, id)
	if err != nil {
		if pgErrCode(err) == "23505" {
			h.flashErr(r, "A group named "+name+" already exists.")
		} else {
			h.Log.Error("audioGroupUpdate", "err", err)
			h.flashErr(r, "Save failed. Check the server logs for details.")
		}
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "Group not found.")
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	h.flashOK(r, "Group saved.")
	http.Redirect(w, r, back, http.StatusFound)
}

// audioGroupDelete drops a group. Refuses if any DID is still reserved
// against it — operator must release those reservations first or
// reassign them, mirroring the audio_files delete-guard.
func (h *Handler) audioGroupDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var inUse int
	var name string
	err := h.DB.QueryRow(r.Context(), `
		SELECT g.name,
		       (SELECT count(*) FROM dids d WHERE d.reserved_audio_group_id = g.id)
		  FROM audio_groups g WHERE g.id = $1`, id).Scan(&name, &inUse)
	if errors.Is(err, pgx.ErrNoRows) {
		h.flashErr(r, "Group not found.")
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	if err != nil {
		h.Log.Error("audioGroupDelete lookup", "err", err)
		h.flashErr(r, "Delete failed. Check the server logs for details.")
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	if inUse > 0 {
		h.flashErr(r, fmt.Sprintf("Cannot delete %q: still reserved on %d DID(s). Release them first.", name, inUse))
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	if _, err := h.DB.Exec(r.Context(), `DELETE FROM audio_groups WHERE id = $1`, id); err != nil {
		h.Log.Error("audioGroupDelete row", "err", err)
		h.flashErr(r, "Delete failed. Check the server logs for details.")
		http.Redirect(w, r, "/audio-groups", http.StatusFound)
		return
	}
	h.flashOK(r, "Group "+name+" deleted.")
	http.Redirect(w, r, "/audio-groups", http.StatusFound)
}

// audioGroupAddMember links an existing audio_files row into the group.
// Idempotent via ON CONFLICT DO NOTHING. Position lands at max+1.
func (h *Handler) audioGroupAddMember(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	r.ParseForm()
	afID := atoi64(r.FormValue("audio_file_id"))
	back := fmt.Sprintf("/audio-groups/%d", gid)
	if afID <= 0 {
		h.flashErr(r, "Pick an audio file to add.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	var nextPos int
	_ = h.DB.QueryRow(r.Context(),
		`SELECT COALESCE(MAX(position)+1, 0) FROM audio_group_members WHERE group_id = $1`,
		gid).Scan(&nextPos)
	_, err := h.DB.Exec(r.Context(),
		`INSERT INTO audio_group_members (group_id, audio_file_id, position)
		 VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, gid, afID, nextPos)
	if err != nil {
		h.Log.Error("audioGroupAddMember", "err", err)
		h.flashErr(r, "Add failed. Check the server logs for details.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	h.flashOK(r, "File added to group.")
	http.Redirect(w, r, back, http.StatusFound)
}

// audioGroupRemoveMember unlinks an audio_files row from the group. The
// file itself stays in the library and in any OTHER groups it's part of.
func (h *Handler) audioGroupRemoveMember(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	afID := pathID(r, "afid")
	back := fmt.Sprintf("/audio-groups/%d", gid)
	if _, err := h.DB.Exec(r.Context(),
		`DELETE FROM audio_group_members WHERE group_id = $1 AND audio_file_id = $2`,
		gid, afID); err != nil {
		h.Log.Error("audioGroupRemoveMember", "err", err)
		h.flashErr(r, "Remove failed. Check the server logs for details.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	h.flashOK(r, "File removed from group.")
	http.Redirect(w, r, back, http.StatusFound)
}

// audioGroupOptions returns JSON [{id, name, member_count}] for the DID
// reserve modal's group dropdown.
func (h *Handler) audioGroupOptions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT g.id, g.name,
		       (SELECT count(*) FROM audio_group_members m WHERE m.group_id = g.id)
		  FROM audio_groups g ORDER BY g.name`)
	if err != nil {
		h.Log.Error("audioGroupOptions", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type opt struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		MemberCount int    `json:"member_count"`
	}
	out := []opt{}
	for rows.Next() {
		var x opt
		if err := rows.Scan(&x.ID, &x.Name, &x.MemberCount); err == nil {
			out = append(out, x)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
