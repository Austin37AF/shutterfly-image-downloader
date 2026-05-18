//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run verify.go <token> <library-id> <output-dir>")
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
	fmt.Printf("API reports %d moments\n\n", len(momentIDs))

	// Count local files
	var localFiles []string
	filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".heic" || ext == ".mp4" || ext == ".mov" {
			localFiles = append(localFiles, filepath.Base(path))
		}
		return nil
	})
	fmt.Printf("Local files: %d\n", len(localFiles))

	diff := len(momentIDs) - len(localFiles)
	fmt.Printf("Difference: %d\n\n", diff)

	if diff > 0 {
		fmt.Println("Possible reasons for difference:")
		fmt.Println("  - Same photo in multiple albums (API counts each reference)")
		fmt.Println("  - Failed downloads (check failed_downloads.txt)")
		fmt.Println("  - Videos that failed to download")
		fmt.Println("  - Deleted photos still in API cache")
	} else if diff < 0 {
		fmt.Println("More local files than API moments - likely renamed/edited files")
	} else {
		fmt.Println("✓ Counts match!")
	}

	// Show file type breakdown
	fmt.Println("\nLocal file types:")
	types := make(map[string]int)
	for _, f := range localFiles {
		ext := strings.ToLower(filepath.Ext(f))
		types[ext]++
	}
	for ext, count := range types {
		fmt.Printf("  %s: %d\n", ext, count)
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
