package shutterfly

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Find possible duplicates among app-created Google Photos items.
// Review-only: never deletes, moves or modifies items — only reports via a
// TSV and (unless -dry-run) gathers groups in a review album.

const dupReviewAlbumTitle = "Shutterfly - possible duplicates (review)"

// dHashThresh is the max Hamming distance between 64-bit dHashes (min over the
// 4 rotations) for two previews to count as the same photo.
const dHashThresh = 4

const dupReviewSchema = `
CREATE TABLE IF NOT EXISTS dup_review(
  media_item_id TEXT PRIMARY KEY,
  group_id INTEGER,
  added INTEGER NOT NULL DEFAULT 0);
`

var baseNameSuffix = regexp.MustCompile(`(?i)[-_ \(~]\(?\d{1,2}\)?$`)

// seqRe matches camera-sequence filenames (IMG_1515, DSC0001, GOPR1234, ...).
var seqRe = regexp.MustCompile(`(?i)^(img|dscn|dsc|_dsc|p|gopr|imag|pict)[_-]?(\d{3,})`)

// groupClass: same-name (matching base names), sequence (burst/adjacent
// frames — all camera-sequence names but not all the same number), or other.
func groupClass(items []listedItem, baseMatch bool) string {
	if baseMatch {
		return "same-name"
	}
	allSeq := true
	nums := map[string]bool{}
	for _, it := range items {
		m := seqRe.FindStringSubmatch(strings.TrimSuffix(it.FileName, filepath.Ext(it.FileName)))
		if m == nil {
			allSeq = false
			break
		}
		nums[m[2]] = true
	}
	if allSeq && len(nums) > 1 {
		return "sequence"
	}
	return "other"
}

// baseName normalizes a filename for the name-match column: lowercase, no
// extension, no trailing "-N", "_N", "(N)", "~N", " (N)".
func baseName(name string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	for {
		b := baseNameSuffix.ReplaceAllString(name, "")
		if b == name {
			return strings.ToLower(name)
		}
		name = b
	}
}

// ---------------------------------------------------------------------------
// dHash

// grayGrid converts an image to a w x h grayscale matrix by area averaging.
func grayGrid(img image.Image, w, h int) [][]int {
	b := img.Bounds()
	gw, gh := b.Dx(), b.Dy()
	grid := make([][]int, h)
	for y := 0; y < h; y++ {
		grid[y] = make([]int, w)
		for x := 0; x < w; x++ {
			var sum, n int
			y0, y1 := b.Min.Y+y*gh/h, b.Min.Y+(y+1)*gh/h
			x0, x1 := b.Min.X+x*gw/w, b.Min.X+(x+1)*gw/w
			for py := y0; py < y1; py++ {
				for px := x0; px < x1; px++ {
					r, g, bl, _ := img.At(px, py).RGBA()
					sum += int((r + g + bl) / 3 >> 8)
					n++
				}
			}
			if n > 0 {
				grid[y][x] = sum / n
			}
		}
	}
	return grid
}

func dHashGrid(g [][]int) uint64 {
	var h uint64
	hh := len(g)
	ww := len(g[0])
	for y := 0; y < hh; y++ {
		for x := 0; x < ww-1; x++ {
			h <<= 1
			if g[y][x] > g[y][x+1] {
				h |= 1
			}
		}
	}
	return h
}

// dHash computes a 64-bit difference hash on a 9x8 grid.
func dHash(img image.Image) uint64 { return dHashGrid(grayGrid(img, 9, 8)) }

// rotateGrid rotates a grid 90° clockwise.
func rotateGrid(g [][]int) [][]int {
	h, w := len(g), len(g[0])
	out := make([][]int, w)
	for y := 0; y < w; y++ {
		out[y] = make([]int, h)
		for x := 0; x < h; x++ {
			out[y][x] = g[h-1-x][y]
		}
	}
	return out
}

// minRotDistance returns the min Hamming distance between imgA's 9x8 dHash
// and imgB's dHash over B's 4 rotations. For 90°/270° the comparison grid is
// built at 8x9 and rotated, matching what dHash of a rotated image produces.
func minRotDistance(a, b image.Image) int {
	ha := dHashGrid(grayGrid(a, 9, 8))
	best := 65
	for r := 0; r < 4; r++ {
		var gb [][]int
		switch r {
		case 0:
			gb = grayGrid(b, 9, 8)
		case 1:
			gb = rotateGrid(grayGrid(b, 8, 9))
		case 2:
			gb = rotateGrid(rotateGrid(grayGrid(b, 9, 8)))
		default:
			gb = rotateGrid(rotateGrid(rotateGrid(grayGrid(b, 8, 9))))
		}
		if d := popcount(ha ^ dHashGrid(gb)); d < best {
			best = d
		}
	}
	return best
}

func popcount(v uint64) int {
	n := 0
	for v != 0 {
		v &= v - 1
		n++
	}
	return n
}

// ---------------------------------------------------------------------------
// Bucketing + verification

func sameDims(a, b listedItem) bool {
	return a.Width == b.Width && a.Height == b.Height || a.Width == b.Height && a.Height == b.Width
}

// candidatePairs buckets items by creation second and returns pairs of bucket
// positions with equal or swapped dimensions.
func candidatePairs(items []listedItem) [][2]int {
	buckets := map[int64][]int{}
	for i, it := range items {
		buckets[it.CreationTime.Unix()] = append(buckets[it.CreationTime.Unix()], i)
	}
	var pairs [][2]int
	for _, b := range buckets {
		if len(b) < 2 {
			continue
		}
		for i := 0; i < len(b); i++ {
			for j := i + 1; j < len(b); j++ {
				if sameDims(items[b[i]], items[b[j]]) {
					pairs = append(pairs, [2]int{b[i], b[j]})
				}
			}
		}
	}
	return pairs
}

// fetchPreview downloads the 256px preview, honoring 429 like g.do.
func (g *GoogleClient) fetchPreview(baseURL string) (image.Image, error) {
	for waits := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}; ; {
		resp, err := g.hc.Get(baseURL + "=w256-h256")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == 429 {
			resp.Body.Close()
			if len(waits) == 0 {
				return nil, ErrQuota
			}
			fmt.Printf("Google rate limit (429), waiting %s...\n", waits[0])
			time.Sleep(waits[0])
			waits = waits[1:]
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, fmt.Errorf("preview: %d", resp.StatusCode)
		}
		img, _, err := image.Decode(resp.Body)
		resp.Body.Close()
		return img, err
	}
}

type dupGroup struct {
	id        int
	items     []listedItem
	maxDist   int
	baseMatch bool
	class     string
	keep      string // media_item_id suggested to keep
}

// FindDuplicates runs the review-only duplicate finder.
func (c *Client) FindDuplicates(g *GoogleClient, statePath string, workers int, dryRun bool, expectedEmail string, yes bool, albumFilter string) error {
	st, err := OpenStateDB(statePath)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.db.Exec(dupReviewSchema); err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("no Google client")
	}
	if expectedEmail != "" && !strings.EqualFold(g.Email, expectedEmail) {
		return fmt.Errorf("signed in as %s, expected %s", g.Email, expectedEmail)
	}
	if stored := st.metaGet("google_account"); stored != "" && !strings.EqualFold(g.Email, stored) {
		return fmt.Errorf("signed in as %s, but this transfer belongs to %s", g.Email, stored)
	}

	fmt.Println("Listing app-created media items...")
	listed, err := g.listAllMediaItems()
	if err != nil {
		return err
	}
	items := make([]listedItem, 0, len(listed))
	for _, it := range listed {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	pairs := candidatePairs(items)
	bucketN := map[int64]bool{}
	for _, p := range pairs {
		bucketN[items[p[0]].CreationTime.Unix()] = true
	}
	fmt.Printf("%d items, %d candidate pairs in %d buckets\n", len(items), len(pairs), len(bucketN))
	if len(pairs) == 0 {
		return nil
	}

	// refresh baseUrls via batchGet for all candidate ids
	var cand []int
	seen := map[int]bool{}
	for _, p := range pairs {
		if !seen[p[0]] {
			seen[p[0]] = true
			cand = append(cand, p[0])
		}
		if !seen[p[1]] {
			seen[p[1]] = true
			cand = append(cand, p[1])
		}
	}
	fresh := map[string]string{}
	for i := 0; i < len(cand); i += 50 {
		end := i + 50
		if end > len(cand) {
			end = len(cand)
		}
		var ids []string
		for _, k := range cand[i:end] {
			ids = append(ids, items[k].ID)
		}
		got, err := g.batchGet(ids)
		if err != nil {
			return err
		}
		for id, it := range got {
			fresh[id] = it.BaseURL
		}
	}

	// fetch previews + dHash concurrently
	hashes := map[int]uint64{}
	imgs := map[int]image.Image{}
	var mu sync.Mutex
	var fetched, failed int64
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				bu := fresh[items[k].ID]
				if bu == "" {
					bu = items[k].BaseURL
				}
				if bu == "" {
					continue
				}
				img, err := g.fetchPreview(bu)
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				mu.Lock()
				imgs[k] = img
				hashes[k] = dHash(img)
				fetched++
				mu.Unlock()
			}
		}()
	}
	for _, k := range cand {
		jobs <- k
	}
	close(jobs)
	wg.Wait()
	fmt.Printf("previews: %d fetched, %d failed\n", fetched, failed)

	// match pairs, union-find for connected components within buckets
	parent := map[int]int{}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	matchDist := map[[2]int]int{}
	for _, p := range pairs {
		a, b := p[0], p[1]
		if _, ok := imgs[a]; !ok || imgs[b] == nil {
			continue
		}
		if _, ok := imgs[b]; !ok {
			continue
		}
		d := minRotDistance(imgs[a], imgs[b])
		if d <= dHashThresh {
			if parent[a] == 0 {
				parent[a] = a
			}
			if parent[b] == 0 {
				parent[b] = b
			}
			union(a, b)
			matchDist[p] = d
		}
	}
	groups := map[int][]int{}
	for p := range matchDist {
		groups[find(p[0])] = appendIfAbsent(groups[find(p[0])], p[0])
		groups[find(p[0])] = appendIfAbsent(groups[find(p[0])], p[1])
	}

	// build output groups
	var out []dupGroup
	gid := 0
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		gid++
		sort.Ints(members)
		g := dupGroup{id: gid}
		maxD := 0
		base := baseName(items[members[0]].FileName)
		sameBase := true
		var keep listedItem
		var keepPx int64 = -1
		for _, k := range members {
			it := items[k]
			g.items = append(g.items, it)
			if baseName(it.FileName) != base {
				sameBase = false
			}
			if px := it.Width * it.Height; px > keepPx {
				keepPx, keep = px, it
			}
		}
		for p, d := range matchDist {
			if inIntSlice(members, p[0]) && inIntSlice(members, p[1]) && d > maxD {
				maxD = d
			}
		}
		g.maxDist, g.baseMatch, g.keep = maxD, sameBase, keep.ID
		g.class = groupClass(g.items, sameBase)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })

	// TSV
	tsvPath := filepath.Join(filepath.Dir(statePath), "duplicates.tsv")
	f, err := os.Create(tsvPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(f, "group_id\tmedia_item_id\tfilename\tcreation_time\twidth\theight\tbase_name_match\tmax_distance_in_group\tsuggested\tclass")
	extra := 0
	noBase := 0
	var albumOverlap int
	var albumItemIDs map[string]bool
	if albumFilter != "" {
		listedAlbs, _ := g.listAlbums()
		if aid, ok := listedAlbs[albumFilter]; ok {
			albumItemIDs, _ = g.albumItems(aid)
		}
	}
	for _, g := range out {
		extra += len(g.items) - 1
		if !g.baseMatch {
			noBase++
		}
		for _, it := range g.items {
			sug := "review"
			if it.ID == g.keep {
				sug = "keep"
			}
			bm := "no"
			if g.baseMatch {
				bm = "yes"
			}
			fmt.Fprintf(f, "%d\t%s\t%s\t%s\t%d\t%d\t%s\t%d\t%s\t%s\n",
				g.id, it.ID, it.FileName, it.CreationTime.Format("2006-01-02 15:04:05"), it.Width, it.Height, bm, g.maxDist, sug, g.class)
			if albumItemIDs[it.ID] {
				albumOverlap++
			}
		}
	}
	f.Close()

	fmt.Printf("\nCandidate buckets: %d, items checked: %d\n", len(bucketN), len(cand))
	fmt.Printf("Verified groups: %d, extra copies: %d\n", len(out), extra)
	byClass := map[string][2]int{}
	for _, g := range out {
		c := byClass[g.class]
		c[0]++
		c[1] += len(g.items) - 1
		byClass[g.class] = c
	}
	for _, cl := range []string{"same-name", "other", "sequence"} {
		if c, ok := byClass[cl]; ok {
			fmt.Printf("  %-9s %d groups, %d extra copies\n", cl+":", c[0], c[1])
		}
	}
	fmt.Printf("Groups with base_name_match=no (careful, may be bursts/edits): %d\n", noBase)
	if albumItemIDs != nil {
		fmt.Printf("Items in %q overlapping verified groups: %d\n", albumFilter, albumOverlap)
	}
	fmt.Printf("TSV: %s\n", tsvPath)

	if dryRun {
		return nil
	}

	// confirm + album
	confirmed := yes && expectedEmail != ""
	if !confirmed {
		fmt.Printf("Signed in to Google as %s. Add %d possible-duplicate items to a review album? [y/N] ", g.Email, extra+len(out))
		var ans string
		fmt.Fscan(c.Stdin, &ans)
		confirmed = strings.EqualFold(strings.TrimSpace(ans), "y")
	}
	if !confirmed {
		return fmt.Errorf("aborted")
	}

	// find/create review album
	existing, err := g.listAlbums()
	if err != nil {
		return err
	}
	var revID string
	if _, ok := st.albumID(dupReviewAlbumTitle); ok {
		revID, _ = st.albumID(dupReviewAlbumTitle)
	} else if id, ok := existing[dupReviewAlbumTitle]; ok {
		revID = id
		st.setAlbumID(dupReviewAlbumTitle, id)
	} else {
		revID, err = g.createAlbum(dupReviewAlbumTitle)
		if err != nil {
			return err
		}
		st.setAlbumID(dupReviewAlbumTitle, revID)
	}

	// add only new items
	added := map[string]bool{}
	rows, _ := st.db.Query(`SELECT media_item_id FROM dup_review WHERE added=1`)
	if rows != nil {
		for rows.Next() {
			var id string
			rows.Scan(&id)
			added[id] = true
		}
		rows.Close()
	}
	var toAdd []struct {
		id string
		gi int
	}
	for _, g := range out {
		if g.class == "sequence" {
			continue // bursts/adjacent frames: TSV only, never the review album
		}
		for _, it := range g.items {
			if !added[it.ID] {
				toAdd = append(toAdd, struct {
					id string
					gi int
				}{it.ID, g.id})
			}
		}
	}
	for i := 0; i < len(toAdd); i += 50 {
		end := i + 50
		if end > len(toAdd) {
			end = len(toAdd)
		}
		var ids []string
		for _, x := range toAdd[i:end] {
			ids = append(ids, x.id)
		}
		if err := g.batchAddMediaItems(revID, ids); err != nil {
			return err
		}
		tx, _ := st.db.Begin()
		for _, x := range toAdd[i:end] {
			tx.Exec(`INSERT INTO dup_review(media_item_id,group_id,added) VALUES(?,?,1)
			  ON CONFLICT(media_item_id) DO UPDATE SET added=1`, x.id, x.gi)
		}
		tx.Commit()
	}

	fmt.Printf("\nAdded %d items to '%s'.\n", len(toAdd), dupReviewAlbumTitle)
	fmt.Println("Open the album in Google Photos — items in each group sit side by side (same date).")
	fmt.Println("Delete whatever you want manually; removing the review album later doesn't delete photos.")
	return nil
}

func appendIfAbsent(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func inIntSlice(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
