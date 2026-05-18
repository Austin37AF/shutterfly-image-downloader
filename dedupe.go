//go:build ignore

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run dedupe.go <directory> [--dry-run]")
		os.Exit(1)
	}

	dir := os.Args[1]
	dryRun := len(os.Args) > 2 && os.Args[2] == "--dry-run"

	hashes := make(map[string]string) // hash -> first file path
	duplicates := 0
	savedBytes := int64(0)

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return nil
		}
		hash := hex.EncodeToString(h.Sum(nil))

		if original, exists := hashes[hash]; exists {
			duplicates++
			savedBytes += info.Size()
			if dryRun {
				fmt.Printf("DUPLICATE: %s\n  (same as: %s)\n", filepath.Base(path), filepath.Base(original))
			} else {
				fmt.Printf("Removing duplicate: %s\n", filepath.Base(path))
				os.Remove(path)
			}
		} else {
			hashes[hash] = path
		}
		return nil
	})

	fmt.Printf("\nFound %d duplicates (%.2f MB)\n", duplicates, float64(savedBytes)/1024/1024)
	if dryRun {
		fmt.Println("Run without --dry-run to delete them")
	} else {
		fmt.Println("Duplicates removed!")
	}
}
