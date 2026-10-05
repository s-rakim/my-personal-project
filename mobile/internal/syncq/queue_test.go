package syncq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(Options{Path: filepath.Join(t.TempDir(), "q.json"), MaxAttempts: 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return q
}

func TestTransferDownloadsAndCompletes(t *testing.T) {
	body := strings.Repeat("payload", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	q := newQueue(t)
	dest := filepath.Join(t.TempDir(), "out", "file.bin")
	if err := q.Add(Item{ID: "a", URL: srv.URL, Dest: dest, Class: "bulk"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := q.Transfer(context.Background(), "a", "corridor", srv.Client()); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != body {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(body))
	}

	items := q.All()
	if len(items) != 1 || !items[0].Done() {
		t.Fatalf("item should be complete: %+v", items)
	}
	if items[0].OverLink != "corridor" {
		t.Errorf("should record which link carried it, got %q", items[0].OverLink)
	}
	// The partial file must not be left behind.
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("partial file was not cleaned up")
	}
}

// Leaving coverage mid-transfer is the normal case on a vehicle, so a resumed
// transfer must pick up rather than start over.
func TestTransferResumesFromPartial(t *testing.T) {
	full := strings.Repeat("x", 5000)
	var sawRange string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		if sawRange != "" {
			// Serve only the tail, as a real server would.
			w.Header().Set("Content-Range", "bytes 2000-4999/5000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(full[2000:]))
			return
		}
		_, _ = w.Write([]byte(full))
	}))
	defer srv.Close()

	q := newQueue(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")

	// Simulate an interrupted transfer by planting a partial file.
	if err := os.WriteFile(dest+".part", []byte(full[:2000]), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	if err := q.Add(Item{ID: "a", URL: srv.URL, Dest: dest, Class: "bulk"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Transfer(context.Background(), "a", "wifi", srv.Client()); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	if sawRange != "bytes=2000-" {
		t.Errorf("should have requested a resume, sent Range %q", sawRange)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != full {
		t.Errorf("resumed file is wrong: got %d bytes, want %d", len(got), len(full))
	}
}

func TestFailuresAreBoundedByMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	q := newQueue(t) // MaxAttempts 3
	dest := filepath.Join(t.TempDir(), "file.bin")
	_ = q.Add(Item{ID: "a", URL: srv.URL, Dest: dest, Class: "bulk"})

	for i := 0; i < 3; i++ {
		if err := q.Transfer(context.Background(), "a", "cell", srv.Client()); err == nil {
			t.Fatal("expected failure")
		}
	}

	// A permanently broken URL must stop being retried, or every link that comes
	// up pays to attempt it again.
	if pending := q.Pending("bulk"); len(pending) != 0 {
		t.Errorf("exhausted item is still pending: %+v", pending)
	}
	if s := q.Stats(); s.Failed != 1 || s.Pending != 0 {
		t.Errorf("stats wrong: %+v", s)
	}
}

func TestPendingIsFilteredByClassAndOrderedOldestFirst(t *testing.T) {
	q := newQueue(t)
	_ = q.Add(Item{ID: "first", URL: "http://x/1", Dest: "/tmp/1", Class: "bulk"})
	_ = q.Add(Item{ID: "second", URL: "http://x/2", Dest: "/tmp/2", Class: "bulk"})
	_ = q.Add(Item{ID: "idle", URL: "http://x/3", Dest: "/tmp/3", Class: "idle"})

	bulk := q.Pending("bulk")
	if len(bulk) != 2 {
		t.Fatalf("expected 2 bulk items, got %d", len(bulk))
	}
	if bulk[0].ID != "first" {
		t.Errorf("oldest should come first, got %q", bulk[0].ID)
	}
	if idle := q.Pending("idle"); len(idle) != 1 {
		t.Errorf("expected 1 idle item, got %d", len(idle))
	}
}

func TestAddIsIdempotent(t *testing.T) {
	q := newQueue(t)
	item := Item{ID: "a", URL: "http://x/1", Dest: "/tmp/1", Class: "bulk"}
	_ = q.Add(item)
	_ = q.Add(item)

	// A cron job enqueuing its work hourly must not pile up duplicates.
	if got := len(q.All()); got != 1 {
		t.Errorf("duplicate add created %d items", got)
	}
}

func TestQueueSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.json")

	first, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Add(Item{ID: "a", URL: "http://x/1", Dest: "/tmp/1",
		Class: "bulk", SizeBytes: 1234}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	second, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	items := second.All()
	if len(items) != 1 || items[0].ID != "a" || items[0].SizeBytes != 1234 {
		t.Fatalf("queue did not survive restart: %+v", items)
	}
}
