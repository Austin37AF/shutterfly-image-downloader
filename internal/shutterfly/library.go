package shutterfly

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// Library mode (photos-api3 searchMoments) — the original tool's behavior.

func (c *Client) GetAllMomentIDs() ([]string, error) {
	uid := c.userID()
	c.log("User ID: %s", uid)

	libraryID := os.Getenv("SHUTTERFLY_LIBRARY_ID")
	if libraryID == "" {
		libraryID = uid
	}
	c.log("Library ID: %s", libraryID)

	buildBody := func(token string) io.Reader {
		return jsonBody(map[string]interface{}{
			"method": "searchMoments",
			"params": []interface{}{
				token,
				libraryID,
				true,
				[]interface{}{},
				[]interface{}{},
				[]interface{}{},
				[]interface{}{},
				[]interface{}{},
				false,
				[]interface{}{},
				[]interface{}{},
				nil,
				"",
				nil, nil, nil, nil, nil, nil, nil,
				true,
			},
			"headers": map[string]string{"X-SFLY-SubSource": "library"},
			"id":      nil,
		})
	}

	resp, err := c.libraryCall("POST", c.LibraryBase+"/photos/json?method=searchMoments", buildBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
			Payload struct {
				NumberMatches int      `json:"number_matches"`
				Moments       []string `json:"moments"`
			} `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.Result.Success {
		return nil, fmt.Errorf("failed: %s", result.Result.Message)
	}

	c.log("Found %d moments", result.Result.Payload.NumberMatches)
	return result.Result.Payload.Moments, nil
}

// DownloadPhoto downloads one library photo into outputDir, writing distinct
// same-name content into outputDir/Duplicates/.
func (c *Client) DownloadPhoto(momentID, outputDir string, index int) bool {
	resp, err := c.request(apiCall{
		profile: libraryProfile,
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

	fpath := filepath.Join(outputDir, filename)

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", momentID, err)
		return false
	}

	if existingData, err := os.ReadFile(fpath); err == nil {
		if len(existingData) == len(data) {
			if sha256.Sum256(existingData) == sha256.Sum256(data) {
				fmt.Printf("Skipping (identical): %s\n", filename)
				return true
			}
		}
		dupDir := filepath.Join(outputDir, "Duplicates")
		os.MkdirAll(dupDir, 0755)
		ext := filepath.Ext(filename)
		base := strings.TrimSuffix(filename, ext)
		newFilename := fmt.Sprintf("%s_%s%s", base, momentID, ext)
		fpath = filepath.Join(dupDir, newFilename)

		if _, err := os.Stat(fpath); err == nil {
			fmt.Printf("Skipping (exists in Duplicates): %s\n", newFilename)
			return true
		}
		fmt.Printf("Different content, saving to Duplicates: %s\n", newFilename)
	}

	if err := os.WriteFile(fpath, data, 0644); err != nil {
		fmt.Printf("Error writing %s: %v\n", fpath, err)
		return false
	}

	if originalTime := extractExifDate(data); !originalTime.IsZero() {
		os.Chtimes(fpath, originalTime, originalTime)
	}

	fmt.Printf("Downloaded: %s\n", filename)
	return true
}

func extractExifDate(data []byte) time.Time {
	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return time.Time{}
	}
	t, err := x.DateTime()
	if err != nil {
		return time.Time{}
	}
	return t
}

func (c *Client) DownloadAll(workers int) (int, int) {
	momentIDs, err := c.GetAllMomentIDs()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}

	fmt.Printf("Found %d photos to download (using %d workers)\n", len(momentIDs), workers)

	failLog, err := os.OpenFile(filepath.Join(c.OutputDir, "failed_downloads.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Printf("Warning: couldn't create failure log: %v\n", err)
	} else {
		defer failLog.Close()
		failLog.WriteString(fmt.Sprintf("# Failed downloads - %s\n", time.Now().Format(time.RFC3339)))
	}

	return c.runWorkers(momentIDs, workers, func(id string, index int) bool {
		return c.DownloadPhoto(id, c.OutputDir, index)
	}, failLog)
}

// runWorkers downloads ids with the given per-item function, logging failures
// (bare IDs) to failLog.
func (c *Client) runWorkers(ids []string, workers int, download func(id string, index int) bool, failLog *os.File) (int, int) {
	type result struct {
		id      string
		success bool
	}
	type job struct {
		id    string
		index int
	}

	jobs := make(chan job, len(ids))
	results := make(chan result, len(ids))

	for w := 0; w < workers; w++ {
		go func() {
			for j := range jobs {
				results <- result{j.id, download(j.id, j.index)}
				time.Sleep(c.RateLimit)
			}
		}()
	}

	for i, id := range ids {
		jobs <- job{id, i + 1}
	}
	close(jobs)

	success, fail := 0, 0
	for i := 0; i < len(ids); i++ {
		r := <-results
		if r.success {
			success++
		} else {
			fail++
			if failLog != nil {
				failLog.WriteString(r.id + "\n")
			}
		}
		if (i+1)%100 == 0 {
			fmt.Printf("Progress: %d/%d (success: %d, fail: %d)\n", i+1, len(ids), success, fail)
		}
	}
	return success, fail
}

func (c *Client) RetryFailed(workers int) (int, int) {
	failPath := filepath.Join(c.OutputDir, "failed_downloads.txt")
	data, err := os.ReadFile(failPath)
	if err != nil {
		fmt.Printf("No failed_downloads.txt found: %v\n", err)
		return 0, 0
	}

	var momentIDs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			momentIDs = append(momentIDs, line)
		}
	}

	if len(momentIDs) == 0 {
		fmt.Println("No failed downloads to retry")
		return 0, 0
	}

	fmt.Printf("Retrying %d failed downloads (using %d workers)\n", len(momentIDs), workers)

	failLog, _ := os.OpenFile(failPath+".new", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if failLog != nil {
		defer failLog.Close()
		failLog.WriteString(fmt.Sprintf("# Failed downloads (retry) - %s\n", time.Now().Format(time.RFC3339)))
	}

	success, fail := c.runWorkers(momentIDs, workers, func(id string, index int) bool {
		return c.DownloadPhoto(id, c.OutputDir, index)
	}, failLog)

	if fail > 0 {
		os.Rename(failPath+".new", failPath)
	} else {
		os.Remove(failPath + ".new")
		os.Remove(failPath)
	}

	return success, fail
}

func (c *Client) Count() {
	momentIDs, err := c.GetAllMomentIDs()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	fmt.Printf("\nTotal: %d photos\n", len(momentIDs))
}
