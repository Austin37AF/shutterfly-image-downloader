package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)


type Downloader struct {
	token      string
	outputDir  string
	rateLimit  time.Duration
	maxRetries int
	verbose    bool
	client     *http.Client
	expiry     time.Time
	claims     map[string]interface{}
}

func NewDownloader(token, outputDir string, rateLimit float64, verbose bool) *Downloader {
	d := &Downloader{
		token:      token,
		outputDir:  outputDir,
		rateLimit:  time.Duration(rateLimit * float64(time.Second)),
		maxRetries: 3,
		verbose:    verbose,
		client:     &http.Client{Timeout: 60 * time.Second},
	}
	d.claims, d.expiry = parseToken(token)
	os.MkdirAll(outputDir, 0755)
	return d
}

func (d *Downloader) log(format string, args ...interface{}) {
	if d.verbose {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func parseToken(token string) (map[string]interface{}, time.Time) {
	claims := make(map[string]interface{})
	defaultExpiry := time.Now().Add(time.Hour)

	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		payload := parts[1]
		if m := len(payload) % 4; m != 0 {
			payload += strings.Repeat("=", 4-m)
		}
		for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.StdEncoding} {
			if decoded, err := enc.DecodeString(payload); err == nil {
				if json.Unmarshal(decoded, &claims) == nil {
					if exp, ok := claims["exp"].(float64); ok {
						return claims, time.Unix(int64(exp), 0)
					}
				}
			}
		}
	}

	if decoded, err := url.QueryUnescape(token); err == nil {
		if m := len(decoded) % 4; m != 0 {
			decoded += strings.Repeat("=", 4-m)
		}
		if raw, err := base64.StdEncoding.DecodeString(decoded); err == nil {
			if json.Unmarshal(raw, &claims) == nil {
				if exp, ok := claims["exp"].(float64); ok {
					return claims, time.Unix(int64(exp), 0)
				}
				return claims, defaultExpiry
			}
		}
	}

	return claims, defaultExpiry
}

func (d *Downloader) request(method, url string, body io.Reader) (*http.Response, error) {
	for retry := 0; retry <= d.maxRetries; retry++ {
		if time.Now().After(d.expiry) {
			return nil, fmt.Errorf("token expired, please restart with a new token")
		}

		if retry > 0 {
			wait := time.Duration(1<<uint(retry))*time.Second + time.Duration(rand.Float64()*500)*time.Millisecond
			fmt.Printf("Retrying in %.1fs...\n", wait.Seconds())
			time.Sleep(wait)
		}

		req, _ := http.NewRequest(method, url, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req.Header.Set("Origin", "https://photos3.shutterfly.com")
		req.Header.Set("Referer", "https://photos3.shutterfly.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Authorization", "Bearer "+d.token)
		req.Header.Set("x-api-key", "zlrHU7h6cTGTApk3IbOU0aVuxlEJkPeA")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-site")

		d.log("Request: %s %s", method, url)
		resp, err := d.client.Do(req)
		if err != nil {
			fmt.Printf("Request failed: %v\n", err)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		resp.Body.Close()
		fmt.Printf("Request failed: %d\n", resp.StatusCode)
	}
	return nil, fmt.Errorf("max retries exceeded")
}

func (d *Downloader) userID() string {
	if uid := os.Getenv("LIFE_UID"); uid != "" {
		return uid
	}
	// Try sfly3_uid first (newer format without leading zeros)
	if uid, ok := d.claims["sfly3_uid"].(string); ok {
		d.log("Using sfly3_uid: %s", uid)
		return uid
	}
	if uid, ok := d.claims["sfly_uid"].(string); ok {
		d.log("Using sfly_uid: %s", uid)
		return uid
	}
	return ""
}

func (d *Downloader) GetAllMomentIDs() ([]string, error) {
	uid := d.userID()
	d.log("User ID: %s", uid)
	
	// Use searchMoments - the second param is a library ID
	libraryID := os.Getenv("SHUTTERFLY_LIBRARY_ID")
	if libraryID == "" {
		libraryID = uid
	}
	d.log("Library ID: %s", libraryID)
	
	payload, _ := json.Marshal(map[string]interface{}{
		"method": "searchMoments",
		"params": []interface{}{
			d.token,
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

	resp, err := d.request("POST", "https://photos-api3.shutterfly.com/photos/json?method=searchMoments", strings.NewReader(string(payload)))
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

	d.log("Found %d moments", result.Result.Payload.NumberMatches)
	return result.Result.Payload.Moments, nil
}

func (d *Downloader) DownloadPhoto(momentID, outputDir string, index int) bool {
	params := url.Values{"accessToken": {d.token}, "momentId": {momentID}, "source": {"library"}}
	resp, err := d.request("GET", "https://io.thislife.com/download?"+params.Encode(), nil)
	if err != nil {
		fmt.Printf("Error downloading %s: %v\n", momentID, err)
		return false
	}
	defer resp.Body.Close()

	filename := fmt.Sprintf("photo_%04d_%s.jpg", index, momentID)
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(cd, "filename=") {
		parts := strings.Split(cd, "filename=")
		if len(parts) > 1 {
			filename = strings.Trim(parts[1], `"' `)
		}
	}

	fpath := filepath.Join(outputDir, filename)

	// Skip if file already exists
	if _, err := os.Stat(fpath); err == nil {
		fmt.Printf("Skipping (exists): %s\n", filename)
		return true
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", momentID, err)
		return false
	}

	if err := os.WriteFile(fpath, data, 0644); err != nil {
		fmt.Printf("Error writing %s: %v\n", fpath, err)
		return false
	}

	// Set file modification time from EXIF DateTimeOriginal
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

func (d *Downloader) DownloadAll(workers int) (int, int) {
	momentIDs, err := d.GetAllMomentIDs()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}

	fmt.Printf("Found %d photos to download (using %d workers)\n", len(momentIDs), workers)
	
	type job struct {
		id    string
		index int
	}
	
	jobs := make(chan job, len(momentIDs))
	results := make(chan bool, len(momentIDs))
	
	// Start workers
	for w := 0; w < workers; w++ {
		go func() {
			for j := range jobs {
				results <- d.DownloadPhoto(j.id, d.outputDir, j.index)
				time.Sleep(d.rateLimit)
			}
		}()
	}
	
	// Send jobs
	for i, id := range momentIDs {
		jobs <- job{id, i + 1}
	}
	close(jobs)
	
	// Collect results
	success, fail := 0, 0
	for i := 0; i < len(momentIDs); i++ {
		if <-results {
			success++
		} else {
			fail++
		}
		if (i+1)%100 == 0 {
			fmt.Printf("Progress: %d/%d (success: %d, fail: %d)\n", i+1, len(momentIDs), success, fail)
		}
	}
	return success, fail
}

func (d *Downloader) Count() {
	momentIDs, err := d.GetAllMomentIDs()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	fmt.Printf("\nTotal: %d photos\n", len(momentIDs))
}

func main() {
	token := flag.String("token", "", "Shutterfly access token (or set SHUTTERFLY_TOKEN)")
	libraryID := flag.String("library-id", "", "Shutterfly library ID (or set SHUTTERFLY_LIBRARY_ID)")
	outputDir := flag.String("output", "shutterfly_photos", "Output directory")
	rateLimit := flag.Float64("rate-limit", 0.05, "Delay between requests per worker (seconds)")
	workers := flag.Int("workers", 10, "Number of parallel download workers")
	countOnly := flag.Bool("count", false, "Only count albums/photos")
	verbose := flag.Bool("verbose", false, "Debug logging")
	flag.Parse()

	accessToken := *token
	if accessToken == "" {
		accessToken = os.Getenv("SHUTTERFLY_TOKEN")
	}
	
	libID := *libraryID
	if libID == "" {
		libID = os.Getenv("SHUTTERFLY_LIBRARY_ID")
	}
	if libID != "" {
		os.Setenv("SHUTTERFLY_LIBRARY_ID", libID)
	}
	if accessToken == "" {
		fmt.Print("Enter token: ")
		reader := bufio.NewReader(os.Stdin)
		accessToken, _ = reader.ReadString('\n')
		accessToken = strings.TrimSpace(accessToken)
	}
	if accessToken == "" {
		fmt.Println("Error: token required")
		os.Exit(1)
	}

	d := NewDownloader(accessToken, *outputDir, *rateLimit, *verbose)

	if *countOnly {
		d.Count()
	} else {
		fmt.Println("Starting download...")
		success, fail := d.DownloadAll(*workers)
		fmt.Printf("\nComplete! Success: %d, Failed: %d\n", success, fail)
	}
}
