//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rwcarlsen/goexif/exif"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run fix_dates.go <directory>")
		os.Exit(1)
	}

	dir := os.Args[1]
	fixed, skipped, failed := 0, 0, 0

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		ext := filepath.Ext(path)
		if ext != ".jpg" && ext != ".jpeg" && ext != ".JPG" && ext != ".JPEG" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			failed++
			return nil
		}

		x, err := exif.Decode(bytes.NewReader(data))
		if err != nil {
			skipped++
			return nil
		}

		t, err := x.DateTime()
		if err != nil {
			skipped++
			return nil
		}

		if err := os.Chtimes(path, t, t); err != nil {
			failed++
			return nil
		}

		fixed++
		if fixed%100 == 0 {
			fmt.Printf("Fixed %d files...\n", fixed)
		}
		return nil
	})

	fmt.Printf("\nDone! Fixed: %d, Skipped (no EXIF): %d, Failed: %d\n", fixed, skipped, failed)
}
