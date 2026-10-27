package shutterfly

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func makeToken(expDelta int64, claims map[string]interface{}) string {
	if claims == nil {
		claims = map[string]interface{}{}
	}
	claims["exp"] = float64(time.Now().Unix() + expDelta)
	h := base64.URLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(claims)
	p := base64.URLEncoding.EncodeToString(b)
	return h + "." + strings.TrimRight(p, "=") + "."
}

// 1. parseToken
func TestParseToken(t *testing.T) {
	// JWT with exp
	claims, expiry := parseToken(makeToken(3600, nil))
	if time.Until(expiry) < 50*time.Minute || time.Until(expiry) > 61*time.Minute {
		t.Fatalf("JWT expiry off: %v", expiry)
	}
	if claims["exp"] == nil {
		t.Fatal("expected exp claim")
	}

	// URL-encoded base64 blob with exp
	blob, _ := json.Marshal(map[string]interface{}{"exp": float64(time.Now().Unix() + 600)})
	enc := base64.StdEncoding.EncodeToString(blob)
	_, expiry = parseToken(enc)
	if time.Until(expiry) < 8*time.Minute {
		t.Fatalf("base64 blob expiry off: %v", expiry)
	}

	// URL-encoded base64 blob without exp -> ~1h default
	blob, _ = json.Marshal(map[string]interface{}{"foo": "bar"})
	enc = base64.StdEncoding.EncodeToString(blob)
	claims, expiry = parseToken(enc)
	if claims["foo"] != "bar" {
		t.Fatal("expected claims parsed")
	}
	if time.Until(expiry) < 55*time.Minute || time.Until(expiry) > 61*time.Minute {
		t.Fatalf("default expiry off: %v", expiry)
	}

	// garbage -> ~1h default, empty claims
	claims, expiry = parseToken("not-a-token!!")
	if len(claims) != 0 {
		t.Fatalf("expected empty claims, got %v", claims)
	}
	if time.Until(expiry) < 55*time.Minute {
		t.Fatalf("garbage expiry off: %v", expiry)
	}
}

// 2. extractMomentIDs
func TestExtractMomentIDs(t *testing.T) {
	var payload struct {
		Result struct {
			Payload struct {
				Moments string `json:"moments"`
			} `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(readFixture(t, "getAlbum.json"), &payload); err != nil {
		t.Fatal(err)
	}
	ids := ExtractMomentIDs(payload.Result.Payload.Moments)
	if len(ids) != 6 {
		t.Fatalf("expected 6 ids, got %d", len(ids))
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "0") {
			t.Fatalf("leading zeros not stripped: %q", id)
		}
	}
	// short trailing record ignored
	short := payload.Result.Payload.Moments + "abc"
	if got := ExtractMomentIDs(short); len(got) != 6 {
		t.Fatalf("expected trailing short record ignored, got %d", len(got))
	}
}

// 3. sanitize
func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"My Album!":       "My Album",
		"photos 2020 (1)": "photos 2020 1",
		"a-b_c d":         "a-b_c d",
		"trail   ":        "trail",
		"Ünïcode namé":    "Ünïcode namé",
		"emoji 😀 strip":   "emoji  strip",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// 4. getAlbums via httptest
func TestGetAlbums(t *testing.T) {
	var gotMethod, gotCT, gotOrigin string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotOrigin = r.Header.Get("Origin")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &gotBody)
		w.Write(readFixture(t, "getAlbums.json"))
	}))
	defer srv.Close()

	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.AlbumBase = srv.URL
	albums, err := c.GetAlbums()
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != "POST" {
		t.Errorf("method = %s", gotMethod)
	}
	if !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		t.Errorf("content-type = %s", gotCT)
	}
	if gotOrigin != "https://photos.shutterfly.com" {
		t.Errorf("origin = %s", gotOrigin)
	}
	if gotBody["method"] != "album.getAlbums" {
		t.Errorf("body method = %v", gotBody["method"])
	}
	params, _ := gotBody["params"].([]interface{})
	if len(params) != 5 || params[2] != nil || params[3] != nil || params[4] != true {
		t.Errorf("params = %v", params)
	}
	if params[0] != c.currentToken() {
		t.Errorf("params[0] should be the token")
	}
	if h, _ := gotBody["headers"].(map[string]interface{}); h["X-SFLY-SubSource"] != "library" {
		t.Errorf("headers = %v", h)
	}

	want := []Album{{"story-111", "Test", 14}, {"story-222", "test as well", 6}}
	if len(albums) != len(want) {
		t.Fatalf("albums = %v", albums)
	}
	for i := range want {
		if albums[i] != want[i] {
			t.Errorf("albums[%d] = %+v, want %+v", i, albums[i], want[i])
		}
	}
}

func TestGetAlbumsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":{"success":false,"message":"boom"},"id":"","error":null}`))
	}))
	defer srv.Close()
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.AlbumBase = srv.URL
	_, err := c.GetAlbums()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected 'boom' error, got %v", err)
	}
}

// 5. getAlbum request body
func TestGetAlbum(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &gotBody)
		w.Write(readFixture(t, "getAlbum.json"))
	}))
	defer srv.Close()

	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.AlbumBase = srv.URL
	moments, err := c.GetAlbum("story-222")
	if err != nil {
		t.Fatal(err)
	}
	params, _ := gotBody["params"].([]interface{})
	if len(params) != 7 {
		t.Fatalf("params = %v", params)
	}
	if params[0] != c.currentToken() || params[1] != "story-222" || params[2] != "startupItem" ||
		params[3] != nil || params[4] != false || params[5] != true || params[6] != true {
		t.Errorf("params = %v", params)
	}
	if h, _ := gotBody["headers"].(map[string]interface{}); h["X-SFLY-SubSource"] != "albums" {
		t.Errorf("headers = %v", h)
	}
	if len(ExtractMomentIDs(moments)) != 6 {
		t.Errorf("expected 6 moments")
	}
}

// 6. album download same-name handling
func TestAlbumDownloadSameName(t *testing.T) {
	var served []byte
	var cd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cd != "" {
			w.Header().Set("Content-Disposition", cd)
		}
		w.Write(served)
	}))
	defer srv.Close()

	out := t.TempDir()
	c := NewClient(makeToken(3600, nil), out, 0, false)
	c.DownloadBase = srv.URL
	albumDir := c.albumDir("Test")

	dl := func(bytes_ []byte, filename string) bool {
		served = bytes_
		if filename != "" {
			cd = fmt.Sprintf(`attachment; filename="%s"`, filename)
		} else {
			cd = ""
		}
		return c.downloadAlbumPhoto("m1", albumDir, 1)
	}

	// (a) first download writes the file
	if !dl([]byte("photo-A"), "pic.jpg") {
		t.Fatal("download a failed")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "pic.jpg")); err != nil {
		t.Fatal("pic.jpg not written")
	}
	// (b) identical bytes → skip, no _1
	if !dl([]byte("photo-A"), "pic.jpg") {
		t.Fatal("download b failed")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "pic_1.jpg")); err == nil {
		t.Fatal("identical rerun created pic_1.jpg")
	}
	// (c) different bytes → pic_1.jpg
	if !dl([]byte("photo-B-different"), "pic.jpg") {
		t.Fatal("download c failed")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "pic_1.jpg")); err != nil {
		t.Fatal("pic_1.jpg not written")
	}
	// (d) third distinct → pic_2; then identical-to-_1 → skip
	if !dl([]byte("photo-C-unique!"), "pic.jpg") {
		t.Fatal("download d failed")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "pic_2.jpg")); err != nil {
		t.Fatal("pic_2.jpg not written")
	}
	if !dl([]byte("photo-B-different"), "pic.jpg") {
		t.Fatal("download d2 failed")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "pic_3.jpg")); err == nil {
		t.Fatal("identical-to-_1 created pic_3.jpg")
	}
	// (e) path traversal stays in the album dir
	if !dl([]byte("evil"), "../../escape.jpg") {
		t.Fatal("download e failed")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "escape.jpg")); err != nil {
		t.Fatal("escape.jpg not in album dir")
	}
	if _, err := os.Stat(filepath.Join(out, "..", "escape.jpg")); err == nil {
		t.Fatal("path traversal escaped")
	}
	// (f) no leftover .part files
	entries, _ := os.ReadDir(albumDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".part-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

// 7. identical: PNG same pixels different bytes, and a changed pixel
func TestIdenticalPixels(t *testing.T) {
	dir := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 30), uint8(y * 30), 128, 255})
		}
	}
	p1 := filepath.Join(dir, "a.png")
	p2 := filepath.Join(dir, "b.png")
	p3 := filepath.Join(dir, "c.png")

	f1, _ := os.Create(p1)
	png.Encode(f1, img) // default compression
	f1.Close()
	f2, _ := os.Create(p2)
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	enc.Encode(f2, img)
	f2.Close()

	if b1, _ := os.ReadFile(p1); string(b1) == string(mustRead(p2)) {
		t.Skip("PNG encodings unexpectedly identical")
	}
	if !Identical(p1, p2) {
		t.Error("same pixels, different bytes should be identical")
	}

	img.SetNRGBA(3, 3, color.NRGBA{0, 0, 0, 255})
	f3, _ := os.Create(p3)
	png.Encode(f3, img)
	f3.Close()
	if Identical(p1, p3) {
		t.Error("changed pixel should differ")
	}
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

// 8. dedupe on a temp dir
func TestDedupe(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	}
	write("a.jpg", "same")
	write("a_1.jpg", "same")    // identical dupe of a.jpg → remove a_1 (longer name)
	write("a_2.jpg", "differ")  // different content → keep
	write("a_1.png", "same")    // different extension → not similar
	write("solo.png", "unique") // no similar

	// dry-run deletes nothing
	n := DedupeAlbum(dir, true, nil, true)
	if n != 1 {
		t.Fatalf("dry-run removed %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "a_1.jpg")); err != nil {
		t.Fatal("dry-run deleted a_1.jpg")
	}

	n = DedupeAlbum(dir, true, nil, false)
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "a_1.jpg")); err == nil {
		t.Fatal("a_1.jpg should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "a_2.jpg")); err != nil {
		t.Fatal("a_2.jpg (different content) should be kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "a_1.png")); err != nil {
		t.Fatal("a_1.png (different ext) should be kept")
	}
}

// 9. compare / fix-incomplete selection
func TestPlanRedownload(t *testing.T) {
	server := []Album{
		{ID: "1", Name: "Complete", PhotoCount: 10},
		{ID: "2", Name: "Partial", PhotoCount: 10},
		{ID: "3", Name: "Missing!", PhotoCount: 5},
		{ID: "4", Name: "Ignored", PhotoCount: 3},
		{ID: "5", Name: "Extra Local Is Fine", PhotoCount: 2},
	}
	local := map[string]int{
		"Complete":            10,
		"Partial":             7,
		"Ignored":             0,
		"Extra Local Is Fine": 99, // local > server → not incomplete for redownload
	}
	ignore := func(name string) bool { return name == "Ignored" }
	plan := PlanRedownload(server, local, ignore)

	// Missing! (sanitized "Missing") has no local dir → missing; Partial → 3 missing.
	// Ignored filtered out. Sorted by photo count: Missing!(5) before Partial(10).
	if len(plan) != 2 {
		t.Fatalf("plan = %v", plan)
	}
	if plan[0].Album.ID != "3" || plan[0].Missing != 5 {
		t.Errorf("plan[0] = %+v", plan[0])
	}
	if plan[1].Album.ID != "2" || plan[1].Missing != 3 {
		t.Errorf("plan[1] = %+v", plan[1])
	}
}

// 10. token refresh under concurrency
func TestTokenRefreshConcurrent(t *testing.T) {
	dir := t.TempDir()
	newTok := makeToken(3600, nil)
	if err := os.WriteFile(filepath.Join(dir, "token.txt"), []byte(newTok+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Query().Get("accessToken")]++
		mu.Unlock()
		w.Write([]byte("data"))
	}))
	defer srv.Close()

	c := NewClient(makeToken(-3600, nil), t.TempDir(), 0, false)
	c.DownloadBase = srv.URL
	c.TokenFile = filepath.Join(dir, "token.txt")
	c.Stdin = strings.NewReader("") // must not be reached

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !c.downloadAlbumPhoto(fmt.Sprintf("m%d", i), t.TempDir(), i) {
				t.Errorf("download %d failed", i)
			}
		}(i)
	}
	wg.Wait()

	if _, err := os.Stat(filepath.Join(dir, "token.txt")); err == nil {
		t.Error("token.txt should be deleted after reading")
	}
	if seen[newTok] != 8 {
		t.Errorf("expected all 8 requests with new token, got %v", seen)
	}
	for tok := range seen {
		if tok != newTok {
			t.Errorf("request used old token %q", tok)
		}
	}
}
