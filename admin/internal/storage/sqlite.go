package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// maxHistoryPoints caps the number of points History returns; older data is
// averaged into time buckets so the chart stays light for any range.
const maxHistoryPoints = 500

// Store wraps the SQLite database for metrics and container events.
type Store struct {
	db *sql.DB
}

// New opens the SQLite database and creates tables if missing. The database
// runs in WAL mode with a busy timeout, and a single connection serialises
// writers coming from several goroutines.
func New(dbPath string) (*Store, error) {
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS metrics (
			id    INTEGER PRIMARY KEY,
			ts    DATETIME DEFAULT CURRENT_TIMESTAMP,
			kind  TEXT,
			value REAL
		);
		CREATE TABLE IF NOT EXISTS container_events (
			id        INTEGER PRIMARY KEY,
			ts        DATETIME DEFAULT CURRENT_TIMESTAMP,
			container TEXT,
			event     TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_metrics_kind_ts ON metrics(kind, ts);
		CREATE INDEX IF NOT EXISTS idx_events_ts ON container_events(ts);
	`)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// WriteMetric inserts a single metric row.
func (s *Store) WriteMetric(kind string, value float64) error {
	_, err := s.db.Exec("INSERT INTO metrics (kind, value) VALUES (?, ?)", kind, value)
	return err
}

// WriteContainerEvent inserts a container event row.
func (s *Store) WriteContainerEvent(container, event string) error {
	_, err := s.db.Exec("INSERT INTO container_events (container, event) VALUES (?, ?)", container, event)
	return err
}

// HistoryPoint is a single time-series data point.
type HistoryPoint struct {
	TS    string  `json:"ts"`
	Value float64 `json:"value"`
}

// History returns time-series data for a metric kind since the given time.
// Rows are averaged into equal time buckets so that at most maxHistoryPoints
// points are returned; each point is stamped with its bucket start (UTC).
func (s *Store) History(ctx context.Context, kind string, since time.Time) ([]HistoryPoint, error) {
	// Buckets are aligned to the epoch, so a range of span seconds touches
	// at most span/bucket+1 of them: divide by maxHistoryPoints-1.
	span := int64(time.Since(since) / time.Second)
	bucket := (span + maxHistoryPoints - 2) / (maxHistoryPoints - 1)
	if bucket < 1 {
		bucket = 1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT strftime('%Y-%m-%d %H:%M:%S', (CAST(strftime('%s', ts) AS INTEGER) / ?1) * ?1, 'unixepoch') AS b,
		       AVG(value)
		FROM metrics
		WHERE kind = ?2 AND ts >= datetime(?3)
		GROUP BY b
		ORDER BY b ASC`,
		bucket, kind, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	points := []HistoryPoint{}
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// Prune deletes metrics and events older than 7 days.
func (s *Store) Prune() error {
	if _, err := s.db.Exec("DELETE FROM metrics WHERE ts < datetime('now', '-7 days')"); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM container_events WHERE ts < datetime('now', '-7 days')")
	return err
}

// PruneLoop runs pruning once per interval until ctx is cancelled.
func (s *Store) PruneLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Prune(); err != nil {
				log.Printf("prune failed: %v", err)
			}
		}
	}
}
