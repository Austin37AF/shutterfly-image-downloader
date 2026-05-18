//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: go run check_dupes.go <token> <library-id>")
		os.Exit(1)
	}

	token := os.Args[1]
	libraryID := os.Args[2]

	fmt.Println("Fetching moment IDs from API...")
	momentIDs, err := getAllMomentIDs(token, libraryID)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Total moment IDs returned: %d\n", len(momentIDs))

	// Check for duplicates
	seen := make(map[string]int)
	for _, id := range momentIDs {
		seen[id]++
	}

	fmt.Printf("Unique moment IDs: %d\n", len(seen))
	fmt.Printf("Duplicate references: %d\n", len(momentIDs)-len(seen))

	// Show most duplicated
	type dup struct {
		id    string
		count int
	}
	var dups []dup
	for id, count := range seen {
		if count > 1 {
			dups = append(dups, dup{id, count})
		}
	}
	
	if len(dups) > 0 {
		fmt.Printf("\nMoments appearing multiple times: %d\n", len(dups))
		// Show top 5
		fmt.Println("Top duplicates:")
		for i, d := range dups {
			if i >= 5 {
				break
			}
			fmt.Printf("  %s: %d times\n", d.id, d.count)
		}
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
