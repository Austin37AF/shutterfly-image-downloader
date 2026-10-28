package shutterfly

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// State file

// GoogleState records created Google albums and per-moment results so runs
// are resumable.
type GoogleState struct {
	Albums map[string]string          `json:"albums"` // google album title -> album id
	Items  map[string]GoogleItemState `json:"items"`  // normalized momentID -> result
}

type GoogleItemState struct {
	MediaItemID string   `json:"mediaItemId"`
	Albums      []string `json:"albums"` // google album titles the item belongs to
}

func loadGoogleState(path string) *GoogleState {
	s := &GoogleState{Albums: map[string]string{}, Items: map[string]GoogleItemState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	// Corrupt state is safer ignored than half-applied.
	if err := json.Unmarshal(data, s); err != nil {
		fmt.Printf("warning: ignoring unreadable state file %s: %v\n", path, err)
	}
	return s
}

func (s *GoogleState) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Plan

type transferPlan struct {
	moments   []string            // normalized moment IDs, library order
	albums    map[string][]string // normalized momentID -> google album titles to add (besides master)
	albumMeta []googleAlbumMeta   // per Google album: title -> member momentIDs
}

type googleAlbumMeta struct {
	title     string
	momentIDs []string
}

// masterAlbumBase is the fixed master album title.
const masterAlbumBase = "Shutterfly"

// maxAlbumItems caps items per Google album (API limit 20,000). A variable so
// tests can shrink it.
var maxAlbumItems = 20000

// albumTitleFor is the Google album title for a Shutterfly album name.
func albumTitleFor(name string) string {
	return "Shutterfly - " + Sanitize(name)
}

// normalizeID strips leading zeros and trims, matching Shutterfly's moment ID
// normalization across the library and album APIs.
func normalizeID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimLeft(id, "0")
	return id
}

// BuildTransferPlan fetches library + album membership and returns the plan.
// It verifies the two ID spaces join; >1% unmatched album IDs aborts.
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
				// still transfer album-only photos, appended at the end
				if !seen[n] {
					seen[n] = true
					p.moments = append(p.moments, n)
				}
			}
			p.albums[n] = append(p.albums[n], title)
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

// ---------------------------------------------------------------------------
// Download helper (bytes only, no disk)

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
	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	return data, mime, filename, nil
}

// ---------------------------------------------------------------------------
// Transfer

// TransferStats summarises a run.
type TransferStats struct {
	Uploaded   int
	Skipped    int
	Failed     int
	FailedIDs  []string
	Bytes      int64
	AlbumItems map[string]int // google title -> items added this run
}

// ToGoogle runs the streaming transfer. restrictTo limits to one Shutterfly
// album; limit caps total moments; dryRun prints the plan without Google calls.
func (c *Client) ToGoogle(g *GoogleClient, statePath string, workers, limit int, restrictTo string, dryRun bool) (*TransferStats, error) {
	plan, err := c.BuildTransferPlan(restrictTo)
	if err != nil {
		return nil, err
	}
	if restrictTo != "" {
		// keep only moments that belong to the restricted album
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

	state := loadGoogleState(statePath)

	if dryRun {
		fmt.Printf("\nDry run plan:\n")
		fmt.Printf("  moments to transfer: %d\n", len(plan.moments))
		fmt.Printf("  already done: %d\n", countDone(state, plan.moments))
		fmt.Printf("  albums to ensure: %d (master %q + %d album albums)\n", 1+len(plan.albumMeta), masterAlbumBase, len(plan.albumMeta))
		for _, m := range plan.albumMeta {
			fmt.Printf("    %s: %d photos\n", m.title, len(m.momentIDs))
		}
		if len(state.Albums) > 0 {
			fmt.Printf("  existing state albums: %d\n", len(state.Albums))
		}
		return &TransferStats{}, nil
	}

	if g == nil {
		return nil, fmt.Errorf("no Google client; provide -google-client and sign in")
	}

	stats := &TransferStats{AlbumItems: map[string]int{}}
	failedPath := filepath.Join(filepath.Dir(statePath), "google-failed.txt")

	// Recover/create the master album (with rollover), then per-album titles.
	masterTitle, err := ensureMasterAlbum(g, state)
	if err != nil {
		return nil, err
	}
	if err := ensureAlbumTitles(g, state, plan); err != nil {
		return nil, err
	}
	if err := state.save(statePath); err != nil {
		return nil, err
	}

	// Split moments into done vs todo.
	var todo []string
	for _, m := range plan.moments {
		if it, ok := state.Items[m]; ok && it.MediaItemID != "" {
			stats.Skipped++
		} else {
			todo = append(todo, m)
		}
	}
	fmt.Printf("Transferring %d photos (%d already done, %d workers)\n", len(todo), stats.Skipped, workers)

	type uploadResult struct {
		momentID string
		token    string
		fileName string
		bytes    int64
		err      error
	}
	jobs := make(chan string)
	results := make(chan uploadResult)
	var stopFlag sync.Map

	// Creator: serial batchCreate into the master album.
	var creatorWG sync.WaitGroup
	creatorWG.Add(1)
	var creatorErr error
	go func() {
		defer creatorWG.Done()
		batch := make([]uploadResult, 0, 50)
		flush := func() {
			if len(batch) == 0 || creatorErr != nil {
				return
			}
			items := make([]struct{ UploadToken, FileName string }, len(batch))
			for i, r := range batch {
				items[i].UploadToken = r.token
				items[i].FileName = r.fileName
			}
			created, err := g.batchCreate(state.Albums[masterTitle], items)
			if err != nil {
				creatorErr = err
				return
			}
			byToken := map[string]uploadResult{}
			for _, r := range batch {
				byToken[r.token] = r
			}
			for _, ci := range created {
				r := byToken[ci.UploadToken]
				if ci.OK && ci.MediaItemID != "" {
					it := state.Items[r.momentID]
					it.MediaItemID = ci.MediaItemID
					it.Albums = appendUnique(it.Albums, masterTitle)
					state.Items[r.momentID] = it
					stats.Uploaded++
					stats.Bytes += r.bytes
					stats.AlbumItems[masterTitle]++
				} else {
					stats.Failed++
					stats.FailedIDs = append(stats.FailedIDs, r.momentID)
				}
			}
			if err := state.save(statePath); err != nil {
				fmt.Printf("warning: state save: %v\n", err)
			}
			batch = batch[:0]
			if (stats.Uploaded+stats.Skipped)%100 < 50 {
				fmt.Printf("Progress: %d/%d done, %d failed, %.1f MB\n",
					stats.Uploaded+stats.Skipped, len(plan.moments), stats.Failed, float64(stats.Bytes)/1e6)
			}
		}
		// Keep draining results after an error so workers never block on send.
		for r := range results {
			if creatorErr != nil {
				continue
			}
			if r.err != nil {
				stats.Failed++
				stats.FailedIDs = append(stats.FailedIDs, r.momentID)
				continue
			}
			batch = append(batch, r)
			if len(batch) >= 50 {
				flush()
				if creatorErr != nil {
					stopFlag.Store("stop", true)
				}
			}
		}
		flush()
	}()

	// Workers: download + upload, stop on quota.
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if _, stopped := stopFlag.Load("stop"); stopped {
					continue
				}
				data, mime, fname, err := c.downloadMomentBytes(id)
				if err != nil {
					results <- uploadResult{momentID: id, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				tok, err := g.upload(data, mime, fname)
				if err == ErrQuota {
					stopFlag.Store("q", true)
					stopFlag.Store("stop", true)
					results <- uploadResult{momentID: id, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				if err != nil {
					results <- uploadResult{momentID: id, err: err}
					time.Sleep(c.RateLimit)
					continue
				}
				results <- uploadResult{momentID: id, token: tok, fileName: fname, bytes: int64(len(data))}
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

	if creatorErr == ErrQuota || func() bool { _, ok := stopFlag.Load("q"); return ok }() {
		state.save(statePath)
		writeFailed(failedPath, stats.FailedIDs)
		fmt.Println("\nGoogle Photos quota reached (429). State saved — re-run tomorrow to resume.")
		return stats, ErrQuota
	}
	if creatorErr != nil {
		state.save(statePath)
		return stats, creatorErr
	}

	// Album memberships for done items.
	if err := c.addMemberships(g, state, plan, statePath); err != nil {
		state.save(statePath)
		if err == ErrQuota {
			writeFailed(failedPath, stats.FailedIDs)
			fmt.Println("\nGoogle Photos quota reached (429). State saved — re-run tomorrow to resume.")
			return stats, ErrQuota
		}
		return stats, err
	}

	if err := state.save(statePath); err != nil {
		return nil, err
	}
	writeFailed(failedPath, stats.FailedIDs)

	fmt.Printf("\nTransfer complete!\n")
	fmt.Printf("  Uploaded: %d (skipped %d already done)\n", stats.Uploaded, stats.Skipped)
	fmt.Printf("  Failed: %d\n", stats.Failed)
	fmt.Printf("  Total bytes uploaded: %.1f MB\n", float64(stats.Bytes)/1e6)
	for title, n := range stats.AlbumItems {
		fmt.Printf("  %s: +%d items\n", title, n)
	}
	return stats, nil
}

// ensureMasterAlbum returns the active master title (rollover to
// "Shutterfly (N)" when the current one reaches maxAlbumItems), creating or
// recovering albums as needed.
func ensureMasterAlbum(g *GoogleClient, state *GoogleState) (string, error) {
	// Recover albums from Google if the state lost them.
	if len(state.Albums) == 0 {
		existing, err := g.listAlbums()
		if err != nil {
			return "", err
		}
		for t, id := range existing {
			state.Albums[t] = id
		}
	}
	for n := 1; ; n++ {
		title := masterAlbumBase
		if n > 1 {
			title = fmt.Sprintf("%s (%d)", masterAlbumBase, n)
		}
		id, ok := state.Albums[title]
		if !ok {
			var err error
			id, err = g.createAlbum(title)
			if err != nil {
				return "", err
			}
			state.Albums[title] = id
		}
		// Count current items in state for this title; rollover when full.
		count := 0
		for _, it := range state.Items {
			for _, t := range it.Albums {
				if t == title {
					count++
				}
			}
		}
		if count < maxAlbumItems {
			return title, nil
		}
		fmt.Printf("Master album %s full (%d items); rolling over\n", title, count)
	}
}

// ensureAlbumTitles creates/recovers the per-Shutterfly-album Google albums.
func ensureAlbumTitles(g *GoogleClient, state *GoogleState, plan *transferPlan) error {
	if len(state.Albums) == 0 {
		existing, err := g.listAlbums()
		if err != nil {
			return err
		}
		for t, id := range existing {
			state.Albums[t] = id
		}
	}
	for _, meta := range plan.albumMeta {
		if _, ok := state.Albums[meta.title]; ok {
			continue
		}
		id, err := g.createAlbum(meta.title)
		if err != nil {
			return err
		}
		state.Albums[meta.title] = id
	}
	return nil
}

// addMemberships batchAdds every done item into each required Google album
// title it isn't yet recorded under (chunks of 50, all-or-nothing).
func (c *Client) addMemberships(g *GoogleClient, state *GoogleState, plan *transferPlan, statePath string) error {
	// For each google album title, the items that need membership added.
	for _, meta := range plan.albumMeta {
		albumID, ok := state.Albums[meta.title]
		if !ok {
			continue
		}
		var pending []string
		for _, mid := range meta.momentIDs {
			it, ok := state.Items[mid]
			if !ok || it.MediaItemID == "" {
				continue
			}
			if !contains(it.Albums, meta.title) {
				pending = append(pending, mid)
			}
		}
		for i := 0; i < len(pending); i += 50 {
			end := i + 50
			if end > len(pending) {
				end = len(pending)
			}
			chunk := pending[i:end]
			ids := make([]string, len(chunk))
			for j, mid := range chunk {
				ids[j] = state.Items[mid].MediaItemID
			}
			if err := g.batchAddMediaItems(albumID, ids); err != nil {
				return err // all-or-nothing: state stays unchanged for this chunk
			}
			for _, mid := range chunk {
				it := state.Items[mid]
				it.Albums = appendUnique(it.Albums, meta.title)
				state.Items[mid] = it
			}
			if err := state.save(statePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func countDone(state *GoogleState, moments []string) int {
	n := 0
	for _, m := range moments {
		if it, ok := state.Items[m]; ok && it.MediaItemID != "" {
			n++
		}
	}
	return n
}

func writeFailed(path string, ids []string) {
	if len(ids) == 0 {
		os.Remove(path)
		return
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte(strings.Join(ids, "\n")+"\n"), 0600)
}

func appendUnique(s []string, v string) []string {
	if !contains(s, v) {
		return append(s, v)
	}
	return s
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
