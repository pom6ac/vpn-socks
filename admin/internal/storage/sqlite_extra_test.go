package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNew_BadPath(t *testing.T) {
	// A path inside a regular file can never be opened as a database.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(f, "sub", "admin.db")); err == nil {
		t.Fatal("expected error for unopenable path")
	}
}

func TestNew_NotADatabase(t *testing.T) {
	f := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(f, []byte("this is definitely not an sqlite database file, padded to be long enough....."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f); err == nil {
		t.Fatal("expected error for non-sqlite file")
	}
}

func TestNew_WALMode(t *testing.T) {
	s := newStore(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := newStore(t)
	const workers, per = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, workers*per*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := s.WriteMetric("rx_bytes", float64(i)); err != nil {
					errs <- err
				}
				if err := s.WriteContainerEvent(fmt.Sprintf("c%d", w), "start"); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != workers*per {
		t.Errorf("metrics rows = %d, want %d", n, workers*per)
	}
}

func insertAt(t *testing.T, s *Store, ts time.Time, kind string, v float64) {
	t.Helper()
	_, err := s.db.Exec("INSERT INTO metrics (ts, kind, value) VALUES (?, ?, ?)",
		ts.UTC().Format("2006-01-02 15:04:05"), kind, v)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHistory_AggregatesToMaxPoints(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	// One row every 10s over 24h = 8640 rows.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8640; i++ {
		ts := now.Add(-time.Duration(i) * 10 * time.Second)
		if _, err := tx.Exec("INSERT INTO metrics (ts, kind, value) VALUES (?, 'rx_bytes', ?)",
			ts.Format("2006-01-02 15:04:05"), float64(i%10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	points, err := s.History(context.Background(), "rx_bytes", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 || len(points) > maxHistoryPoints {
		t.Fatalf("points = %d, want 1..%d", len(points), maxHistoryPoints)
	}
	if len(points) < maxHistoryPoints-5 {
		t.Errorf("points = %d, expected close to %d", len(points), maxHistoryPoints)
	}
	for i := 1; i < len(points); i++ {
		if points[i].TS <= points[i-1].TS {
			t.Fatalf("points not ascending at %d: %s <= %s", i, points[i].TS, points[i-1].TS)
		}
	}
}

func TestHistory_AveragesWithinBucket(t *testing.T) {
	s := newStore(t)
	// 1h range -> 8s buckets (3600/499 rounded up); align to a boundary.
	base := time.Unix((time.Now().Add(-30*time.Minute).Unix()/8)*8, 0)
	insertAt(t, s, base, "k", 10)
	insertAt(t, s, base.Add(2*time.Second), "k", 20)
	insertAt(t, s, base.Add(time.Minute), "k", 5)
	insertAt(t, s, base, "other", 99)

	points, err := s.History(context.Background(), "k", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("points = %+v, want 2", points)
	}
	if points[0].Value != 15 || points[1].Value != 5 {
		t.Errorf("values = %v, %v; want 15, 5", points[0].Value, points[1].Value)
	}
	if want := base.UTC().Format("2006-01-02 15:04:05"); points[0].TS != want {
		t.Errorf("ts = %q, want %q", points[0].TS, want)
	}
}

func TestHistory_EmptyIsNotNil(t *testing.T) {
	s := newStore(t)
	points, err := s.History(context.Background(), "none", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if points == nil {
		t.Error("want empty slice (JSON []), got nil")
	}
}

func TestPruneLoop_Ticks(t *testing.T) {
	s := newStore(t)
	insertAt(t, s, time.Now().Add(-8*24*time.Hour), "old", 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.PruneLoop(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old metric not pruned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestPruneLoop_LogsErrors(t *testing.T) {
	s := newStore(t)
	_ = s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	s.PruneLoop(ctx, 5*time.Millisecond) // must not panic on a closed db
}
