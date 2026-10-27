package shutterfly

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Album mode — ported from JoshBurke/Shutterfly-Album-Downloader/downloader.py.

type Album struct {
	ID         string
	Name       string
	PhotoCount int
}

// GetAlbums: POST /json?method=album.getAlbums, params
// [token, userID, null, null, true], subsource "library".
func (c *Client) GetAlbums() ([]Album, error) {
	uid := c.userID()
	resp, err := c.request(apiCall{
		profile: albumProfile,
		method:  "POST",
		url:     func(string) string { return c.AlbumBase + "/json?method=album.getAlbums" },
		body: func(token string) io.Reader {
			return jsonBody(map[string]interface{}{
				"method":  "album.getAlbums",
				"params":  []interface{}{token, uid, nil, nil, true},
				"headers": map[string]string{"X-SFLY-SubSource": "library"},
				"id":      nil,
			})
		},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
			Payload [][]struct {
				Story struct {
					UID                string `json:"uid"`
					Name               string `json:"name"`
					VisibleMomentCount int    `json:"visible_moment_count"`
				} `json:"story"`
			} `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.Result.Success {
		return nil, fmt.Errorf("failed to get albums: %s", result.Result.Message)
	}

	var albums []Album
	if len(result.Result.Payload) > 0 {
		for _, perm := range result.Result.Payload[0] {
			albums = append(albums, Album{
				ID:         perm.Story.UID,
				Name:       perm.Story.Name,
				PhotoCount: perm.Story.VisibleMomentCount,
			})
		}
	}
	return albums, nil
}

// GetAlbum: POST /json?method=album.getAlbum, params
// [token, albumID, "startupItem", null, false, true, true], subsource "albums".
// Returns the payload.moments string.
func (c *Client) GetAlbum(albumID string) (string, error) {
	resp, err := c.request(apiCall{
		profile: albumProfile,
		method:  "POST",
		url:     func(string) string { return c.AlbumBase + "/json?method=album.getAlbum" },
		body: func(token string) io.Reader {
			return jsonBody(map[string]interface{}{
				"method":  "album.getAlbum",
				"params":  []interface{}{token, albumID, "startupItem", nil, false, true, true},
				"headers": map[string]string{"X-SFLY-SubSource": "albums"},
				"id":      nil,
			})
		},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
			Payload struct {
				Moments string `json:"moments"`
			} `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Result.Success {
		return "", fmt.Errorf("failed to get album: %s", result.Result.Message)
	}
	return result.Result.Payload.Moments, nil
}

// ExtractMomentIDs splits the moments blob into 277-char records; the ID is
// record[9:25] with leading zeros stripped. Short trailing records are skipped.
func ExtractMomentIDs(moments string) []string {
	var ids []string
	for i := 0; i < len(moments); i += 277 {
		record := moments[i:min(i+277, len(moments))]
		if len(record) < 25 {
			continue
		}
		ids = append(ids, strings.TrimLeft(record[9:25], "0"))
	}
	return ids
}

// Sanitize keeps unicode letters/digits, space, '-' and '_', then trims
// trailing spaces (Python isalnum + rstrip).
func Sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// albumDir returns (creating) the output dir for a sanitized album name.
func (c *Client) albumDir(albumName string) string {
	dir := filepath.Join(c.OutputDir, Sanitize(albumName))
	os.MkdirAll(dir, 0755)
	return dir
}

// downloadAlbumPhoto downloads one moment into albumDir. Same-name handling is
// idempotent across runs: under a per-dir mutex the data is compared against
// the existing name.ext and every existing name_N.ext; identical → skip
// (success), else saved as the lowest free name_N.ext. Writes go through a
// dot-prefixed temp file and rename.
func (c *Client) downloadAlbumPhoto(momentID, albumDir string, index int) bool {
	resp, err := c.request(apiCall{
		profile: albumProfile,
		method:  "GET",
		url: func(token string) string {
			params := url.Values{"accessToken": {token}, "momentId": {momentID}, "source": {"library"}}
			return c.DownloadBase + "/download?" + params.Encode()
		},
	})
	if err != nil {
		fmt.Printf("Error downloading %s: %v\n", momentID, err)
		return false
	}
	defer resp.Body.Close()

	filename := fmt.Sprintf("photo_%04d_%s.jpg", index, momentID)
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(cd, "filename=") {
		parts := strings.Split(cd, "filename=")
		if len(parts) > 1 {
			filename = filepath.Base(strings.Trim(parts[1], `"' `))
		}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", momentID, err)
		return false
	}

	mu := c.dirMutex(albumDir)
	mu.Lock()
	defer mu.Unlock()

	tmp, err := os.CreateTemp(albumDir, ".part-*")
	if err != nil {
		fmt.Printf("Error creating temp file: %v\n", err)
		return false
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		fmt.Printf("Error writing temp file: %v\n", err)
		return false
	}
	tmp.Close()

	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	target := ""
	for n := 0; ; n++ {
		candidate := filename
		if n > 0 {
			candidate = fmt.Sprintf("%s_%d%s", base, n, ext)
		}
		path := filepath.Join(albumDir, candidate)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			target = path
			filename = candidate
			break
		}
		if Identical(path, tmpName) {
			os.Remove(tmpName)
			fmt.Printf("Skipping (identical): %s\n", candidate)
			return true
		}
	}

	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		fmt.Printf("Error moving %s into place: %v\n", filename, err)
		return false
	}

	if originalTime := extractExifDate(data); !originalTime.IsZero() {
		os.Chtimes(target, originalTime, originalTime)
	}

	fmt.Printf("Downloaded: %s\n", filename)
	return true
}

// DownloadAlbum fetches an album's moments and downloads them with workers.
// Failed moment IDs are appended to failed as "<albumID>\t<momentID>".
func (c *Client) DownloadAlbum(album Album, workers int) (int, int, []string) {
	fmt.Printf("\nProcessing album: %s\n", album.Name)

	momentsString, err := c.GetAlbum(album.ID)
	if err != nil {
		fmt.Printf("Failed to get album contents for %s: %v\n", album.Name, err)
		return 0, 0, nil
	}
	momentIDs := ExtractMomentIDs(momentsString)
	fmt.Printf("Found %d photos in album\n", len(momentIDs))

	seen := map[string]int{}
	for i, id := range momentIDs {
		if prev, ok := seen[id]; ok {
			fmt.Printf("\nWarning: Duplicate moment ID %s found at positions %d and %d\n", id, prev, i+1)
		} else {
			seen[id] = i + 1
		}
	}
	if len(seen) != len(momentIDs) {
		fmt.Printf("\nWarning: Album contains %d photos but only %d unique moment IDs\n", len(momentIDs), len(seen))
	}

	albumDir := c.albumDir(album.Name)

	var failed []string
	var failMu sync.Mutex
	success, fail := c.runWorkers(momentIDs, workers, func(id string, index int) bool {
		ok := c.downloadAlbumPhoto(id, albumDir, index)
		if !ok {
			failMu.Lock()
			failed = append(failed, album.ID+"\t"+id)
			failMu.Unlock()
		}
		return ok
	}, nil)
	return success, fail, failed
}

// writeFailedLog appends "<albumID>\t<momentID>" lines to failed_downloads.txt.
func (c *Client) writeFailedLog(failed []string, header string) {
	if len(failed) == 0 {
		return
	}
	path := filepath.Join(c.OutputDir, "failed_downloads.txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Warning: couldn't write failure log: %v\n", err)
		return
	}
	defer f.Close()
	f.WriteString(header + "\n")
	for _, line := range failed {
		f.WriteString(line + "\n")
	}
}

// albumFailureLogPath mirrors the library-mode file.
func (c *Client) albumFailureLogPath() string {
	return filepath.Join(c.OutputDir, "failed_downloads.txt")
}

// DownloadAlbums downloads all albums (optionally resuming from resumeFrom).
func (c *Client) DownloadAlbums(resumeFrom string, workers int) (int, int) {
	albums, err := c.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}
	fmt.Printf("Found %d albums\n", len(albums))

	start := 0
	if resumeFrom != "" {
		found := false
		for i, a := range albums {
			if a.Name == resumeFrom {
				start = i
				found = true
				fmt.Printf("Resuming from album: %s\n", resumeFrom)
				break
			}
		}
		if !found {
			fmt.Printf("Warning: Album '%s' not found, starting from beginning\n", resumeFrom)
		}
	}

	totalSuccess, totalFail := 0, 0
	var failed []string
	for _, a := range albums[start:] {
		if c.shouldIgnoreAlbum(a.Name) {
			continue
		}
		fmt.Printf("\nProcessing album: %s (%d photos)\n", a.Name, a.PhotoCount)
		s, f, failedIDs := c.DownloadAlbum(a, workers)
		totalSuccess += s
		totalFail += f
		failed = append(failed, failedIDs...)
		time.Sleep(c.RateLimit)
	}
	c.writeFailedLog(failed, "# Failed downloads - "+time.Now().Format(time.RFC3339))
	return totalSuccess, totalFail
}

// DownloadSingleAlbum downloads one album by exact name; prints the available
// names when not found.
func (c *Client) DownloadSingleAlbum(name string, workers int) (int, int) {
	albums, err := c.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}
	for _, a := range albums {
		if a.Name == name {
			fmt.Printf("\nFound album: %s (%d photos)\n", a.Name, a.PhotoCount)
			s, f, failedIDs := c.DownloadAlbum(a, workers)
			c.writeFailedLog(failedIDs, "# Failed downloads - "+time.Now().Format(time.RFC3339))
			return s, f
		}
	}
	fmt.Printf("\nAlbum not found: %s\n\nAvailable albums:\n", name)
	for _, a := range albums {
		fmt.Printf("- %s\n", a.Name)
	}
	return 0, 0
}

// CountAlbums prints per-album counts and a total (Python count_items).
func (c *Client) CountAlbums() {
	albums, err := c.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	total := 0
	for _, a := range albums {
		total += a.PhotoCount
	}
	fmt.Printf("\nFound %d albums containing %d total photos:\n", len(albums), total)
	for _, a := range albums {
		fmt.Printf("- %s: %d photos\n", a.Name, a.PhotoCount)
	}
}

// RetryAlbumFailed re-downloads failed_downloads.txt lines formatted as
// "<albumID>\t<momentID>", re-fetching getAlbums for names.
func (c *Client) RetryAlbumFailed(workers int) (int, int) {
	data, err := os.ReadFile(c.albumFailureLogPath())
	if err != nil {
		fmt.Printf("No failed_downloads.txt found: %v\n", err)
		return 0, 0
	}
	type failure struct {
		albumID  string
		momentID string
	}
	byAlbum := map[string][]string{}
	var order []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if _, ok := byAlbum[parts[0]]; !ok {
			order = append(order, parts[0])
		}
		byAlbum[parts[0]] = append(byAlbum[parts[0]], parts[1])
	}
	if len(byAlbum) == 0 {
		fmt.Println("No failed downloads to retry")
		return 0, 0
	}

	albums, err := c.GetAlbums()
	if err != nil {
		fmt.Printf("Error fetching albums: %v\n", err)
		return 0, 0
	}
	names := map[string]string{}
	for _, a := range albums {
		names[a.ID] = a.Name
	}

	var failed []string
	var failMu sync.Mutex
	totalSuccess, totalFail := 0, 0
	for _, albumID := range order {
		name, ok := names[albumID]
		if !ok {
			fmt.Printf("Warning: album %s no longer exists, skipping %d failures\n", albumID, len(byAlbum[albumID]))
			for _, id := range byAlbum[albumID] {
				failed = append(failed, albumID+"\t"+id)
			}
			continue
		}
		albumDir := c.albumDir(name)
		ids := byAlbum[albumID]
		fmt.Printf("\nRetrying %d downloads for album: %s\n", len(ids), name)
		s, f := c.runWorkers(ids, workers, func(id string, index int) bool {
			ok := c.downloadAlbumPhoto(id, albumDir, index)
			if !ok {
				failMu.Lock()
				failed = append(failed, albumID+"\t"+id)
				failMu.Unlock()
			}
			return ok
		}, nil)
		totalSuccess += s
		totalFail += f
	}

	// Rewrite the failure log with only the still-failed entries.
	path := c.albumFailureLogPath()
	if len(failed) == 0 {
		os.Remove(path)
	} else {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			f.WriteString("# Failed downloads (retry) - " + time.Now().Format(time.RFC3339) + "\n")
			for _, line := range failed {
				f.WriteString(line + "\n")
			}
			f.Close()
		}
	}
	return totalSuccess, totalFail
}

// ignore set

func (c *Client) ignoreSet() map[string]bool {
	set := map[string]bool{}
	for _, name := range c.IgnoreAlbums {
		set[name] = true
	}
	if data, err := os.ReadFile(filepath.Join(c.OutputDir, "ignore_albums.txt")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				set[line] = true
			}
		}
	}
	return set
}

func (c *Client) shouldIgnoreAlbum(name string) bool {
	set := c.ignoreSet()
	return set[name] || set[Sanitize(name)]
}
