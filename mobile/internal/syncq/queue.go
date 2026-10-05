// Package syncq holds transfers that can wait, and runs them when a link worth
// using appears.
//
// This is the half of the design that actually saves money, and it rests on an
// observation: most bytes are not urgent. Map tiles, podcasts, photo backup,
// package updates, offline course material and system images are the bulk of
// what a connection carries, and none of it is waited on by a human. Only
// navigation, messaging, calls and the page somebody is looking at right now
// need the link they need at the moment they need it.
//
// Separating the two means an expensive or scarce link only ever carries the
// traffic that justifies it. On a school uplink the effect is larger than on a
// vehicle: one slow line can serve a classroom if the fifty-megabyte curriculum
// download happens overnight instead of during a lesson.
package syncq

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Item is one deferred transfer.
type Item struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Dest  string `json:"dest"`
	Class string `json:"class"`

	// SizeBytes is the expected size, used to decide whether a transfer can
	// finish inside the window a link is likely to stay up. Zero means unknown.
	SizeBytes int64 `json:"size_bytes"`

	AddedAt     time.Time `json:"added_at"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`

	BytesDone int64  `json:"bytes_done"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`

	// OverLink records which link carried it, so the cost of a transfer can be
	// attributed after the fact.
	OverLink string `json:"over_link,omitempty"`
}

// Done reports whether the item finished.
func (i Item) Done() bool { return !i.CompletedAt.IsZero() }

// Queue is a persistent list of deferred transfers.
type Queue struct {
	mu    sync.Mutex
	path  string
	items map[string]*Item

	// maxAttempts stops a permanently broken URL from being retried forever
	// every time a link comes up, which on a metered link costs real money.
	maxAttempts int
}

// Options configures a queue.
type Options struct {
	Path        string
	MaxAttempts int
}

// Open loads a queue from disk, creating an empty one if absent.
func Open(opts Options) (*Queue, error) {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	q := &Queue{
		path:        opts.Path,
		items:       make(map[string]*Item),
		maxAttempts: opts.MaxAttempts,
	}

	raw, err := os.ReadFile(opts.Path)
	if os.IsNotExist(err) {
		return q, nil
	}
	if err != nil {
		return nil, fmt.Errorf("syncq: read %s: %w", opts.Path, err)
	}
	if len(raw) == 0 {
		return q, nil
	}

	var items []*Item
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("syncq: parse %s: %w", opts.Path, err)
	}
	for _, it := range items {
		q.items[it.ID] = it
	}
	return q, nil
}

// Add enqueues a transfer. Adding the same ID twice is a no-op, so a scheduled
// job can enqueue its work every hour without duplicating it.
func (q *Queue) Add(it Item) error {
	if it.ID == "" || it.URL == "" || it.Dest == "" {
		return fmt.Errorf("syncq: id, url and dest are all required")
	}
	if it.Class == "" {
		it.Class = "bulk"
	}

	q.mu.Lock()
	if _, exists := q.items[it.ID]; exists {
		q.mu.Unlock()
		return nil
	}
	it.AddedAt = time.Now().UTC()
	q.items[it.ID] = &it
	q.mu.Unlock()

	return q.save()
}

// Pending returns unfinished items of a class, oldest first, skipping anything
// that has exhausted its attempts.
func (q *Queue) Pending(class string) []Item {
	q.mu.Lock()
	defer q.mu.Unlock()

	var out []Item
	for _, it := range q.items {
		if it.Done() || it.Class != class || it.Attempts >= q.maxAttempts {
			continue
		}
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

// All returns every item, for the status endpoint.
func (q *Queue) All() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]Item, 0, len(q.items))
	for _, it := range q.items {
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

// Stats summarises the queue.
type Stats struct {
	Pending      int   `json:"pending"`
	Completed    int   `json:"completed"`
	Failed       int   `json:"failed"`
	PendingBytes int64 `json:"pending_bytes"`
}

// Stats returns a summary.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()

	var s Stats
	for _, it := range q.items {
		switch {
		case it.Done():
			s.Completed++
		case it.Attempts >= q.maxAttempts:
			s.Failed++
		default:
			s.Pending++
			s.PendingBytes += it.SizeBytes - it.BytesDone
		}
	}
	return s
}

// Transfer downloads one item over the given link.
//
// Resumes with a Range request when a partial file is present, because a
// vehicle leaving coverage mid-transfer is the normal case rather than the
// exception, and restarting a large download from zero every time guarantees it
// never completes.
func (q *Queue) Transfer(ctx context.Context, id, linkName string, client *http.Client) error {
	q.mu.Lock()
	it, ok := q.items[id]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("syncq: no item %q", id)
	}
	item := *it
	q.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(item.Dest), 0o755); err != nil {
		return q.fail(id, fmt.Errorf("create destination directory: %w", err))
	}

	partial := item.Dest + ".part"
	var resumeFrom int64
	if info, err := os.Stat(partial); err == nil {
		resumeFrom = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil)
	if err != nil {
		return q.fail(id, err)
	}
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := client.Do(req)
	if err != nil {
		return q.fail(id, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored the Range header, so start over.
		resumeFrom = 0
	case http.StatusPartialContent:
		// Resuming as asked.
	default:
		return q.fail(id, fmt.Errorf("server returned %s", resp.Status))
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumeFrom > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(partial, flags, 0o644)
	if err != nil {
		return q.fail(id, err)
	}

	written, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()

	q.mu.Lock()
	if cur, ok := q.items[id]; ok {
		cur.BytesDone = resumeFrom + written
		cur.OverLink = linkName
		if cur.StartedAt.IsZero() {
			cur.StartedAt = time.Now().UTC()
		}
	}
	q.mu.Unlock()

	if copyErr != nil {
		// A cut-off transfer is not a failure worth counting against the retry
		// budget: leaving coverage is expected, and the partial file means the
		// next attempt picks up where this one stopped.
		_ = q.save()
		return fmt.Errorf("syncq: transfer interrupted after %d bytes: %w", written, copyErr)
	}
	if closeErr != nil {
		return q.fail(id, closeErr)
	}

	if err := os.Rename(partial, item.Dest); err != nil {
		return q.fail(id, err)
	}

	q.mu.Lock()
	if cur, ok := q.items[id]; ok {
		cur.CompletedAt = time.Now().UTC()
		cur.LastError = ""
	}
	q.mu.Unlock()
	return q.save()
}

func (q *Queue) fail(id string, cause error) error {
	q.mu.Lock()
	if it, ok := q.items[id]; ok {
		it.Attempts++
		it.LastError = cause.Error()
	}
	q.mu.Unlock()
	_ = q.save()
	return fmt.Errorf("syncq: %s: %w", id, cause)
}

func (q *Queue) save() error {
	q.mu.Lock()
	items := make([]*Item, 0, len(q.items))
	for _, it := range q.items {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	raw, err := json.MarshalIndent(items, "", "  ")
	path := q.path
	q.mu.Unlock()

	if err != nil {
		return fmt.Errorf("syncq: encode: %w", err)
	}
	if path == "" {
		return nil
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("syncq: create %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o640); err != nil {
		return fmt.Errorf("syncq: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}
