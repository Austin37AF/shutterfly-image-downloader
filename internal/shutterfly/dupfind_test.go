package shutterfly

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// patternImage makes an image whose dHash is deterministic per seed.
func patternImage(seed int, w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// low-frequency pattern so JPEG re-encode barely shifts the hash
			v := uint8((x/25*seed*7 + y/20*seed*11) % 256)
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func rot90(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(b.Max.Y-1-y, x, img.At(x, y))
		}
	}
	return out
}

func TestDHash(t *testing.T) {
	a := patternImage(7, 200, 100)
	if minRotDistance(a, a) != 0 {
		t.Fatal("identical")
	}
	// re-encode at different quality
	var lo bytes.Buffer
	jpeg.Encode(&lo, a, &jpeg.Options{Quality: 30})
	b, _ := jpeg.Decode(bytes.NewReader(lo.Bytes()))
	if d := minRotDistance(a, b); d > dHashThresh {
		t.Fatalf("re-encode dist %d", d)
	}
	// rotated 90
	if d := minRotDistance(a, rot90(a)); d > dHashThresh {
		t.Fatalf("rotated dist %d", d)
	}
	// different image
	c := patternImage(41, 200, 100)
	if d := minRotDistance(a, c); d <= dHashThresh {
		t.Fatalf("different dist %d", d)
	}
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"IMG_0980.JPG":          "img_0980",
		"20150917_081955-1.jpg": "20150917_081955",
		"photo_0004_m4 (2).jpg": "photo_0004_m4",
		"a~2.png":               "a",
		"b_3.jpeg":              "b",
		"plain.jpg":             "plain",
	}
	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("%s -> %q want %q", in, got, want)
		}
	}
}

func TestBucketing(t *testing.T) {
	ts := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	mk := func(id string, w, h int64) listedItem {
		return listedItem{ID: id, CreationTime: ts, Width: w, Height: h}
	}
	items := []listedItem{
		mk("a", 100, 200), mk("b", 100, 200), mk("c", 200, 100), mk("d", 300, 400),
	}
	pairs := candidatePairs(items)
	// a-b equal, a-c + b-c swapped, nothing with d
	if len(pairs) != 3 {
		t.Fatalf("%v", pairs)
	}
}

// end-to-end fake google
type dupFake struct {
	*httptest.Server
	mu       sync.Mutex
	addCalls [][]string
	albumIDs map[string]string
	previews map[string][]byte // id -> jpeg bytes
	items    []map[string]interface{}
}

func newDupFake(t *testing.T, previews map[string][]byte, items []map[string]interface{}) *dupFake {
	f := &dupFake{previews: previews, items: items, albumIDs: map[string]string{"Shutterfly": "alb-m"}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/mediaItems":
			json.NewEncoder(w).Encode(map[string]interface{}{"mediaItems": f.items})
		case r.URL.Path == "/v1/mediaItems:batchGet":
			ids := r.URL.Query()["mediaItemIds"]
			var res []map[string]interface{}
			for _, id := range ids {
				res = append(res, map[string]interface{}{"mediaItem": map[string]string{
					"id": id, "filename": id + ".jpg", "baseUrl": f.Server.URL + "/p/" + id}})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"mediaItemResults": res})
		case strings.HasPrefix(r.URL.Path, "/p/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/p/"), "=w256-h256")
			w.Write(f.previews[id])
		case r.URL.Path == "/v1/albums" && r.Method == "GET":
			var al []map[string]string
			for t, id := range f.albumIDs {
				al = append(al, map[string]string{"title": t, "id": id})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"albums": al})
		case r.URL.Path == "/v1/albums" && r.Method == "POST":
			var req struct{ Album struct{ Title string } }
			json.NewDecoder(r.Body).Decode(&req)
			f.albumIDs[req.Album.Title] = "alb-" + req.Album.Title
			json.NewEncoder(w).Encode(map[string]string{"id": f.albumIDs[req.Album.Title]})
		case r.URL.Path == "/v1/mediaItems:search":
			var req struct {
				AlbumID string `json:"albumId"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var ms []map[string]string
			if req.AlbumID == "alb-hawaii" {
				ms = append(ms, map[string]string{"id": "i1"})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"mediaItems": ms})
		case strings.HasPrefix(r.URL.Path, "/v1/albums/"):
			var req struct{ MediaItemIds []string }
			json.NewDecoder(r.Body).Decode(&req)
			f.addCalls = append(f.addCalls, req.MediaItemIds)
			json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

func jpegBytes(t *testing.T, img image.Image) []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func TestFindDuplicatesE2E(t *testing.T) {
	ts := time.Date(2020, 5, 1, 10, 0, 0, 0, time.UTC)
	same := patternImage(7, 120, 90)
	diff := patternImage(55, 120, 90)
	previews := map[string][]byte{
		"i1": jpegBytes(t, same), "i2": jpegBytes(t, same), "i3": jpegBytes(t, diff),
	}
	items := []map[string]interface{}{
		{"id": "i1", "filename": "a.jpg", "mediaMetadata": map[string]string{"creationTime": ts.Format(time.RFC3339), "width": "120", "height": "90"}},
		{"id": "i2", "filename": "a-1.jpg", "mediaMetadata": map[string]string{"creationTime": ts.Format(time.RFC3339), "width": "120", "height": "90"}},
		{"id": "i3", "filename": "b.jpg", "mediaMetadata": map[string]string{"creationTime": ts.Format(time.RFC3339), "width": "120", "height": "90"}},
	}
	fg := newDupFake(t, previews, items)
	g := googleClientFor(fg.Server)
	g.Email = "t@t"

	c := NewClient("", t.TempDir(), 0, false) // no shutterfly token needed
	c.Stdin = strings.NewReader("y\n")
	dbPath := filepath.Join(t.TempDir(), "s.db")

	// dry run: TSV only, no album created
	if err := c.FindDuplicates(g, dbPath, 2, true, "t@t", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := fg.albumIDs[dupReviewAlbumTitle]; ok {
		t.Fatal("dry run created album")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), "duplicates.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 { // header + i1 + i2
		t.Fatalf("tsv: %v", lines)
	}

	// real run: album created, i1+i2 added
	if err := c.FindDuplicates(g, dbPath, 2, false, "t@t", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := fg.albumIDs[dupReviewAlbumTitle]; !ok {
		t.Fatal("review album not created")
	}
	var total int
	for _, call := range fg.addCalls {
		total += len(call)
		for _, id := range call {
			if id == "i3" {
				t.Fatal("non-dup added")
			}
		}
	}
	if total != 2 {
		t.Fatalf("added %d", total)
	}
	// second run adds nothing
	fg.addCalls = nil
	if err := c.FindDuplicates(g, dbPath, 2, false, "t@t", true, ""); err != nil {
		t.Fatal(err)
	}
	if len(fg.addCalls) != 0 {
		t.Fatalf("rerun re-added %v", fg.addCalls)
	}
}
