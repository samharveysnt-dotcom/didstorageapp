package didsimport

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

// Status is the lifecycle state of a job. Browsers see it via the SSE
// "init"/"progress"/"done" events; the registry keeps it for the
// fallback /status polling endpoint and for cleanup.
type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusErrored Status = "errored"
)

// EventType discriminates the SSE payload shape on the client.
type EventType string

const (
	EventInit     EventType = "init"     // sent once when the worker starts
	EventRow      EventType = "row"      // per-row outcome
	EventProgress EventType = "progress" // periodic counter snapshot
	EventDone     EventType = "done"     // sent once when the worker exits
)

// RowStatus is what happened to one input row.
type RowStatus string

const (
	RowAdded   RowStatus = "added"
	RowSkipped RowStatus = "skipped"
	RowError   RowStatus = "error"
)

// Event is the on-the-wire JSON shape the browser receives via SSE. Fields
// are zero-valued / omitted depending on the Type so a single struct works
// for every event kind.
type Event struct {
	Type EventType `json:"type"`
	At   int64     `json:"at"` // unix milli

	// Per-row fields. Populated only when Type == EventRow.
	E164      string    `json:"e164,omitempty"`
	RowStatus RowStatus `json:"row_status,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Country   string    `json:"country,omitempty"`
	DIDType   string    `json:"did_type,omitempty"`

	// Counters. Populated on init/progress/done.
	Total     int `json:"total,omitempty"`
	Added     int `json:"added,omitempty"`
	Skipped   int `json:"skipped,omitempty"`
	Errors    int `json:"errors,omitempty"`
	Processed int `json:"processed,omitempty"`

	// Done-only fields.
	DurationMs int64  `json:"duration_ms,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Defaults are the form-level fallbacks the worker applies to any entry that
// doesn't carry an explicit override. SupplierID is always required at the
// form level (the only way an entry can carry a different supplier is via the
// CSV path), so it's stored on the Job, not here.
type Defaults struct {
	Country    string // ISO-3166 alpha-2; "" means auto-detect from prefix
	DIDType    string // mobile / national / local / tollfree
	ChannelCap int    // -1 = unlimited (matches dids.supplier_channel_cap convention)
}

// Entry is one row of input ready for the worker. CSV rows can carry
// per-row overrides; range / bulk-line modes leave the override fields
// blank and the worker pulls from Defaults / the Job's SupplierID.
type Entry struct {
	E164       string
	Country    string // optional override; "" = auto-detect / default
	DIDType    string // optional override
	SupplierID int64  // optional override; 0 = use Job.SupplierID
	ChannelCap int    // optional override; 0 = use Defaults.ChannelCap; -1 = unlimited
}

// Job is a single in-flight import. The browser subscribes to Events to see
// per-row outcomes + counter snapshots. Counters are written atomically so
// the SSE goroutine and the worker goroutine don't need to share a lock.
//
// Lifetime: created in Registry.New, started by Run, finished when the
// worker emits EventDone and closes Events. The registry retains the Job
// for a short grace window so a late SSE reconnect can replay the final
// state via /status, then reaps.
type Job struct {
	ID         string
	SupplierID int64
	Source     string // "range" / "bulk" / "csv" — for logging only
	Defaults   Defaults
	CreatedAt  time.Time

	// Counters — read with atomic.LoadInt64 from any goroutine.
	total     atomic.Int64
	added     atomic.Int64
	skipped   atomic.Int64
	errors    atomic.Int64
	processed atomic.Int64

	mu       sync.RWMutex
	status   Status
	finishAt time.Time
	finalErr string

	// Events is the buffered channel the worker writes to and the SSE
	// handler reads from. Closed when the worker exits.
	Events chan Event
}

// Status returns the current lifecycle state. Safe to call from any goroutine.
func (j *Job) Status() Status {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.status
}

// Counters returns a snapshot tuple of (total, added, skipped, errors,
// processed) so the SSE handler / status endpoint can build a Progress event
// without taking any lock.
func (j *Job) Counters() (int, int, int, int, int) {
	return int(j.total.Load()),
		int(j.added.Load()),
		int(j.skipped.Load()),
		int(j.errors.Load()),
		int(j.processed.Load())
}

// setStatus is the only path that flips Status. Worker calls it on start /
// finish; the registry's reaper checks Status to know when a Job is collectable.
func (j *Job) setStatus(s Status, finalErr string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = s
	if s == StatusDone || s == StatusErrored {
		j.finishAt = time.Now()
	}
	if finalErr != "" {
		j.finalErr = finalErr
	}
}

// emit pushes an event into the bus. Non-blocking with a 1-second timeout to
// guard against a slow SSE consumer holding up the worker — if the consumer
// is that far behind it's probably gone, and the registry's final-state
// snapshot covers them on reconnect anyway.
func (j *Job) emit(ev Event) {
	if ev.At == 0 {
		ev.At = time.Now().UnixMilli()
	}
	select {
	case j.Events <- ev:
	case <-time.After(1 * time.Second):
		// drop the event silently — the next progress tick will catch the
		// counter state up
	}
}

// Registry is the in-memory store of import jobs keyed by short hex id.
// One per Handler; lives for the lifetime of the didapi process. Old jobs
// are reaped every reapInterval after they reach a terminal state.
type Registry struct {
	mu             sync.RWMutex
	jobs           map[string]*Job
	retainAfterEnd time.Duration
	stopReaper     chan struct{}
}

const (
	defaultRetainAfterEnd = 10 * time.Minute
	reapInterval          = 1 * time.Minute
	eventChanBuffer       = 1024
)

// NewRegistry constructs a Registry and starts the background reaper. The
// returned object is safe for concurrent use across the entire process.
func NewRegistry() *Registry {
	r := &Registry{
		jobs:           map[string]*Job{},
		retainAfterEnd: defaultRetainAfterEnd,
		stopReaper:     make(chan struct{}),
	}
	go r.reaper()
	return r
}

// Stop signals the reaper to exit. Useful from a test; the production
// didapi process doesn't call this because we never gracefully shut down.
func (r *Registry) Stop() { close(r.stopReaper) }

// New registers an empty Job ready to be populated by a worker. Caller
// supplies the form-level supplier id + defaults; the worker fills in
// counters as it runs.
func (r *Registry) New(supplierID int64, source string, defaults Defaults) *Job {
	j := &Job{
		ID:         newID(),
		SupplierID: supplierID,
		Source:     source,
		Defaults:   defaults,
		CreatedAt:  time.Now(),
		status:     StatusPending,
		Events:     make(chan Event, eventChanBuffer),
	}
	r.mu.Lock()
	r.jobs[j.ID] = j
	r.mu.Unlock()
	return j
}

// Get returns a Job by id, or nil if not found / already reaped.
func (r *Registry) Get(id string) *Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.jobs[id]
}

func (r *Registry) reaper() {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stopReaper:
			return
		case <-t.C:
			r.reapOnce()
		}
	}
}

func (r *Registry) reapOnce() {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, j := range r.jobs {
		j.mu.RLock()
		done := j.status == StatusDone || j.status == StatusErrored
		past := !j.finishAt.IsZero() && now.Sub(j.finishAt) > r.retainAfterEnd
		j.mu.RUnlock()
		if done && past {
			delete(r.jobs, id)
		}
	}
}

// newID returns a 12-char hex id. Short enough to fit in a URL, random
// enough to avoid guess-collisions in the (tiny) admin-only namespace.
func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
