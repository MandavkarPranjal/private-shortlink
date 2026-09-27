package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("link not found")
	ErrExists   = errors.New("link already exists")
)

type Link struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Clicks    int64     `json:"clicks"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS links (
			name       TEXT PRIMARY KEY,
			url        TEXT NOT NULL,
			owner      TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			clicks     INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_links_url ON links(url);
	`)
	if err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func scanLink(row interface{ Scan(...any) error }) (*Link, error) {
	var l Link
	var created, updated int64
	if err := row.Scan(&l.Name, &l.URL, &l.Owner, &created, &updated, &l.Clicks); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	l.CreatedAt = time.Unix(created, 0).UTC()
	l.UpdatedAt = time.Unix(updated, 0).UTC()
	return &l, nil
}

func (s *Store) Get(ctx context.Context, name string) (*Link, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, url, owner, created_at, updated_at, clicks FROM links WHERE name = ?`, name)
	return scanLink(row)
}

// Save creates or updates a link. If the link exists, the caller must have
// already checked ownership; Save itself only enforces the owner field on
// creation (existing owner is preserved on update).
func (s *Store) Save(ctx context.Context, l *Link, exists bool) error {
	now := time.Now().UTC().Unix()
	if exists {
		_, err := s.db.ExecContext(ctx,
			`UPDATE links SET url = ?, updated_at = ? WHERE name = ?`,
			l.URL, now, l.Name)
		if err != nil {
			return fmt.Errorf("update link: %w", err)
		}
		updated, err := s.Get(ctx, l.Name)
		if err != nil {
			return err
		}
		*l = *updated
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO links (name, url, owner, created_at, updated_at, clicks)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		l.Name, l.URL, l.Owner, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrExists
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return s.mustCopy(ctx, l)
}

func (s *Store) mustCopy(ctx context.Context, l *Link) error {
	got, err := s.Get(ctx, l.Name)
	if err != nil {
		return err
	}
	*l = *got
	return nil
}

func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns links matching q (case-insensitive substring on name or url),
// ordered by name. Empty q returns all links.
func (s *Store) List(ctx context.Context, q string) ([]*Link, error) {
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT name, url, owner, created_at, updated_at, clicks FROM links ORDER BY name`)
	} else {
		like := "%" + escapeLike(q) + "%"
		rows, err = s.db.QueryContext(ctx,
			`SELECT name, url, owner, created_at, updated_at, clicks FROM links
			 WHERE name LIKE ? ESCAPE '\' OR url LIKE ? ESCAPE '\'
			 ORDER BY name`, like, like)
	}
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()
	var out []*Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// IncClick increments the click counter for name. Missing links are ignored.
func (s *Store) IncClick(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE links SET clicks = clicks + 1 WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("increment clicks: %w", err)
	}
	return nil
}
