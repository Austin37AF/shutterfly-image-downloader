package shutterfly

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// localAlbumCounts counts regular, non-dot files in each subdirectory of
// outputDir.
func localAlbumCounts(outputDir string) map[string]int {
	counts := map[string]int{}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return counts
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(outputDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		n := 0
		for _, f := range files {
			if f.Type().IsRegular() && !strings.HasPrefix(f.Name(), ".") {
				n++
			}
		}
		counts[e.Name()] = n
	}
	return counts
}

// Comparison is the factored-out result of comparing server albums against
// local directory counts (names keyed by sanitized album name).
type Comparison struct {
	Common     []CommonRow      // albums present on both, any count status
	Missing    []Album          // on server, no local dir
	Extra      []string         // local dir names with no server album
	Incomplete []IncompleteItem // local < server or missing entirely
}

type CommonRow struct {
	Name        string // sanitized
	ServerCount int
	LocalCount  int
}

type IncompleteItem struct {
	Album   Album
	Missing int // server - local (== server count for missing albums)
}

// ComputeComparison diffs server albums (keyed by sanitized name) against
// local counts. It does not apply ignores — callers filter Incomplete when
// acting on it.
func ComputeComparison(server []Album, local map[string]int) *Comparison {
	serverData := map[string]Album{}
	for _, a := range server {
		serverData[Sanitize(a.Name)] = a
	}

	cmp := &Comparison{}
	for sname, a := range serverData {
		lc, ok := local[sname]
		if !ok {
			cmp.Missing = append(cmp.Missing, a)
			cmp.Incomplete = append(cmp.Incomplete, IncompleteItem{a, a.PhotoCount})
			continue
		}
		cmp.Common = append(cmp.Common, CommonRow{sname, a.PhotoCount, lc})
		if lc != a.PhotoCount {
			cmp.Incomplete = append(cmp.Incomplete, IncompleteItem{a, a.PhotoCount - lc})
		}
	}
	for lname := range local {
		if _, ok := serverData[lname]; !ok {
			cmp.Extra = append(cmp.Extra, lname)
		}
	}
	sort.Slice(cmp.Common, func(i, j int) bool { return cmp.Common[i].Name < cmp.Common[j].Name })
	sort.Slice(cmp.Missing, func(i, j int) bool { return Sanitize(cmp.Missing[i].Name) < Sanitize(cmp.Missing[j].Name) })
	sort.Strings(cmp.Extra)
	sort.Slice(cmp.Incomplete, func(i, j int) bool {
		return Sanitize(cmp.Incomplete[i].Album.Name) < Sanitize(cmp.Incomplete[j].Album.Name)
	})
	return cmp
}

// PlanRedownload returns the albums fix-incomplete will fetch: local < server
// for common albums, or missing entirely; ignores filtered; sorted by photo
// count. (Stricter than Compare, which flags any count mismatch.)
func PlanRedownload(server []Album, local map[string]int, ignore func(name string) bool) []IncompleteItem {
	serverData := map[string]Album{}
	for _, a := range server {
		serverData[Sanitize(a.Name)] = a
	}
	var plan []IncompleteItem
	for sname, a := range serverData {
		lc, ok := local[sname]
		if !ok {
			if !ignore(a.Name) {
				plan = append(plan, IncompleteItem{a, a.PhotoCount})
			}
			continue
		}
		if lc < a.PhotoCount && !ignore(a.Name) {
			plan = append(plan, IncompleteItem{a, a.PhotoCount - lc})
		}
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].Album.PhotoCount < plan[j].Album.PhotoCount })
	return plan
}

// CompareLocalVsServer prints the Python compare_local_vs_server table.
func (c *Client) CompareLocalVsServer() {
	fmt.Println("\nComparing local downloads with server data...")
	albums, err := c.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	local := localAlbumCounts(c.OutputDir)
	cmp := ComputeComparison(albums, local)

	fmt.Println("\nComparison Results:")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("%-40s %-8s %-8s %s\n", "Album Name", "Server", "Local", "Status")
	fmt.Println(strings.Repeat("-", 60))

	for _, row := range cmp.Common {
		status := "✓"
		if row.ServerCount != row.LocalCount {
			status = fmt.Sprintf("Missing %d", row.ServerCount-row.LocalCount)
		}
		name := row.Name
		if len(name) > 39 {
			name = name[:39]
		}
		fmt.Printf("%-40s %-8d %-8d %s\n", name, row.ServerCount, row.LocalCount, status)
	}
	for _, a := range cmp.Missing {
		name := Sanitize(a.Name)
		if len(name) > 39 {
			name = name[:39]
		}
		fmt.Printf("%-40s %-8d %-8s %s\n", name, a.PhotoCount, "0", "Not downloaded")
	}
	for _, name := range cmp.Extra {
		if len(name) > 39 {
			name = name[:39]
		}
		fmt.Printf("%-40s %-8s %-8d %s\n", name, "?", local[name], "Local only")
	}

	serverTotal, localTotal, incompletePhotos := 0, 0, 0
	for _, a := range albums {
		serverTotal += a.PhotoCount
	}
	for _, n := range local {
		localTotal += n
	}
	for _, item := range cmp.Incomplete {
		incompletePhotos += item.Album.PhotoCount
	}

	fmt.Println("\nSummary:")
	fmt.Printf("Total albums on server: %d\n", len(albums))
	fmt.Printf("Total albums locally: %d\n", len(local))
	fmt.Printf("Missing albums: %d\n", len(cmp.Missing))
	fmt.Printf("Extra local albums: %d\n", len(cmp.Extra))
	fmt.Printf("Incomplete albums: %d (%d photos total)\n", len(cmp.Incomplete), incompletePhotos)
	fmt.Printf("\nTotal photos on server: %d\n", serverTotal)
	fmt.Printf("Total photos locally: %d\n", localTotal)
	fmt.Printf("Difference: %d\n", serverTotal-localTotal)

	if len(cmp.Incomplete) > 0 {
		fmt.Println("\nAlbums needing redownload:")
		for _, item := range cmp.Incomplete {
			fmt.Printf("- %s (%d photos)\n", Sanitize(item.Album.Name), item.Album.PhotoCount)
		}
		fmt.Printf("\nUse -fix-incomplete to redownload these %d albums\n", len(cmp.Incomplete))
	}
}

// RedownloadIncompleteAlbums ports the Python redownload_incomplete_albums.
func (c *Client) RedownloadIncompleteAlbums(workers int) (int, int) {
	fmt.Println("\nChecking for incomplete albums...")
	albums, err := c.GetAlbums()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return 0, 0
	}
	local := localAlbumCounts(c.OutputDir)
	plan := PlanRedownload(albums, local, c.shouldIgnoreAlbum)

	if len(plan) > 0 {
		fmt.Println("\nWill download albums in this order:")
		for _, item := range plan {
			fmt.Printf("- %s (%d photos, %d missing)\n", item.Album.Name, item.Album.PhotoCount, item.Missing)
		}
	}

	totalSuccess, totalFail := 0, 0
	var failed []string
	for _, item := range plan {
		fmt.Printf("\nDownloading %s: %d photos\n", item.Album.Name, item.Album.PhotoCount)
		s, f, failedIDs := c.DownloadAlbum(item.Album, workers)
		totalSuccess += s
		totalFail += f
		failed = append(failed, failedIDs...)
	}
	c.writeFailedLog(failed, "# Failed downloads (fix-incomplete)")

	fmt.Printf("\nRedownload complete!\n")
	fmt.Printf("Total successfully downloaded: %d\n", totalSuccess)
	fmt.Printf("Total failed downloads: %d\n", totalFail)
	return totalSuccess, totalFail
}
