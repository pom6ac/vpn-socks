package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNew_Migrate(t *testing.T) {
	s := newTestStore(t)
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestWriteMetric(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteMetric("rx_bytes", 1234.5); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}
	if err := s.WriteMetric("tx_bytes", 6789.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}
}

func TestWriteContainerEvent(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteContainerEvent("vpn-ovpn", "restart"); err != nil {
		t.Fatalf("WriteContainerEvent: %v", err)
	}
	if err := s.WriteContainerEvent("vpn-socks", "die"); err != nil {
		t.Fatalf("WriteContainerEvent: %v", err)
	}
}

func TestHistory_ReturnsDataAfterWrite_Q6(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteMetric("rx_bytes", 100.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}
	if err := s.WriteMetric("rx_bytes", 200.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}
	if err := s.WriteMetric("tx_bytes", 300.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	since := time.Now().Add(-1 * time.Hour)
	points, err := s.History(context.Background(), "rx_bytes", since)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	// Both writes normally land in the same time bucket and are averaged;
	// they may straddle a bucket boundary.
	if len(points) == 1 && points[0].Value != 150 {
		t.Fatalf("expected averaged value 150, got %+v", points)
	}
	if len(points) == 0 || len(points) > 2 {
		t.Fatalf("expected 1-2 points, got %d (Q6 fix: datetime(?) should return data)", len(points))
	}

	for _, p := range points {
		if p.TS == "" {
			t.Error("expected non-empty timestamp")
		}
	}
}

func TestHistory_FutureSince_Empty(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteMetric("rx_bytes", 100.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}

	since := time.Now().Add(1 * time.Hour)
	points, err := s.History(context.Background(), "rx_bytes", since)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	if len(points) != 0 {
		t.Errorf("expected 0 points for future since, got %d", len(points))
	}
}

func TestHistory_DifferentKind_Empty(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteMetric("rx_bytes", 100.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}

	since := time.Now().Add(-1 * time.Hour)
	points, err := s.History(context.Background(), "nonexistent_kind", since)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	if len(points) != 0 {
		t.Errorf("expected 0 points for different kind, got %d", len(points))
	}
}

func TestHistory_CancelledContext(t *testing.T) {
	s := newTestStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	since := time.Now().Add(-1 * time.Hour)
	_, err := s.History(ctx, "rx_bytes", since)
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}

func TestPrune(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteMetric("rx_bytes", 100.0); err != nil {
		t.Fatalf("WriteMetric: %v", err)
	}

	oldTime := time.Now().Add(-8 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	_, err := s.db.Exec("INSERT INTO metrics (ts, kind, value) VALUES (?, ?, ?)", oldTime, "rx_bytes", 999.0)
	if err != nil {
		t.Fatalf("insert old metric: %v", err)
	}

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	since := time.Now().Add(-30 * 24 * time.Hour)
	points, err := s.History(context.Background(), "rx_bytes", since)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	for _, p := range points {
		if p.Value == 999.0 {
			t.Error("old metric was not pruned")
		}
	}

	if len(points) == 0 {
		t.Error("recent metric was pruned (should be kept)")
	}
}

func TestPrune_Events(t *testing.T) {
	s := newTestStore(t)

	if err := s.WriteContainerEvent("vpn-ovpn", "start"); err != nil {
		t.Fatalf("WriteContainerEvent: %v", err)
	}

	oldTime := time.Now().Add(-8 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	_, err := s.db.Exec("INSERT INTO container_events (ts, container, event) VALUES (?, ?, ?)", oldTime, "vpn-ovpn", "die")
	if err != nil {
		t.Fatalf("insert old event: %v", err)
	}

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var count int
	err = s.db.QueryRow("SELECT count(*) FROM container_events").Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 event after prune, got %d", count)
	}
}

func TestClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPruneLoop_CancelledContext(t *testing.T) {
	s := newTestStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.PruneLoop(ctx, time.Hour)
}
