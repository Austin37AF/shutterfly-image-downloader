package shutterfly

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FixDates re-uploads photos that landed on their upload date in Google
// Photos (malformed or missing EXIF dates). Old items are gathered in an
// app-created "delete these" album for manual removal.
const deleteAlbumTitle = "Shutterfly - wrong dates (delete these)"

const fixDatesSchema = `
CREATE TABLE IF NOT EXISTS date_fixes(
  moment_id TEXT PRIMARY KEY,
  old_media_item_id TEXT,
  new_media_item_id TEXT,
  fixed_date TEXT,
  source TEXT,
  gap_seconds INTEGER,
  in_delete_album INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL CHECK(status IN ('pending','creating','done')));
`

type listedItem struct {
	ID           string
	FileName     string
	CreationTime time.Time
}

// listAllMediaItems pages all app-created items with creationTime.
func (g *GoogleClient) listAllMediaItems() (map[string]listedItem, error) {
	out := map[string]listedItem{}
	page := ""
	for {
		items, next, err := g.listMediaItemsTimed(page)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			out[it.ID] = it
		}
		if next == "" {
			break
		}
		page = next
	}
	return out, nil
}

// listMediaItemsTimed is like listMediaItems but returns creationTime too.
func (g *GoogleClient) listMediaItemsTimed(pageToken string) ([]listedItem, string, error) {
	path := "/v1/mediaItems?pageSize=100"
	if pageToken != "" {
		path += "&pageToken=" + url.QueryEscape(pageToken)
	}
	resp, err := g.do("GET", path, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var result struct {
		MediaItems []struct {
			ID            string `json:"id"`
			FileName      string `json:"filename"`
			MediaMetadata struct {
				CreationTime string `json:"creationTime"`
			} `json:"mediaMetadata"`
		} `json:"mediaItems"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", err
	}
	items := make([]listedItem, len(result.MediaItems))
	for i, m := range result.MediaItems {
		items[i] = listedItem{ID: m.ID, FileName: m.FileName}
		if m.MediaMetadata.CreationTime != "" {
			items[i].CreationTime, _ = time.Parse(time.RFC3339, m.MediaMetadata.CreationTime)
		}
	}
	return items, result.NextPageToken, nil
}

type badItem struct {
	momentID     string
	mediaItemID  string
	fileName     string
	creationTime time.Time
	neighborDate time.Time
	gapSeconds   int64
	proposedDate time.Time
	source       string
}

// findBadItems joins app-created listing to done DB rows and returns the bad
// ones (creationTime within 48h of our upload record), plus momentID ->
// creationTime for good items (for neighbor dates).
func findBadItems(st *StateDB, listed map[string]listedItem) ([]badItem, map[string]time.Time, int, error) {
	// media_item_id -> (moment_id, updated_at)
	rows, err := st.db.Query(`SELECT media_item_id, moment_id, updated_at, file_name FROM items WHERE status='done' AND media_item_id IS NOT NULL`)
	if err != nil {
		return nil, nil, 0, err
	}
	type rec struct {
		moment string
		at     time.Time
		fname  string
	}
	doneMap := map[string]rec{}
	for rows.Next() {
		var miid, mid, fname string
		var at int64
		rows.Scan(&miid, &mid, &at, &fname)
		doneMap[miid] = rec{mid, time.Unix(at, 0), fname}
	}
	rows.Close()

	var bad []badItem
	missing := 0
	doneIDs := map[string]bool{}
	goodDates := map[string]time.Time{}
	badMoments := map[string]bool{}
	for _, it := range listed {
		r, ok := doneMap[it.ID]
		if !ok {
			continue // not ours
		}
		doneIDs[it.ID] = true
		diff := it.CreationTime.Sub(r.at)
		if diff < 0 {
			diff = -diff
		}
		if diff < 48*time.Hour {
			fname := it.FileName
			if fname == "" {
				fname = r.fname
			}
			bad = append(bad, badItem{momentID: r.moment, mediaItemID: it.ID, fileName: fname, creationTime: it.CreationTime})
			badMoments[r.moment] = true
		} else {
			goodDates[r.moment] = it.CreationTime
		}
	}
	for miid := range doneMap {
		if !doneIDs[miid] {
			missing++ // in DB but absent from listing: skip, count only
		}
	}
	return bad, goodDates, missing, nil
}

// assignNeighborDates walks the searchMoments order (newest first) and picks
// neighbor dates for each bad item.
func assignNeighborDates(c *Client, st *StateDB, bad []badItem, goodDates map[string]time.Time) error {
	libraryIDs, err := c.GetAllMomentIDs()
	if err != nil {
		return err
	}
	order := make([]string, len(libraryIDs))
	pos := map[string]int{}
	for i, id := range libraryIDs {
		n := normalizeID(id)
		order[i] = n
		pos[n] = i
	}
	badSet := map[string]bool{}
	for i := range bad {
		badSet[bad[i].momentID] = true
	}
	for i := range bad {
		b := &bad[i]
		idx, ok := pos[b.momentID]
		if !ok {
			continue
		}
		var left, right time.Time
		for j := idx - 1; j >= 0; j-- {
			if badSet[order[j]] {
				continue
			}
			if t, ok := goodDates[order[j]]; ok {
				left = t
				break
			}
		}
		for j := idx + 1; j < len(order); j++ {
			if badSet[order[j]] {
				continue
			}
			if t, ok := goodDates[order[j]]; ok {
				right = t
				break
			}
		}
		switch {
		case !left.IsZero() && !right.IsZero():
			if left.Before(right) { // inverted: prefer older side
				b.neighborDate = left
				b.gapSeconds = int64(right.Sub(left).Seconds())
			} else {
				b.gapSeconds = int64(left.Sub(right).Seconds())
				b.neighborDate = right.Add(time.Duration(b.gapSeconds/2) * time.Second)
			}
		case !left.IsZero():
			b.neighborDate = left
		case !right.IsZero():
			b.neighborDate = right
		}
	}
	return nil
}

// FixDates runs the repair mode. dryRun only lists/downloads/repairs and
// writes the TSV.
func (c *Client) FixDates(g *GoogleClient, statePath string, workers int, dryRun bool, expectedEmail string, yes bool) error {
	st, err := OpenStateDB(statePath)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.db.Exec(fixDatesSchema); err != nil {
		return err
	}

	if g == nil {
		return fmt.Errorf("no Google client")
	}
	if expectedEmail != "" && !strings.EqualFold(g.Email, expectedEmail) {
		return fmt.Errorf("signed in as %s, expected %s", g.Email, expectedEmail)
	}
	if stored := st.metaGet("google_account"); stored != "" && !strings.EqualFold(g.Email, stored) {
		return fmt.Errorf("signed in as %s, but this transfer belongs to %s", g.Email, stored)
	}

	fmt.Println("Listing app-created media items (this takes a few minutes)...")
	listed, err := g.listAllMediaItems()
	if err != nil {
		return err
	}
	bad, goodDates, missing, err := findBadItems(st, listed)
	if err != nil {
		return err
	}
	fmt.Printf("%d bad items, %d done items absent from listing (skipped)\n", len(bad), missing)
	if len(bad) == 0 {
		return nil
	}
	if err := assignNeighborDates(c, st, bad, goodDates); err != nil {
		return err
	}

	if dryRun {
		// download + repair in memory, write TSV
		tsvPath := filepath.Join(filepath.Dir(statePath), "date-fixes.tsv")
		f, err := os.Create(tsvPath)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintln(f, "moment_id\tfile_name\told_media_item_id\tproposed_date\tsource\tgap_days")
		var mu sync.Mutex
		srcCounts := map[string]int{}
		jobs := make(chan badItem)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for b := range jobs {
					data, _, fname, err := c.downloadMomentBytes(b.momentID)
					if err != nil {
						fmt.Printf("download failed %s: %v\n", b.momentID, err)
						continue
					}
					if fname != "" {
						b.fileName = fname
					}
					_, dt, src := repairDateBytes(data, b.fileName, b.neighborDate)
					if dt.IsZero() && !b.neighborDate.IsZero() {
						dt, src = b.neighborDate, "neighbor"
					}
					mu.Lock()
					srcCounts[src]++
					gapDays := b.gapSeconds / 86400
					fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\t%d\n", b.momentID, b.fileName, b.mediaItemID, dt.Format("2006-01-02 15:04:05"), src, gapDays)
					mu.Unlock()
					time.Sleep(c.RateLimit)
				}
			}()
		}
		for _, b := range bad {
			jobs <- b
		}
		close(jobs)
		wg.Wait()
		bigGap := 0
		for _, b := range bad {
			if b.gapSeconds > 30*86400 {
				bigGap++
			}
		}
		fmt.Printf("\nDry run: %d bad items\n", len(bad))
		for src, n := range srcCounts {
			fmt.Printf("  %-10s %d\n", src+":", n)
		}
		fmt.Printf("  neighbour fixes with gap > 30 days: %d\n  TSV: %s\n", bigGap, tsvPath)
		return nil
	}

	// ---- real run ----
	// confirm
	if st.metaGet("google_account") == "" {
		confirmed := yes && expectedEmail != ""
		if !confirmed {
			fmt.Printf("Signed in to Google as %s. Fix %d photos in this account? [y/N] ", g.Email, len(bad))
			var ans string
			fmt.Fscan(c.Stdin, &ans)
			confirmed = strings.EqualFold(strings.TrimSpace(ans), "y")
		}
		if !confirmed {
			return fmt.Errorf("aborted")
		}
		st.metaSet("google_account", g.Email)
	}

	// ensure master + delete album
	if err := ensureAlbums(g, st, &transferPlan{}); err != nil {
		return err
	}
	if _, ok := st.albumID(deleteAlbumTitle); !ok {
		id, err := g.createAlbum(deleteAlbumTitle)
		if err != nil {
			return err
		}
		st.setAlbumID(deleteAlbumTitle, id)
	}
	masterTitle, err := currentMasterTitle(st)
	if err != nil {
		return err
	}
	masterID, _ := st.albumID(masterTitle)

	// skip already-done fixes
	doneFix := map[string]bool{}
	rows, _ := st.db.Query(`SELECT moment_id FROM date_fixes WHERE status='done'`)
	if rows != nil {
		for rows.Next() {
			var m string
			rows.Scan(&m)
			doneFix[m] = true
		}
		rows.Close()
	}

	var doneN, failN int64
	var stopFlag atomic.Bool
	type fixResult struct {
		badItem
		token string
		err   error
	}
	results := make(chan fixResult)
	var creatorWG sync.WaitGroup
	creatorWG.Add(1)
	var creatorErr error
	go func() {
		defer creatorWG.Done()
		for r := range results {
			if r.err != nil {
				atomic.AddInt64(&failN, 1)
				continue
			}
			// creating row first (crash-safety)
			if _, err := st.db.Exec(`INSERT INTO date_fixes(moment_id,old_media_item_id,fixed_date,source,gap_seconds,status)
			  VALUES(?,?,?,?,?,'creating') ON CONFLICT(moment_id) DO UPDATE SET status='creating'`,
				r.momentID, r.mediaItemID, r.proposedDate.Format(time.RFC3339), r.source, r.gapSeconds); err != nil {
				creatorErr = err
				return
			}
			created, err := g.batchCreate(masterID, []struct{ UploadToken, FileName string }{{r.token, r.fileName}})
			if err != nil || len(created) != 1 || !created[0].OK {
				if err == ErrQuota {
					creatorErr = err
					return
				}
				atomic.AddInt64(&failN, 1)
				st.db.Exec(`UPDATE date_fixes SET status='pending' WHERE moment_id=?`, r.momentID)
				continue
			}
			newID := created[0].MediaItemID
			tx, err := st.db.Begin()
			if err != nil {
				creatorErr = err
				return
			}
			tx.Exec(`UPDATE date_fixes SET new_media_item_id=?, status='done' WHERE moment_id=?`, newID, r.momentID)
			tx.Exec(`UPDATE items SET media_item_id=?, status='done', updated_at=? WHERE moment_id=?`, newID, time.Now().Unix(), r.momentID)
			tx.Exec(`UPDATE memberships SET added=0 WHERE moment_id=? AND album_title!=?`, r.momentID, masterTitle)
			tx.Exec(`UPDATE memberships SET added=1 WHERE moment_id=? AND album_title=?`, r.momentID, masterTitle)
			if err := tx.Commit(); err != nil {
				creatorErr = err
				return
			}
			atomic.AddInt64(&doneN, 1)
			if doneN%100 == 0 {
				fmt.Printf("Progress: %d/%d fixed, %d failed\n", doneN, len(bad), failN)
			}
		}
	}()

	jobs := make(chan badItem)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				if stopFlag.Load() || doneFix[b.momentID] {
					continue
				}
				data, mime, fname, err := c.downloadMomentBytes(b.momentID)
				if err != nil {
					results <- fixResult{badItem: b, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				if fname != "" {
					b.fileName = fname
				}
				newData, dt, src := repairDateBytes(data, b.fileName, b.neighborDate)
				if dt.IsZero() {
					results <- fixResult{badItem: b, err: fmt.Errorf("no date could be determined")}
					time.Sleep(c.RateLimit)
					continue
				}
				b.proposedDate, b.source = dt, src
				tok, err := g.upload(newData, mime, fname)
				if err == ErrQuota {
					stopFlag.Store(true)
					results <- fixResult{badItem: b, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				if err != nil {
					results <- fixResult{badItem: b, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				results <- fixResult{badItem: b, token: tok}
				time.Sleep(c.RateLimit)
			}
		}()
	}
	for _, b := range bad {
		jobs <- b
	}
	close(jobs)
	wg.Wait()
	close(results)
	creatorWG.Wait()
	if creatorErr != nil {
		return creatorErr
	}
	if stopFlag.Load() {
		fmt.Println("\nGoogle Photos quota reached (429). Progress saved — re-run tomorrow.")
		return ErrQuota
	}

	// memberships for the new items
	if err := addMemberships(g, st); err != nil {
		return err
	}

	// gather old ids into the delete album
	delID, _ := st.albumID(deleteAlbumTitle)
	var oldIDs []string
	rows2, _ := st.db.Query(`SELECT old_media_item_id FROM date_fixes WHERE status='done' AND in_delete_album=0`)
	if rows2 != nil {
		for rows2.Next() {
			var id string
			rows2.Scan(&id)
			oldIDs = append(oldIDs, id)
		}
		rows2.Close()
	}
	for i := 0; i < len(oldIDs); i += 50 {
		end := i + 50
		if end > len(oldIDs) {
			end = len(oldIDs)
		}
		if err := g.batchAddMediaItems(delID, oldIDs[i:end]); err != nil {
			return err
		}
		tx, _ := st.db.Begin()
		for _, id := range oldIDs[i:end] {
			tx.Exec(`UPDATE date_fixes SET in_delete_album=1 WHERE old_media_item_id=?`, id)
		}
		tx.Commit()
	}

	fmt.Printf("\nFix complete: %d fixed, %d failed\n", doneN, failN)
	fmt.Println("Old items are in album '" + deleteAlbumTitle + "' — open it in Google Photos, select all, and delete (Trash keeps them 60 days).")
	return nil
}
