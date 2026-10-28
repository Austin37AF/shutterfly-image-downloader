package shutterfly

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoogle is a minimal Photos Library API stub.
type fakeGoogle struct {
	*httptest.Server
	mu            sync.Mutex
	uploads       map[string]string // upload token -> filename
	created       map[string]string // upload token -> mediaItem id
	albums        map[string]string // title -> id
	addCalls      [][]string
	createCalls   []string
	batchCreateN  int
	failUploads   bool
	failCreate    bool
	partialFailAt int
	nextItem      int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{
		uploads: map[string]string{},
		created: map[string]string{},
		albums:  map[string]string{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/uploads":
			if f.failUploads {
				w.WriteHeader(429)
				return
			}
			tok := fmt.Sprintf("tok-%d", len(f.uploads)+1)
			f.uploads[tok] = r.Header.Get("X-Goog-Upload-File-Name")
			fmt.Fprint(w, tok)
		case r.URL.Path == "/v1/mediaItems:batchCreate":
			f.batchCreateN++
			if f.failCreate {
				w.WriteHeader(429)
				return
			}
			var req struct {
				AlbumID       string `json:"albumId"`
				NewMediaItems []struct {
					SimpleMediaItem struct {
						UploadToken string `json:"uploadToken"`
						FileName    string `json:"fileName"`
					} `json:"simpleMediaItem"`
				} `json:"newMediaItems"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var results []map[string]interface{}
			for i, it := range req.NewMediaItems {
				tok := it.SimpleMediaItem.UploadToken
				if f.partialFailAt > 0 && i+1 == f.partialFailAt {
					results = append(results, map[string]interface{}{
						"uploadToken": tok,
						"status":      map[string]interface{}{"code": 5, "message": "NOT_FOUND"},
					})
					continue
				}
				f.nextItem++
				id := fmt.Sprintf("mi-%d", f.nextItem)
				f.created[tok] = id
				results = append(results, map[string]interface{}{
					"uploadToken": tok,
					"status":      map[string]interface{}{"code": 0, "message": "Success"},
					"mediaItem":   map[string]interface{}{"id": id},
				})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"newMediaItemResults": results})
		case r.URL.Path == "/v1/albums" && r.Method == "POST":
			var req struct {
				Album struct {
					Title string `json:"title"`
				} `json:"album"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			f.createCalls = append(f.createCalls, req.Album.Title)
			id := fmt.Sprintf("alb-%d", len(f.albums)+1)
			f.albums[req.Album.Title] = id
			json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "title": req.Album.Title})
		case r.URL.Path == "/v1/albums" && r.Method == "GET":
			var albums []map[string]string
			for t, id := range f.albums {
				albums = append(albums, map[string]string{"id": id, "title": t})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"albums": albums})
		case strings.HasPrefix(r.URL.Path, "/v1/albums/") && strings.HasSuffix(r.URL.Path, ":batchAddMediaItems"):
			var req struct {
				MediaItemIds []string `json:"mediaItemIds"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			f.addCalls = append(f.addCalls, req.MediaItemIds)
			json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// fakeShutterfly serves searchMoments, getAlbums, getAlbum, and downloads.
func fakeShutterfly(t *testing.T, libraryIDs []string, albums map[string][]string, downloads *sync.Map) *httptest.Server {
	// albums: albumName -> raw moment IDs
	mkRecord := func(id string) string {
		// record[9:25] is a zero-padded 16-char moment ID
		padded := strings.Repeat("0", 16-len(id)) + id
		rec := strings.Repeat("x", 9) + padded
		return rec + strings.Repeat("x", 277-len(rec))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Query().Get("method")
		switch {
		case method == "searchMoments":
			moms, _ := json.Marshal(libraryIDs)
			fmt.Fprintf(w, `{"result":{"success":true,"message":"ok","payload":{"number_matches":%d,"moments":%s}}}`, len(libraryIDs), moms)
		case method == "album.getAlbums":
			var perms []string
			for name, ids := range albums {
				perms = append(perms, fmt.Sprintf(`{"story":{"uid":"uid-%s","name":%q,"visible_moment_count":%d}}`, name, name, len(ids)))
			}
			fmt.Fprintf(w, `{"result":{"success":true,"message":"ok","payload":[[%s],[],[]]}}`, strings.Join(perms, ","))
		case method == "album.getAlbum":
			var req struct {
				Params []interface{} `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			uid := fmt.Sprint(req.Params[1])
			name := strings.TrimPrefix(uid, "uid-")
			var blob strings.Builder
			for _, id := range albums[name] {
				blob.WriteString(mkRecord(id))
			}
			fmt.Fprintf(w, `{"result":{"success":true,"message":"ok","payload":{"moments":%q}}}`, blob.String())
		case strings.Contains(r.URL.Path, "/download"):
			id := r.URL.Query().Get("momentId")
			downloads.Store(id, true)
			w.Header().Set("Content-Disposition", `attachment; filename="dl-`+id+`.jpg"`)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpegdata-" + id))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func googleClientFor(srv *httptest.Server) *GoogleClient {
	return &GoogleClient{BaseURL: srv.URL, hc: srv.Client(), maxRetries: 0}
}

// upload + batchCreate happy path
func TestGoogleUploadBatchCreate(t *testing.T) {
	f := newFakeGoogle(t)
	g := googleClientFor(f.Server)
	tok, err := g.upload([]byte("img"), "image/jpeg", "a.jpg")
	if err != nil || tok == "" {
		t.Fatalf("upload: %v %q", err, tok)
	}
	items := []struct{ UploadToken, FileName string }{{tok, "a.jpg"}}
	created, err := g.batchCreate("alb-1", items)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || !created[0].OK || created[0].MediaItemID == "" {
		t.Fatalf("created: %+v", created)
	}
	if f.uploads[tok] != "a.jpg" {
		t.Fatalf("filename header not passed: %q", f.uploads[tok])
	}
}

// partial batchCreate failure
func TestBatchCreatePartial(t *testing.T) {
	f := newFakeGoogle(t)
	f.partialFailAt = 2
	g := googleClientFor(f.Server)
	items := []struct{ UploadToken, FileName string }{{"t1", "a"}, {"t2", "b"}, {"t3", "c"}}
	created, err := g.batchCreate("alb-1", items)
	if err != nil {
		t.Fatal(err)
	}
	if !created[0].OK || created[1].OK || !created[2].OK {
		t.Fatalf("expected middle item failed: %+v", created)
	}
	if created[1].MediaItemID != "" {
		t.Fatal("failed item got mediaItemId")
	}
}

// batchAddMediaItems chunking + state + all-or-nothing
func TestAddMembershipsChunked(t *testing.T) {
	f := newFakeGoogle(t)
	g := googleClientFor(f.Server)
	state := &GoogleState{Albums: map[string]string{"Shutterfly - A": "alb-1"}, Items: map[string]GoogleItemState{}}
	var mids []string
	for i := 0; i < 120; i++ {
		mid := fmt.Sprintf("m%d", i)
		mids = append(mids, mid)
		state.Items[mid] = GoogleItemState{MediaItemID: "mi-" + mid, Albums: []string{"Shutterfly"}}
	}
	plan := &transferPlan{albumMeta: []googleAlbumMeta{{title: "Shutterfly - A", momentIDs: mids}}}
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	if err := c.addMemberships(g, state, plan, statePath); err != nil {
		t.Fatal(err)
	}
	if len(f.addCalls) != 3 {
		t.Fatalf("expected 3 chunked calls, got %d", len(f.addCalls))
	}
	for _, call := range f.addCalls {
		if len(call) > 50 {
			t.Fatalf("chunk >50: %d", len(call))
		}
	}
	if !contains(state.Items["m0"].Albums, "Shutterfly - A") {
		t.Fatal("state not updated")
	}

	// all-or-nothing error leaves state unchanged for that chunk
	f2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer f2.Close()
	state2 := &GoogleState{Albums: map[string]string{"T": "a1"}, Items: map[string]GoogleItemState{"x": {MediaItemID: "mi", Albums: nil}}}
	plan2 := &transferPlan{albumMeta: []googleAlbumMeta{{title: "T", momentIDs: []string{"x"}}}}
	g2 := &GoogleClient{BaseURL: f2.URL, hc: f2.Client()}
	if err := c.addMemberships(g2, state2, plan2, filepath.Join(t.TempDir(), "s.json")); err == nil {
		t.Fatal("expected error")
	}
	if contains(state2.Items["x"].Albums, "T") {
		t.Fatal("state changed despite failed chunk")
	}
}

// resume: done items not re-downloaded/uploaded, missing memberships added
func TestTransferResume(t *testing.T) {
	var downloads sync.Map
	libIDs := []string{"001", "002", "003"}
	sf := fakeShutterfly(t, libIDs, map[string][]string{"A": {"001", "002", "003"}}, &downloads)
	fg := newFakeGoogle(t)

	statePath := filepath.Join(t.TempDir(), "state.json")
	state := &GoogleState{Albums: map[string]string{}, Items: map[string]GoogleItemState{
		"1": {MediaItemID: "mi-old", Albums: []string{"Shutterfly"}},
	}}
	state.save(statePath)

	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase = sf.URL
	c.AlbumBase = sf.URL
	c.DownloadBase = sf.URL
	g := googleClientFor(fg.Server)

	stats, err := c.ToGoogle(g, statePath, 2, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := downloads.Load("1"); ok {
		t.Error("done moment re-downloaded")
	}
	if stats.Uploaded != 2 || stats.Skipped != 1 {
		t.Errorf("stats: %+v", stats)
	}
	// old item should get album membership via batchAdd
	found := false
	for _, call := range fg.addCalls {
		for _, id := range call {
			if id == "mi-old" {
				found = true
			}
		}
	}
	if !found {
		t.Error("missing membership for pre-done item not added")
	}
}

// 429 stops cleanly
func TestQuotaStops(t *testing.T) {
	var downloads sync.Map
	sf := fakeShutterfly(t, []string{"001"}, map[string][]string{}, &downloads)
	fg := newFakeGoogle(t)
	fg.failUploads = true

	statePath := filepath.Join(t.TempDir(), "state.json")
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase = sf.URL
	c.AlbumBase = sf.URL
	c.DownloadBase = sf.URL
	g := googleClientFor(fg.Server)

	_, err := c.ToGoogle(g, statePath, 1, 0, "", false)
	if err != ErrQuota {
		t.Fatalf("expected ErrQuota, got %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Error("state file not saved")
	}
}

// 429 on batchCreate with more work queued must stop, not deadlock
func TestQuotaOnBatchCreateDoesNotHang(t *testing.T) {
	var downloads sync.Map
	var ids []string
	for i := 1; i <= 120; i++ {
		ids = append(ids, fmt.Sprintf("%d", i))
	}
	sf := fakeShutterfly(t, ids, map[string][]string{}, &downloads)
	fg := newFakeGoogle(t)
	fg.failCreate = true

	statePath := filepath.Join(t.TempDir(), "state.json")
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase = sf.URL
	c.AlbumBase = sf.URL
	c.DownloadBase = sf.URL
	g := googleClientFor(fg.Server)

	done := make(chan error, 1)
	go func() {
		_, err := c.ToGoogle(g, statePath, 4, 0, "", false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != ErrQuota {
			t.Fatalf("expected ErrQuota, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ToGoogle hung after batchCreate quota error")
	}
}

// album creation only when missing; recovery via list
func TestEnsureAlbumRecovery(t *testing.T) {
	f := newFakeGoogle(t)
	f.albums["Shutterfly"] = "alb-existing"
	f.albums["Shutterfly - A"] = "alb-a"
	g := googleClientFor(f.Server)
	state := &GoogleState{Albums: map[string]string{}, Items: map[string]GoogleItemState{}}

	title, err := ensureMasterAlbum(g, state)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Shutterfly" || state.Albums["Shutterfly"] != "alb-existing" {
		t.Fatalf("recovery failed: %s %v", title, state.Albums)
	}
	if len(f.createCalls) != 0 {
		t.Fatalf("album re-created despite recovery: %v", f.createCalls)
	}

	plan := &transferPlan{albumMeta: []googleAlbumMeta{{title: "Shutterfly - A"}, {title: "Shutterfly - B"}}}
	if err := ensureAlbumTitles(g, state, plan); err != nil {
		t.Fatal(err)
	}
	if len(f.createCalls) != 1 || f.createCalls[0] != "Shutterfly - B" {
		t.Fatalf("expected only B created: %v", f.createCalls)
	}
}

// master rollover
func TestMasterRollover(t *testing.T) {
	old := maxAlbumItems
	maxAlbumItems = 2
	defer func() { maxAlbumItems = old }()

	f := newFakeGoogle(t)
	f.albums["Shutterfly"] = "alb-1"
	g := googleClientFor(f.Server)
	state := &GoogleState{
		Albums: map[string]string{"Shutterfly": "alb-1"},
		Items: map[string]GoogleItemState{
			"m1": {MediaItemID: "a", Albums: []string{"Shutterfly"}},
			"m2": {MediaItemID: "b", Albums: []string{"Shutterfly"}},
		},
	}
	title, err := ensureMasterAlbum(g, state)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Shutterfly (2)" {
		t.Fatalf("expected rollover, got %s", title)
	}
	if len(f.createCalls) != 1 {
		t.Fatal("rollover album not created")
	}
}

// join normalization
func TestNormalizeID(t *testing.T) {
	if normalizeID("  00012345 ") != "12345" {
		t.Fail()
	}
	if normalizeID("0") != "" {
		t.Fail()
	}
}

// state file atomic + perms
func TestStateSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	s := &GoogleState{Albums: map[string]string{"T": "id"}, Items: map[string]GoogleItemState{}}
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
	s2 := loadGoogleState(path)
	if s2.Albums["T"] != "id" {
		t.Fatal("state did not round-trip")
	}
}

// library ID fallback for album mode
func TestUserIDLibraryFallback(t *testing.T) {
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	t.Setenv("LIFE_UID", "")
	t.Setenv("SHUTTERFLY_LIBRARY_ID", "60030568104")
	c.LifeUID = ""
	if got := c.userID(); got != "60030568104" {
		t.Fatalf("userID = %q", got)
	}
}

// full dry-run needs no Google client
func TestDryRunNoGoogle(t *testing.T) {
	var downloads sync.Map
	sf := fakeShutterfly(t, []string{"001"}, map[string][]string{"A": {"001"}}, &downloads)
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase = sf.URL
	c.AlbumBase = sf.URL
	c.DownloadBase = sf.URL
	_, err := c.ToGoogle(nil, filepath.Join(t.TempDir(), "s.json"), 1, 0, "", true)
	if err != nil {
		t.Fatalf("dry run should not need Google client: %v", err)
	}
}
