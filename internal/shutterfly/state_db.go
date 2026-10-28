package shutterfly

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// StateDB is the transfer's durable state.
type StateDB struct {
	db       *sql.DB
	path     string
	lockFile *os.File
}

const stateSchema = `
CREATE TABLE IF NOT EXISTS albums(title TEXT PRIMARY KEY, google_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS items(
  moment_id TEXT PRIMARY KEY,
  status TEXT NOT NULL CHECK(status IN ('pending','creating','done','failed')),
  media_item_id TEXT, file_name TEXT, sha256 TEXT, bytes INTEGER,
  attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT,
  batch_id TEXT, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS items_sha ON items(sha256);
CREATE INDEX IF NOT EXISTS items_status ON items(status);
CREATE TABLE IF NOT EXISTS memberships(
  moment_id TEXT NOT NULL REFERENCES items(moment_id),
  album_title TEXT NOT NULL REFERENCES albums(title),
  added INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(moment_id, album_title));
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
`

var errLocked = fmt.Errorf("lock held")

// OpenStateDB opens/creates the DB and takes the exclusive run lock.
// Dry-run/status callers use it read-only afterwards; the lock is still held.
func OpenStateDB(path string) (*StateDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("another transfer is running (lock %s)", lockPath)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		lf.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"journal_mode=WAL", "busy_timeout=5000", "foreign_keys=ON"} {
		if _, err := db.Exec("PRAGMA " + p); err != nil {
			db.Close()
			lf.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		lf.Close()
		return nil, err
	}
	os.Chmod(path, 0600)
	return &StateDB{db: db, path: path, lockFile: lf}, nil
}

func (s *StateDB) Close() {
	if s.db != nil {
		s.db.Close()
	}
	if s.lockFile != nil {
		syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
		s.lockFile.Close()
	}
}

func (s *StateDB) loadPlan(p *transferPlan, masterTitle string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	itemStmt, err := tx.Prepare(`INSERT INTO items(moment_id,status,updated_at) VALUES(?,?,?)
	  ON CONFLICT(moment_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer itemStmt.Close()
	for _, m := range p.moments {
		if _, err := itemStmt.Exec(m, "pending", now); err != nil {
			return err
		}
	}
	// ensure album rows exist for titles we'll reference
	albStmt, err := tx.Prepare(`INSERT OR IGNORE INTO albums(title,google_id) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer albStmt.Close()
	memStmt, err := tx.Prepare(`INSERT OR IGNORE INTO memberships(moment_id,album_title,added) VALUES(?,?,0)`)
	if err != nil {
		return err
	}
	defer memStmt.Close()
	titles := map[string]bool{masterTitle: true}
	for _, meta := range p.albumMeta {
		titles[meta.title] = true
	}
	for t := range titles {
		if _, err := albStmt.Exec(t, ""); err != nil {
			return err
		}
	}
	for _, m := range p.moments {
		if _, err := memStmt.Exec(m, masterTitle); err != nil {
			return err
		}
		for _, t := range p.albums[m] {
			if _, err := memStmt.Exec(m, t); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *StateDB) albumID(title string) (string, bool) {
	var id string
	err := s.db.QueryRow(`SELECT google_id FROM albums WHERE title=?`, title).Scan(&id)
	return id, err == nil && id != ""
}

func (s *StateDB) setAlbumID(title, id string) error {
	_, err := s.db.Exec(`INSERT INTO albums(title,google_id) VALUES(?,?)
	  ON CONFLICT(title) DO UPDATE SET google_id=excluded.google_id`, title, id)
	return err
}

func (s *StateDB) itemDone(momentID string) bool {
	var st string
	err := s.db.QueryRow(`SELECT status FROM items WHERE moment_id=?`, momentID).Scan(&st)
	return err == nil && st == "done"
}

// shaDone returns (momentID, mediaItemID) of a done item with this hash, if any.
func (s *StateDB) shaDone(sha string) (string, string, bool) {
	var mid, miid string
	err := s.db.QueryRow(`SELECT moment_id, media_item_id FROM items WHERE sha256=? AND status='done' LIMIT 1`, sha).Scan(&mid, &miid)
	return mid, miid, err == nil
}

func (s *StateDB) linkDup(momentID, mediaItemID, sha, note string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE items SET status='done', media_item_id=?, sha256=?, last_error=?, updated_at=? WHERE moment_id=?`,
		mediaItemID, sha, note, now, momentID); err != nil {
		return err
	}
	// inherit membership completion: any title where a moment sharing this
	// media_item_id is already added counts as added for the dup too
	if _, err := tx.Exec(`UPDATE memberships SET added=1 WHERE moment_id=? AND added=0 AND album_title IN (
	  SELECT m2.album_title FROM memberships m2 JOIN items i2 ON i2.moment_id=m2.moment_id
	  WHERE i2.media_item_id=? AND m2.added=1)`, momentID, mediaItemID); err != nil {
		return err
	}
	return tx.Commit()
}

// beginBatch marks items 'creating' in one transaction.
func (s *StateDB) beginBatch(batchID string, rs []uploadResult) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, r := range rs {
		if _, err := tx.Exec(`UPDATE items SET status='creating', batch_id=?, file_name=?, sha256=?, bytes=?, updated_at=? WHERE moment_id=?`,
			batchID, r.fileName, r.sha256, r.bytes, now, r.momentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// finishBatch commits batchCreate results in one transaction; each success is
// also marked as a member of the master album in the same tx.
func (s *StateDB) finishBatch(masterTitle string, rs []uploadResult, mediaIDs map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, r := range rs {
		if miid, ok := mediaIDs[r.token]; ok && miid != "" {
			if _, err := tx.Exec(`UPDATE items SET status='done', media_item_id=?, batch_id=NULL, updated_at=? WHERE moment_id=?`,
				miid, now, r.momentID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE memberships SET added=1 WHERE moment_id=? AND album_title=?`,
				r.momentID, masterTitle); err != nil {
				return err
			}
		} else {
			// per-item failure: back to pending, attempts+1; failed after 3
			if _, err := tx.Exec(`UPDATE items SET status=CASE WHEN attempts+1>=3 THEN 'failed' ELSE 'pending' END,
			  attempts=attempts+1, last_error='batchCreate item error', batch_id=NULL, updated_at=? WHERE moment_id=?`,
				now, r.momentID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// mediaItemIDFor looks up a done item's media item id.
func (s *StateDB) mediaItemIDFor(momentID string) (string, bool) {
	var id string
	err := s.db.QueryRow(`SELECT media_item_id FROM items WHERE moment_id=? AND status='done'`, momentID).Scan(&id)
	return id, err == nil
}

// membershipCounts returns album_title -> added member count.
func (s *StateDB) membershipCounts(addedOnly bool) (map[string]int, error) {
	q := `SELECT album_title, COUNT(*) FROM memberships`
	if addedOnly {
		q += ` WHERE added=1`
	}
	q += ` GROUP BY album_title`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// pendingMemberships returns DISTINCT media_item_ids for done items with a
// pending (added=0) membership in title, excluding ids already added under
// any moment sharing the same media_item_id (content-duplicate safety).
func (s *StateDB) pendingMemberships(title string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT i.media_item_id FROM memberships m
	  JOIN items i ON i.moment_id=m.moment_id
	  WHERE m.album_title=? AND m.added=0 AND i.status='done'
	    AND i.media_item_id NOT IN (
	      SELECT i2.media_item_id FROM memberships m2 JOIN items i2 ON i2.moment_id=m2.moment_id
	      WHERE m2.album_title=? AND m2.added=1 AND i2.media_item_id IS NOT NULL)`, title, title)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// markMemberships sets added=1 for all rows in title whose item has one of
// the given media_item_ids, in one transaction.
func (s *StateDB) markMemberships(title string, mediaItemIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, miid := range mediaItemIDs {
		if _, err := tx.Exec(`UPDATE memberships SET added=1 WHERE album_title=? AND moment_id IN (
		  SELECT moment_id FROM items WHERE media_item_id=?)`, title, miid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// membershipExists for the master album row inserted at plan load.
func (s *StateDB) markMembershipAdded(momentID, title string) error {
	_, err := s.db.Exec(`UPDATE memberships SET added=1 WHERE moment_id=? AND album_title=?`, momentID, title)
	return err
}

// creatingItems returns moment_id -> (file_name) for 'creating' items.
func (s *StateDB) creatingItems() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT moment_id, COALESCE(file_name,'') FROM items WHERE status='creating'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, fn string
		if err := rows.Scan(&id, &fn); err != nil {
			return nil, err
		}
		out[id] = fn
	}
	return out, rows.Err()
}

func (s *StateDB) assignedMediaIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT media_item_id FROM items WHERE status='done' AND media_item_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// markDone records a reconciled item. It was created by batchCreate into the
// master album, so its master membership is marked added in the same tx.
func (s *StateDB) markDone(momentID, mediaItemID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE items SET status='done', media_item_id=?, batch_id=NULL, updated_at=? WHERE moment_id=?`,
		mediaItemID, time.Now().Unix(), momentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE memberships SET added=1 WHERE moment_id=? AND album_title=?`, momentID, masterAlbumBase); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *StateDB) markPending(momentID string) error {
	_, err := s.db.Exec(`UPDATE items SET status='pending', batch_id=NULL, updated_at=? WHERE moment_id=?`,
		time.Now().Unix(), momentID)
	return err
}

// Status prints DB-only summary.
func (s *StateDB) Status() error {
	counts := map[string]int{}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM items GROUP BY status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var st string
		var n int
		rows.Scan(&st, &n)
		counts[st] = n
	}
	rows.Close()
	fmt.Println("Transfer status:")
	for _, st := range []string{"pending", "creating", "done", "failed"} {
		fmt.Printf("  %-9s %d\n", st+":", counts[st])
	}
	var bytes int64
	s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM items WHERE status='done'`).Scan(&bytes)
	fmt.Printf("  bytes uploaded: %.1f MB\n", float64(bytes)/1e6)

	fmt.Println("\nAlbums:")
	arows, err := s.db.Query(`SELECT album_title, SUM(added=0) as pending, SUM(added=1) as added, COUNT(*) FROM memberships GROUP BY album_title ORDER BY album_title`)
	if err == nil {
		defer arows.Close()
		fmt.Printf("  %-50s %8s %8s %8s\n", "title", "planned", "added", "pending")
		for arows.Next() {
			var t string
			var pend, added, total int
			arows.Scan(&t, &pend, &added, &total)
			fmt.Printf("  %-50s %8d %8d %8d\n", t, total, added, pend)
		}
	}

	frows, err := s.db.Query(`SELECT moment_id, COALESCE(last_error,'') FROM items WHERE status='failed' ORDER BY updated_at LIMIT 20`)
	if err == nil {
		defer frows.Close()
		var out []string
		for frows.Next() {
			var id, e string
			frows.Scan(&id, &e)
			out = append(out, id+" ("+e+")")
		}
		if len(out) > 0 {
			fmt.Println("\nFailed:")
			for _, l := range out {
				fmt.Println("  " + l)
			}
		}
	}
	if counts["creating"] > 0 {
		fmt.Printf("\n%d item(s) in 'creating' — run -reconcile\n", counts["creating"])
	}
	return nil
}

// failedIDs for the google-failed.txt report.
func (s *StateDB) failedIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT moment_id FROM items WHERE status='failed' ORDER BY moment_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, rows.Err()
}
