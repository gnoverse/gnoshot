package shot

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the manifest plus the bytes: a SQLite index of what is known about
// each URL, and a content-addressed directory of the images themselves.
//
// SQLite rather than a directory of JSON files because the manifest is a
// single-writer index with range queries over probed_at, which is a
// file-plus-fsync problem otherwise.
type Store struct {
	root string
	db   *sql.DB
}

// OpenStore opens or creates a store rooted at dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "manifest.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One writer. The workers serialise through this handle, and a second
	// connection would only buy SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{root: dir, db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS entries (
  key             TEXT PRIMARY KEY,
  url             TEXT NOT NULL,
  theme           TEXT NOT NULL,
  matched         TEXT NOT NULL,
  selector        TEXT NOT NULL DEFAULT '',
  box_w           INTEGER NOT NULL DEFAULT 0,
  box_h           INTEGER NOT NULL DEFAULT 0,
  truncated       INTEGER NOT NULL DEFAULT 0,
  upstream_status INTEGER NOT NULL DEFAULT 0,
  state           TEXT NOT NULL DEFAULT '',
  freshness       TEXT NOT NULL DEFAULT '',
  captured_at     INTEGER NOT NULL DEFAULT 0,
  probed_at       INTEGER NOT NULL DEFAULT 0,
  object          TEXT NOT NULL DEFAULT '',
  rungs           TEXT NOT NULL DEFAULT '{}',
  modes           TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS entries_probed ON entries(probed_at);
`

// Key identifies one manifest row. The theme is part of it because the same URL
// captured light and dark is two different pictures.
func Key(url string, theme Theme) string { return url + "|" + string(theme) }

// ObjectKey is the content address of one capture generation.
//
// Derived from the freshness hash, so a re-capture that finds the page
// unchanged reuses the same directory and the URLs a page is already holding
// stay valid.
func ObjectKey(url string, theme Theme, freshness string) string {
	sum := sha256.Sum256([]byte(url + "|" + string(theme) + "|" + freshness))
	return hex.EncodeToString(sum[:16])
}

func (s *Store) objectDir(obj string) string {
	return filepath.Join(s.root, "objects", obj[:2], obj)
}

// PutFile writes one file of an object.
func (s *Store) PutFile(obj, name string, b []byte) error {
	dir := s.objectDir(obj)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// ReadFile reads one file of an object.
func (s *Store) ReadFile(obj, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.objectDir(obj), name))
}

// Get returns the manifest row for a URL, or nil when nothing is known.
func (s *Store) Get(url string, theme Theme) (*Entry, error) {
	row := s.db.QueryRow(`SELECT url, theme, matched, selector, box_w, box_h, truncated,
		upstream_status, state, freshness, captured_at, probed_at, object, rungs, modes
		FROM entries WHERE key = ?`, Key(url, theme))
	var (
		e            Entry
		trunc        int
		capAt, prbAt int64
		rungs, modes string
	)
	err := row.Scan(&e.URL, &e.Theme, &e.Matched, &e.Selector, &e.Box.W, &e.Box.H, &trunc,
		&e.UpstreamStatus, &e.State, &e.Freshness, &capAt, &prbAt, &e.Object, &rungs, &modes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Truncated = trunc != 0
	if capAt > 0 {
		e.CapturedAt = time.Unix(capAt, 0).UTC()
	}
	if prbAt > 0 {
		e.ProbedAt = time.Unix(prbAt, 0).UTC()
	}
	_ = json.Unmarshal([]byte(rungs), &e.Rungs)
	_ = json.Unmarshal([]byte(modes), &e.Modes)
	return &e, nil
}

// Put upserts a manifest row.
func (s *Store) Put(e *Entry) error {
	rungs, _ := json.Marshal(e.Rungs)
	modes, _ := json.Marshal(e.Modes)
	trunc := 0
	if e.Truncated {
		trunc = 1
	}
	var capAt, prbAt int64
	if !e.CapturedAt.IsZero() {
		capAt = e.CapturedAt.Unix()
	}
	if !e.ProbedAt.IsZero() {
		prbAt = e.ProbedAt.Unix()
	}
	_, err := s.db.Exec(`INSERT INTO entries
		(key, url, theme, matched, selector, box_w, box_h, truncated, upstream_status,
		 state, freshness, captured_at, probed_at, object, rungs, modes)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
		  matched=excluded.matched, selector=excluded.selector, box_w=excluded.box_w,
		  box_h=excluded.box_h, truncated=excluded.truncated,
		  upstream_status=excluded.upstream_status, state=excluded.state,
		  freshness=excluded.freshness, captured_at=excluded.captured_at,
		  probed_at=excluded.probed_at, object=excluded.object, rungs=excluded.rungs,
		  modes=excluded.modes`,
		Key(e.URL, e.Theme), e.URL, string(e.Theme), string(e.Matched), e.Selector,
		e.Box.W, e.Box.H, trunc, e.UpstreamStatus, string(e.State), e.Freshness,
		capAt, prbAt, e.Object, string(rungs), string(modes))
	return err
}

// Touch records that a probe found the page unchanged, without rewriting the
// capture. This is what keeps a sweep cheap: 45 ms and one UPDATE for a realm
// nobody has called.
func (s *Store) Touch(url string, theme Theme, at time.Time) error {
	_, err := s.db.Exec(`UPDATE entries SET probed_at = ? WHERE key = ?`, at.Unix(), Key(url, theme))
	return err
}

// Stale lists the entries whose probe is older than age, oldest first.
func (s *Store) Stale(age time.Duration, limit int) ([]string, error) {
	cutoff := time.Now().Add(-age).Unix()
	rows, err := s.db.Query(`SELECT url FROM entries WHERE probed_at < ? ORDER BY probed_at LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Stats is what /healthz reports about the store.
type Stats struct {
	Entries  int       `json:"entries"`
	Captured int       `json:"captured"`
	Oldest   time.Time `json:"oldest_probe,omitempty"`
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	var oldest sql.NullInt64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(object <> ''),0), MIN(NULLIF(probed_at,0)) FROM entries`).
		Scan(&st.Entries, &st.Captured, &oldest)
	if err != nil {
		return st, err
	}
	if oldest.Valid {
		st.Oldest = time.Unix(oldest.Int64, 0).UTC()
	}
	return st, nil
}
