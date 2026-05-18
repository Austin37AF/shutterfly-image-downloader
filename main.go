package main

import (
	"bufio"
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
	"regexp"
	"strings"
	"time"
)

type Album struct {
	ID         string
	Name       string
	PhotoCount int
}

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
		req.Header.Set("Origin", "https://photos.shutterfly.com")
		req.Header.Set("Referer", "https://photos.shutterfly.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15")
		req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")

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
	if uid, ok := d.claims["sfly_uid"].(string); ok {
		return uid
	}
	return ""
}

func (d *Downloader) GetAlbums() ([]Album, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"method":  "album.getAlbums",
		"params":  []interface{}{d.token, d.userID(), nil, nil, true},
		"headers": map[string]string{"X-SFLY-SubSource": "library"},
		"id":      nil,
	})

	resp, err := d.request("POST", "https://cmd.thislife.com/json?method=album.getAlbums", strings.NewReader(string(payload)))
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
	json.NewDecoder(resp.Body).Decode(&result)

	if !result.Result.Success {
		return nil, fmt.Errorf("failed: %s", result.Result.Message)
	}

	var albums []Album
	if len(result.Result.Payload) > 0 {
		for _, p := range result.Result.Payload[0] {
			albums = append(albums, Album{p.Story.UID, p.Story.Name, p.Story.VisibleMomentCount})
		}
	}
	return albums, nil
}

func (d *Downloader) GetMomentIDs(albumID string) ([]string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"method":  "album.getAlbum",
		"params":  []interface{}{d.token, albumID, "startupItem", nil, false, true, true},
		"headers": map[string]string{"X-SFLY-SubSource": "albums"},
		"id":      nil,
	})

	resp, err := d.request("POST", "https://cmd.thislife.com/json?method=album.getAlbum", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			Success bool `json:"success"`
			Payload struct {
				Moments string `json:"moments"`
			} `json:"payload"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if !result.Result.Success {
		return nil, fmt.Errorf("failed to get album")
	}

	var ids []string
	moments := result.Result.Payload.Moments
	for i := 0; i+277 <= len(moments); i += 277 {
		if id := strings.TrimLeft(moments[i+9:i+25], "0"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func sanitize(name string) string {
	return strings.TrimSpace(regexp.MustCompile(`[^a-zA-Z0-9 \-_]`).ReplaceAllString(name, ""))
}

func (d *Downloader) DownloadPhoto(momentID, albumDir string, index int) bool {
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

	fpath := filepath.Join(albumDir, filename)

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

	fmt.Printf("Downloaded: %s\n", filename)
	return true
}

func (d *Downloader) DownloadAlbum(album Album) (int, int) {
	fmt.Printf("\n=== %s (%d photos) ===\n", album.Name, album.PhotoCount)

	albumDir := filepath.Join(d.outputDir, sanitize(album.Name))
	os.MkdirAll(albumDir, 0755)

	ids, err := d.GetMomentIDs(album.ID)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}

	success, fail := 0, 0
	for i, id := range ids {
		if d.DownloadPhoto(id, albumDir, i+1) {
			success++
		} else {
			fail++
		}
		time.Sleep(d.rateLimit)
	}
	return success, fail
}

func (d *Downloader) DownloadAll() (int, int) {
	albums, err := d.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}

	fmt.Printf("Found %d albums\n", len(albums))
	totalSuccess, totalFail := 0, 0

	for _, album := range albums {
		s, f := d.DownloadAlbum(album)
		totalSuccess += s
		totalFail += f
	}
	return totalSuccess, totalFail
}

func (d *Downloader) Count() {
	albums, err := d.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	total := 0
	fmt.Printf("\n%d albums:\n", len(albums))
	for _, a := range albums {
		fmt.Printf("  - %s: %d\n", a.Name, a.PhotoCount)
		total += a.PhotoCount
	}
	fmt.Printf("\nTotal: %d photos\n", total)
}

func main() {
	token := flag.String("token", "", "Shutterfly access token (or set SHUTTERFLY_TOKEN)")
	outputDir := flag.String("output", "shutterfly_photos", "Output directory")
	rateLimit := flag.Float64("rate-limit", 0.1, "Delay between requests (seconds)")
	countOnly := flag.Bool("count", false, "Only count albums/photos")
	verbose := flag.Bool("verbose", false, "Debug logging")
	flag.Parse()

	accessToken := *token
	if accessToken == "" {
		accessToken = os.Getenv("SHUTTERFLY_TOKEN")
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
		success, fail := d.DownloadAll()
		fmt.Printf("\nComplete! Success: %d, Failed: %d\n", success, fail)
	}
}
