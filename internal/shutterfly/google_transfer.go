package shutterfly

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Plan

type transferPlan struct {
	moments   []string            // normalized moment IDs, library order
	albums    map[string][]string // normalized momentID -> google album titles (besides master)
	albumMeta []googleAlbumMeta
}

type googleAlbumMeta struct {
	title     string
	momentIDs []string
}

const masterAlbumBase = "Shutterfly"

// maxAlbumItems caps items per Google album (API limit 20,000).
var maxAlbumItems = 20000

func albumTitleFor(name string) string {
	return "Shutterfly - " + Sanitize(name)
}

func normalizeID(id string) string {
	return strings.TrimLeft(strings.TrimSpace(id), "0")
}

// BuildTransferPlan fetches library + album membership.
func (c *Client) BuildTransferPlan(restrictTo string) (*transferPlan, error) {
	libraryIDs, err := c.GetAllMomentIDs()
	if err != nil {
		return nil, fmt.Errorf("library: %w", err)
	}
	libSet := map[string]bool{}
	var moments []string
	seen := map[string]bool{}
	for _, id := range libraryIDs {
		n := normalizeID(id)
		if !seen[n] {
			seen[n] = true
			moments = append(moments, n)
		}
		libSet[n] = true
	}

	albums, err := c.GetAlbums()
	if err != nil {
		return nil, fmt.Errorf("albums: %w", err)
	}

	p := &transferPlan{moments: moments, albums: map[string][]string{}}
	albumIDsTotal, albumIDsMissing := 0, 0
	var sampleLib, sampleAlbum []string
	for i, id := range libraryIDs {
		if i < 3 {
			sampleLib = append(sampleLib, id)
		}
	}

	for _, a := range albums {
		if restrictTo != "" && a.Name != restrictTo {
			continue
		}
		if a.PhotoCount == 0 {
			continue
		}
		momentsStr, err := c.GetAlbum(a.ID)
		if err != nil {
			return nil, fmt.Errorf("album %s: %w", a.Name, err)
		}
		ids := ExtractMomentIDs(momentsStr)
		title := albumTitleFor(a.Name)
		meta := googleAlbumMeta{title: title}
		for _, raw := range ids {
			if len(sampleAlbum) < 3 {
				sampleAlbum = append(sampleAlbum, raw)
			}
			n := normalizeID(raw)
			albumIDsTotal++
			if !libSet[n] {
				albumIDsMissing++
				if !seen[n] {
					seen[n] = true
					p.moments = append(p.moments, n)
				}
			}
			p.albums[n] = appendUnique(p.albums[n], title)
			meta.momentIDs = append(meta.momentIDs, n)
		}
		p.albumMeta = append(p.albumMeta, meta)
	}

	if albumIDsTotal > 0 {
		pct := 100.0 * float64(albumIDsMissing) / float64(albumIDsTotal)
		fmt.Printf("Album/library ID join: %d of %d album moment IDs not in library set (%.1f%%)\n", albumIDsMissing, albumIDsTotal, pct)
		if pct > 1.0 {
			return nil, fmt.Errorf("album and library moment IDs don't join (%.1f%% missing); library IDs look like %v, album IDs like %v — ID spaces differ, stopping rather than guessing",
				pct, sampleLib, sampleAlbum)
		}
	}
	return p, nil
}

// downloadMomentBytes fetches one moment's bytes via the existing
// retry/token-refresh machinery.
func (c *Client) downloadMomentBytes(momentID string) ([]byte, string, string, error) {
	resp, err := c.request(apiCall{
		profile: libraryProfile,
		method:  "GET",
		url: func(token string) string {
			params := url.Values{"accessToken": {token}, "momentId": {momentID}, "source": {"library"}}
			return c.DownloadBase + "/download?" + params.Encode()
		},
	})
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	filename := "photo_" + momentID + ".jpg"
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(cd, "filename=") {
		if parts := strings.SplitN(cd, "filename=", 2); len(parts) == 2 {
			filename = filepath.Base(strings.Trim(parts[1], `"' `))
		}
	}
	mime, filename := uploadType(resp.Header.Get("Content-Type"), filename, data)
	return data, mime, filename, nil
}

// uploadType picks the MIME type and file name to send to Google. Shutterfly
// labels some Live Photo items as video/quicktime ".MOV" while serving a JPEG
// still; Google rejects that mismatch, so the sniffed image type wins and the
// extension is corrected to match.
func uploadType(header, filename string, data []byte) (string, string) {
	sniffed := http.DetectContentType(data)
	if strings.HasPrefix(sniffed, "image/") {
		if !strings.HasPrefix(header, "image/") || header != sniffed {
			if ext, ok := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/bmp": ".bmp"}[sniffed]; ok {
				if !strings.EqualFold(filepath.Ext(filename), ext) && !(ext == ".jpg" && strings.EqualFold(filepath.Ext(filename), ".jpeg")) {
					filename = strings.TrimSuffix(filename, filepath.Ext(filename)) + ext
				}
			}
		}
		return sniffed, filename
	}
	if header == "" || header == "application/octet-stream" {
		return sniffed, filename
	}
	return header, filename
}

// ---------------------------------------------------------------------------
// Transfer

type uploadResult struct {
	momentID string
	token    string // google upload token
	dupMIID  string // set when linked to an already-done identical item
	fileName string
	sha256   string
	bytes    int64
	err      error
}

type TransferStats struct {
	Uploaded int
	Skipped  int
	Linked   int // content dedupe links
	Failed   int
	Bytes    int64
}

// ToGoogle runs the streaming transfer against the SQLite state DB.
// expectedEmail (if non-empty) is the -google-account guard; yes skips the
// first-run confirmation only when expectedEmail also matches.
func (c *Client) ToGoogle(g *GoogleClient, statePath string, workers, limit int, restrictTo string, dryRun bool, expectedEmail string, yes bool) (*TransferStats, error) {
	st, err := OpenStateDB(statePath)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	plan, err := c.BuildTransferPlan(restrictTo)
	if err != nil {
		return nil, err
	}
	if restrictTo != "" {
		var only []string
		for _, m := range plan.moments {
			if len(plan.albums[m]) > 0 {
				only = append(only, m)
			}
		}
		plan.moments = only
	}
	if limit > 0 && len(plan.moments) > limit {
		plan.moments = plan.moments[:limit]
	}

	stats := &TransferStats{}

	if dryRun {
		fmt.Printf("\nDry run plan:\n  moments to transfer: %d\n  albums to ensure: %d (master %q + %d album albums)\n",
			len(plan.moments), 1+len(plan.albumMeta), masterAlbumBase, len(plan.albumMeta))
		for _, m := range plan.albumMeta {
			fmt.Printf("    %s: %d photos\n", m.title, len(m.momentIDs))
		}
		return stats, nil
	}

	if g == nil {
		return nil, fmt.Errorf("no Google client; provide -google-client and sign in")
	}

	// Account guard: must run before any album creation or upload.
	if expectedEmail != "" && !strings.EqualFold(g.Email, expectedEmail) {
		return nil, fmt.Errorf("signed in as %s, expected %s — delete ~/.config/shutterfly/google-token.json and sign in with %s",
			g.Email, expectedEmail, expectedEmail)
	}
	if stored := st.metaGet("google_account"); stored != "" && !strings.EqualFold(g.Email, stored) {
		return nil, fmt.Errorf("signed in as %s, but this transfer belongs to %s — delete ~/.config/shutterfly/google-token.json and sign in with %s (or use a fresh -google-state)",
			g.Email, stored, stored)
	}
	if st.metaGet("google_account") == "" {
		confirmed := yes && expectedEmail != ""
		if !confirmed {
			fmt.Printf("Signed in to Google as %s. Upload %d photos into this account? [y/N] ", g.Email, len(plan.moments))
			var ans string
			fmt.Fscan(c.Stdin, &ans)
			confirmed = strings.EqualFold(strings.TrimSpace(ans), "y")
		}
		if !confirmed {
			return nil, fmt.Errorf("aborted")
		}
		if err := st.metaSet("google_account", g.Email); err != nil {
			return nil, err
		}
	}

	// Load plan into DB (creates pending items + memberships).
	if err := st.loadPlan(plan, masterAlbumBase); err != nil {
		return nil, err
	}

	// Ensure album rows exist on Google and store their ids.
	if err := ensureAlbums(g, st, plan); err != nil {
		return nil, err
	}

	// Reconcile any items left 'creating' by a previous crash.
	if c.RetryFailedGoogle {
		if n, err := st.resetFailed(); err != nil {
			return nil, err
		} else if n > 0 {
			fmt.Printf("Retrying %d previously failed item(s)\n", n)
		}
	}
	if err := Reconcile(g, st); err != nil {
		fmt.Printf("warning: reconcile failed: %v\n", err)
	}

	// Todo: pending or failed-with-attempts-left items.
	var todo []string
	rows, err := st.db.Query(`SELECT moment_id FROM items WHERE status='pending'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		todo = append(todo, id)
	}
	rows.Close()
	var total int64
	st.db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&total)
	stats.Skipped = int(total) - len(todo)
	fmt.Printf("Transferring %d photos (%d already done, %d workers)\n", len(todo), stats.Skipped, workers)

	var uploaded, failed, linked int64
	var stopFlag atomic.Bool // quota (429) seen
	var abort atomic.Bool    // creator hit an error; drain and stop

	jobs := make(chan string)
	results := make(chan uploadResult)
	var deferred []uploadResult

	// Creator: serial batchCreate + dedupe decisions; the only DB writer.
	var creatorWG sync.WaitGroup
	creatorWG.Add(1)
	var creatorErr error
	go func() {
		defer creatorWG.Done()
		inflight := map[string]string{} // sha256 -> momentID already queued this run
		batch := make([]uploadResult, 0, 50)
		flush := func() {
			if len(batch) == 0 || creatorErr != nil {
				return
			}
			masterTitle, _ := currentMasterTitle(st)
			masterID, _ := st.albumID(masterTitle)
			batchID := fmt.Sprintf("b%d", time.Now().UnixNano())
			if err := st.beginBatch(batchID, batch); err != nil {
				creatorErr = err
				return
			}
			items := make([]struct{ UploadToken, FileName string }, len(batch))
			for i, r := range batch {
				items[i].UploadToken = r.token
				items[i].FileName = r.fileName
			}
			created, err := g.batchCreate(masterID, items)
			if err != nil {
				creatorErr = err // 'creating' reconciles next run
				return
			}
			mediaIDs := map[string]string{}
			for _, ci := range created {
				if ci.OK {
					mediaIDs[ci.UploadToken] = ci.MediaItemID
				}
			}
			if err := st.finishBatch(masterTitle, batch, mediaIDs); err != nil {
				creatorErr = err
				return
			}
			// Record Google's reason for per-item failures (shown by -status).
			byToken := map[string]string{}
			for _, r := range batch {
				byToken[r.token] = r.momentID
			}
			for _, ci := range created {
				if !ci.OK {
					if mid, ok := byToken[ci.UploadToken]; ok {
						st.db.Exec(`UPDATE items SET last_error=? WHERE moment_id=?`, "batchCreate: "+ci.Message, mid)
					}
				}
			}
			for _, r := range batch {
				if _, ok := mediaIDs[r.token]; ok {
					atomic.AddInt64(&uploaded, 1)
					atomic.AddInt64(&stats.Bytes, r.bytes)
					delete(inflight, r.sha256)
				} else {
					atomic.AddInt64(&failed, 1)
				}
			}
			if (uploaded+int64(stats.Skipped))%100 < 50 {
				fmt.Printf("Progress: %d/%d done, %d failed, %.1f MB\n",
					uploaded+int64(stats.Skipped), total, failed, float64(stats.Bytes)/1e6)
			}
			batch = batch[:0]
		}
		// Never return early: workers block on send until results is drained.
		for r := range results {
			if stopFlag.Load() || abort.Load() {
				continue
			}
			if r.err != nil {
				atomic.AddInt64(&failed, 1)
				continue
			}
			if r.dupMIID != "" {
				if err := st.linkDup(r.momentID, r.dupMIID, r.sha256, "dup-of:sha"); err != nil {
					creatorErr = err
					abort.Store(true)
					continue
				}
				atomic.AddInt64(&linked, 1)
				continue
			}
			// content dedupe: already done with same hash, or in-flight
			if dupMoment, dupMIID, ok := st.shaDone(r.sha256); ok {
				note := "dup-of:" + dupMoment
				if err := st.linkDup(r.momentID, dupMIID, r.sha256, note); err != nil {
					creatorErr = err
					abort.Store(true)
					continue
				}
				// copy master+album memberships via pending pass
				atomic.AddInt64(&linked, 1)
				continue
			}
			if _, dup := inflight[r.sha256]; dup {
				// link to the in-flight item once it's done: mark pending;
				// shaDone will link it next pass — simplest: defer by leaving
				// it 'pending' and letting the dedupe catch it on the next run,
				// or re-check after flush. We handle it post-flush below.
				r.err = fmt.Errorf("deferred-dup")
				// park it on a deferred list appended to batch end marker
				deferred = append(deferred, r)
				continue
			}
			inflight[r.sha256] = r.momentID
			batch = append(batch, r)
			if len(batch) >= 50 {
				flush()
				if creatorErr != nil {
					abort.Store(true)
				}
			}
		}
		flush()
		// deferred dups: now that inflight items are done, link them
		for _, r := range deferred {
			if _, dupMIID, ok := st.shaDone(r.sha256); ok {
				st.linkDup(r.momentID, dupMIID, r.sha256, "dup-of:inflight")
				atomic.AddInt64(&linked, 1)
			} else {
				st.db.Exec(`UPDATE items SET status='pending' WHERE moment_id=?`, r.momentID)
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if stopFlag.Load() || abort.Load() {
					continue
				}
				data, mime, fname, err := c.downloadMomentBytes(id)
				if err != nil {
					results <- uploadResult{momentID: id, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				sum := sha256.Sum256(data)
				sha := hex.EncodeToString(sum[:])
				// content dedupe before uploading: already-done identical bytes
				if _, miid, ok := st.shaDone(sha); ok {
					results <- uploadResult{momentID: id, sha256: sha, dupMIID: miid, fileName: fname, bytes: int64(len(data))}
					time.Sleep(c.RateLimit)
					continue
				}
				tok, err := g.upload(data, mime, fname)
				if err == ErrQuota {
					stopFlag.Store(true)
					results <- uploadResult{momentID: id, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				if err != nil {
					results <- uploadResult{momentID: id, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				results <- uploadResult{momentID: id, token: tok, fileName: fname, sha256: sha, bytes: int64(len(data))}
				time.Sleep(c.RateLimit)
			}
		}()
	}
	for _, id := range todo {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	close(results)
	creatorWG.Wait()

	stats.Uploaded = int(uploaded)
	stats.Linked = int(linked)
	stats.Failed = int(failed)
	failedPath := filepath.Join(filepath.Dir(statePath), "google-failed.txt")

	if creatorErr == ErrQuota || stopFlag.Load() {
		writeFailedIDs(st, failedPath)
		fmt.Println("\nGoogle Photos quota reached (429). State saved — re-run tomorrow to resume.")
		return stats, ErrQuota
	}
	if creatorErr != nil {
		writeFailedIDs(st, failedPath)
		return stats, fmt.Errorf("transfer stopped (state saved; re-run to resume): %w", creatorErr)
	}

	// Album memberships: pending rows where the item is done.
	if err := addMemberships(g, st); err != nil {
		if err == ErrQuota {
			writeFailedIDs(st, failedPath)
			fmt.Println("\nGoogle Photos quota reached (429). State saved — re-run tomorrow to resume.")
			return stats, ErrQuota
		}
		return stats, err
	}

	writeFailedIDs(st, failedPath)
	fmt.Printf("\nTransfer complete!\n  Uploaded: %d (skipped %d already done, %d linked dups)\n  Failed: %d\n  Total bytes uploaded: %.1f MB\n",
		stats.Uploaded, stats.Skipped, stats.Linked, stats.Failed, float64(stats.Bytes)/1e6)
	return stats, nil
}

// currentMasterTitle returns the first master album title not yet full.
func currentMasterTitle(st *StateDB) (string, error) {
	counts, err := st.membershipCounts(true)
	if err != nil {
		return "", err
	}
	for n := 1; ; n++ {
		title := masterAlbumBase
		if n > 1 {
			title = fmt.Sprintf("%s (%d)", masterAlbumBase, n)
		}
		if counts[title] < maxAlbumItems {
			return title, nil
		}
	}
}

// ensureAlbums recovers/creates all needed Google albums and stores google_id.
func ensureAlbums(g *GoogleClient, st *StateDB, plan *transferPlan) error {
	existing, err := g.listAlbums()
	if err != nil {
		return err
	}
	needed := map[string]bool{masterAlbumBase: true}
	for _, meta := range plan.albumMeta {
		needed[meta.title] = true
	}
	// master rollover titles may also exist already
	for t := range existing {
		if strings.HasPrefix(t, masterAlbumBase) {
			needed[t] = true
		}
	}
	for title := range needed {
		if _, ok := st.albumID(title); ok {
			continue
		}
		id, ok := existing[title]
		if !ok {
			var err error
			id, err = g.createAlbum(title)
			if err != nil {
				return err
			}
		}
		if err := st.setAlbumID(title, id); err != nil {
			return err
		}
	}
	return nil
}

// addMemberships pushes pending membership rows to Google in 50-id chunks.
func addMemberships(g *GoogleClient, st *StateDB) error {
	titles, err := st.membershipCounts(false)
	if err != nil {
		return err
	}
	for title := range titles {
		albumID, ok := st.albumID(title)
		if !ok {
			continue
		}
		ids, err := st.pendingMemberships(title)
		if err != nil {
			return err
		}
		for i := 0; i < len(ids); i += 50 {
			chunk := ids[i:min(i+50, len(ids))]
			if err := g.batchAddMediaItems(albumID, chunk); err != nil {
				return err // all-or-nothing: rows stay added=0
			}
			if err := st.markMemberships(title, chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcile fixes items left 'creating' by a crash around batchCreate.
// Reconcile fixes items left creating by a crash around batchCreate.
func Reconcile(g *GoogleClient, st *StateDB) error {
	creating, err := st.creatingItems()
	if err != nil || len(creating) == 0 {
		return err
	}
	fmt.Printf("Reconciling %d item(s) left 'creating'...\n", len(creating))
	assigned, err := st.assignedMediaIDs()
	if err != nil {
		return err
	}
	// filename -> unassigned media item ids
	byName := map[string][]string{}
	page := ""
	for {
		items, next, err := g.listMediaItems(page)
		if err != nil {
			return err
		}
		for _, it := range items {
			if !assigned[it.ID] {
				byName[it.FileName] = append(byName[it.FileName], it.ID)
			}
		}
		if next == "" {
			break
		}
		page = next
	}

	var byFile map[string][]string = byName
	ambiguous := 0
	resolved := 0
	// group creating items by filename
	groups := map[string][]string{}
	for mid, fn := range creating {
		groups[fn] = append(groups[fn], mid)
	}
	for fn, mids := range groups {
		ids := byFile[fn]
		switch {
		case len(ids) == 0:
			for _, mid := range mids {
				st.markPending(mid)
				resolved++
			}
		case len(ids) == len(mids):
			// one-to-one (or same-count) assignment in id order
			sortStrings(ids)
			sortStrings(mids)
			for i, mid := range mids {
				st.markDone(mid, ids[i])
				resolved++
			}
		case len(ids) == 1 && len(mids) == 1:
			st.markDone(mids[0], ids[0])
			resolved++
		default:
			ambiguous += len(mids)
			fmt.Printf("  needs manual check: filename %q has %d remote items vs %d 'creating' rows\n", fn, len(ids), len(mids))
		}
	}
	fmt.Printf("Reconcile: %d resolved, %d ambiguous\n", resolved, ambiguous)
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sortStrings(s []string) { sort.Strings(s) }

func writeFailedIDs(st *StateDB, path string) {
	ids, err := st.failedIDs()
	if err != nil || len(ids) == 0 {
		os.Remove(path)
		return
	}
	os.WriteFile(path, []byte(strings.Join(ids, "\n")+"\n"), 0600)
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
