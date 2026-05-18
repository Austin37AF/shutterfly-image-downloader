//go:build ignore

package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run find_missing.go <token> <library-id> <output-dir>")
		os.Exit(1)
	}

	token := os.Args[1]
	libraryID := os.Args[2]
	outputDir := os.Args[3]

	// Get all moment IDs from API
	fmt.Println("Fetching moment IDs from API...")
	momentIDs, err := getAllMomentIDs(token, libraryID)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("API reports %d moments\n", len(momentIDs))

	// Build set of downloaded moment IDs from filenames
	downloaded := make(map[string]bool)
	filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Extract moment ID from filename patterns:
		// - photo_XXXX_MOMENTID.jpg
		// - MOMENTID.jpg (UUID format)
		// - IMG_XXXX.jpg (no moment ID)
		name := info.Name()
		
		// Check for photo_XXXX_MOMENTID.jpg pattern
		if strings.HasPrefix(name, "photo_") {
			parts := strings.Split(strings.TrimSuffix(name, filepath.Ext(name)), "_")
			if len(parts) >= 3 {
				downloaded[parts[2]] = true
			}
		}
		return nil
	})
	fmt.Printf("Found %d downloaded with moment IDs in filename\n", len(downloaded))

	// Find missing
	var missing []string
	for _, id := range momentIDs {
		if !downloaded[id] {
			missing = append(missing, id)
		}
	}

	fmt.Printf("\nMissing: %d moments\n", len(missing))

	if len(missing) > 0 {
		// Write missing IDs to file
		f, _ := os.Create(filepath.Join(outputDir, "missing_moments.txt"))
		defer f.Close()
		f.WriteString(fmt.Sprintf("# Missing moments - %s\n", time.Now().Format(time.RFC3339)))
		for _, id := range missing {
			f.WriteString(id + "\n")
		}
		fmt.Printf("Written to: %s/missing_moments.txt\n", outputDir)
	}
}

func getAllMomentIDs(token, libraryID string) ([]string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"method": "searchMoments",
		"params": []interface{}{
			token, libraryID, true,
			[]interface{}{}, []interface{}{}, []interface{}{}, []interface{}{}, []interface{}{},
			false, []interface{}{}, []interface{}{}, nil, "",
			nil, nil, nil, nil, nil, nil, nil, true,
		},
		"headers": map[string]string{"X-SFLY-SubSource": "library"},
		"id":      nil,
	})

	req, _ := http.NewRequest("POST", "https://photos-api3.shutterfly.com/photos/json?method=searchMoments", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Origin", "https://photos3.shutterfly.com")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-api-key", "zlrHU7h6cTGTApk3IbOU0aVuxlEJkPeA")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			Success bool   `json:"success"`
			Payload struct {
				Moments []string `json:"moments"`
			} `json:"payload"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Result.Payload.Moments, nil
}

func parseToken(token string) map[string]interface{} {
	claims := make(map[string]interface{})
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		payload := parts[1]
		if m := len(payload) % 4; m != 0 {
			payload += strings.Repeat("=", 4-m)
		}
		if decoded, err := base64.URLEncoding.DecodeString(payload); err == nil {
			json.Unmarshal(decoded, &claims)
		}
	}
	return claims
}

// Unused but required for compilation
var _ = bufio.NewReader
var _ = io.Copy
