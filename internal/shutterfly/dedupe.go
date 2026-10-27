package shutterfly

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dedupe — ported from the Python dedupe_album/dedupe_all.

// FindSimilar returns files in dir that look like duplicates of filename:
// same extension (case-insensitive) and base equal to the original base or
// original_<digits>.
func FindSimilar(dir, filename string) []string {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	original := base
	if i := strings.LastIndex(base, "_"); i >= 0 && i+1 < len(base) {
		if isDigits(base[i+1:]) {
			original = base[:i]
		}
	}

	var similar []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return similar
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fExt := filepath.Ext(e.Name())
		if !strings.EqualFold(fExt, ext) {
			continue
		}
		fBase := strings.TrimSuffix(e.Name(), fExt)
		if fBase == original {
			similar = append(similar, e.Name())
			continue
		}
		if i := strings.LastIndex(fBase, "_"); i >= 0 && fBase[:i] == original && isDigits(fBase[i+1:]) {
			similar = append(similar, e.Name())
		}
	}
	sort.Strings(similar)
	return similar
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// DedupeAlbum removes identical same-name files from albumDir, keeping the
// shorter name. serverCount == nil means "no server data" → always checked.
// Returns the number of files removed (or that would be, with dryRun).
func DedupeAlbum(albumDir string, thorough bool, serverCount *int, dryRun bool) int {
	info, err := os.Stat(albumDir)
	if err != nil || !info.IsDir() {
		return 0
	}

	localCount := countRegularFiles(albumDir)

	if !thorough && serverCount != nil && localCount <= *serverCount {
		fmt.Printf("Skipping %s: has %d files, should have %d\n", filepath.Base(albumDir), localCount, *serverCount)
		return 0
	}
	if serverCount != nil {
		fmt.Printf("Checking %s: has %d files, should have %d\n", filepath.Base(albumDir), localCount, *serverCount)
	} else {
		fmt.Printf("Checking %s: has %d files\n", filepath.Base(albumDir), localCount)
	}

	processed := map[string]bool{}
	removed := 0

	entries, err := os.ReadDir(albumDir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || processed[e.Name()] {
			continue
		}
		similar := FindSimilar(albumDir, e.Name())
		if len(similar) <= 1 {
			processed[e.Name()] = true
			continue
		}
		remove := map[string]bool{}
		for i := 0; i < len(similar); i++ {
			for j := i + 1; j < len(similar); j++ {
				f1 := filepath.Join(albumDir, similar[i])
				f2 := filepath.Join(albumDir, similar[j])
				if remove[similar[j]] {
					continue
				}
				if Identical(f1, f2) {
					keep, drop := similar[i], similar[j]
					if len(similar[j]) < len(similar[i]) {
						keep, drop = similar[j], similar[i]
					}
					if remove[drop] {
						continue
					}
					if dryRun {
						fmt.Printf("Would remove duplicate: %s (identical to %s)\n", drop, keep)
					} else {
						fmt.Printf("Removing duplicate: %s (identical to %s)\n", drop, keep)
					}
					remove[drop] = true
				} else {
					fmt.Printf("Keeping both %s and %s (different content)\n", similar[i], similar[j])
				}
			}
		}
		for name := range remove {
			if !dryRun {
				os.Remove(filepath.Join(albumDir, name))
			}
			removed++
		}
		for _, name := range similar {
			processed[name] = true
		}
	}

	if removed > 0 {
		verb := "Removed"
		if dryRun {
			verb = "Would remove"
		}
		newCount := localCount
		if !dryRun {
			newCount = countRegularFiles(albumDir)
		}
		fmt.Printf("\n%s %d duplicates from %s\n", verb, removed, filepath.Base(albumDir))
		fmt.Printf("Album now has %d files\n", newCount)
		if serverCount != nil {
			switch {
			case newCount < *serverCount:
				fmt.Printf("Warning: Album still missing %d photos\n", *serverCount-newCount)
			case newCount > *serverCount:
				fmt.Printf("Warning: Album has %d extra photos\n", newCount-*serverCount)
			default:
				fmt.Println("Album now has the correct number of photos")
			}
		}
	}
	return removed
}

func countRegularFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

// DedupeAll runs DedupeAlbum across all subdirectories. getAlbums is fetched
// ONCE (the Python fetches per album); a nil/failed fetch runs with no server
// data, as if every album were thorough-eligible.
func (c *Client) DedupeAll(thorough, dryRun bool) int {
	fmt.Println("\nChecking for duplicates...")
	if thorough {
		fmt.Println("Running in thorough mode: checking all albums")
	} else {
		fmt.Println("Running in quick mode: only checking albums that might have duplicates")
	}

	var serverCounts map[string]int
	if c.currentToken() != "" {
		if albums, err := c.GetAlbums(); err == nil {
			serverCounts = map[string]int{}
			for _, a := range albums {
				serverCounts[Sanitize(a.Name)] = a.PhotoCount
			}
		}
	}

	totalRemoved, processed, withDupes, skipped := 0, 0, 0, 0

	entries, err := os.ReadDir(c.OutputDir)
	if err != nil {
		fmt.Printf("Error reading output dir: %v\n", err)
		return 0
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	for _, name := range dirs {
		dir := filepath.Join(c.OutputDir, name)
		var serverCount *int
		if serverCounts != nil {
			if n, ok := serverCounts[name]; ok {
				serverCount = &n
			}
		}
		removed := DedupeAlbum(dir, thorough, serverCount, dryRun)
		if removed > 0 {
			withDupes++
			totalRemoved += removed
		} else if !thorough {
			skipped++
		}
		processed++
	}

	fmt.Println("\nDeduplication complete!")
	if !thorough {
		fmt.Printf("Skipped %d albums with correct or fewer files than expected\n", skipped)
	}
	if totalRemoved > 0 {
		verb := "Removed"
		if dryRun {
			verb = "Would remove"
		}
		fmt.Printf("Found duplicates in %d out of %d checked albums\n", withDupes, processed-skipped)
		fmt.Printf("%s %d duplicate files\n", verb, totalRemoved)
	} else {
		fmt.Println("No exact duplicates found")
	}
	return totalRemoved
}
