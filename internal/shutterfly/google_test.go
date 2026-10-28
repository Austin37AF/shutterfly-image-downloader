package shutterfly

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fakeGoogle: uploads, batchCreate, albums CRUD, batchAddMediaItems, mediaItems list.
type fakeGoogle struct {
	*httptest.Server
	mu           sync.Mutex
	uploads      map[string]string
	albums       map[string]string
	addCalls     [][]string
	createCalls  []string
	mediaItems   []map[string]string // reconcile list
	failUploads  bool
	uploadErrAt  int
	uploadN      int
	batchCreateN int
	partialFail  bool
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{uploads: map[string]string{}, albums: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/uploads":
			f.uploadN++
			if f.failUploads || (f.uploadErrAt > 0 && f.uploadN == f.uploadErrAt) {
				w.WriteHeader(429)
				return
			}
			tok := fmt.Sprintf("tok-%d", f.uploadN)
			f.uploads[tok] = r.Header.Get("X-Goog-Upload-File-Name")
			fmt.Fprint(w, tok)
		case r.URL.Path == "/v1/mediaItems:batchCreate":
			f.batchCreateN++
			var req struct {
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
				if f.partialFail && i == 1 {
					results = append(results, map[string]interface{}{"uploadToken": it.SimpleMediaItem.UploadToken,
						"status": map[string]interface{}{"code": 5, "message": "NOT_FOUND"}})
					continue
				}
				id := fmt.Sprintf("mi-%s", it.SimpleMediaItem.UploadToken)
				results = append(results, map[string]interface{}{
					"uploadToken": it.SimpleMediaItem.UploadToken,
					"status":      map[string]interface{}{"code": 0, "message": "Success"},
					"mediaItem":   map[string]interface{}{"id": id}})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"newMediaItemResults": results})
		case r.URL.Path == "/v1/mediaItems":
			var items []map[string]string
			for _, m := range f.mediaItems {
				items = append(items, m)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"mediaItems": items})
		case r.URL.Path == "/v1/albums" && r.Method == "POST":
			var req struct {
				Album struct {
					Title string `json:"title"`
				}
			}
			json.NewDecoder(r.Body).Decode(&req)
			f.createCalls = append(f.createCalls, req.Album.Title)
			id := fmt.Sprintf("alb-%d", len(f.albums)+1)
			f.albums[req.Album.Title] = id
			json.NewEncoder(w).Encode(map[string]string{"id": id})
		case r.URL.Path == "/v1/albums" && r.Method == "GET":
			var al []map[string]string
			for t, id := range f.albums {
				al = append(al, map[string]string{"id": id, "title": t})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"albums": al})
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
	t.Cleanup(f.Server.Close)
	return f
}

func fakeShutterfly(t *testing.T, libraryIDs []string, albums map[string][]string, downloads *sync.Map) *httptest.Server {
	mkRecord := func(id string) string {
		padded := strings.Repeat("0", 16-len(id)) + id
		rec := strings.Repeat("x", 9) + padded
		return rec + strings.Repeat("x", 277-len(rec))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch m := r.URL.Query().Get("method"); {
		case m == "searchMoments":
			moms, _ := json.Marshal(libraryIDs)
			fmt.Fprintf(w, `{"result":{"success":true,"payload":{"number_matches":%d,"moments":%s}}}`, len(libraryIDs), moms)
		case m == "album.getAlbums":
			var perms []string
			for name, ids := range albums {
				perms = append(perms, fmt.Sprintf(`{"story":{"uid":"uid-%s","name":%q,"visible_moment_count":%d}}`, name, name, len(ids)))
			}
			fmt.Fprintf(w, `{"result":{"success":true,"payload":[[%s],[],[]]}}`, strings.Join(perms, ","))
		case m == "album.getAlbum":
			var req struct {
				Params []interface{} `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			name := strings.TrimPrefix(fmt.Sprint(req.Params[1]), "uid-")
			var blob strings.Builder
			for _, id := range albums[name] {
				blob.WriteString(mkRecord(id))
			}
			fmt.Fprintf(w, `{"result":{"success":true,"payload":{"moments":%q}}}`, blob.String())
		case strings.Contains(r.URL.Path, "/download"):
			id := r.URL.Query().Get("momentId")
			if downloads != nil {
				downloads.Store(id, true)
			}
			w.Header().Set("Content-Disposition", `attachment; filename="dl-`+id+`.jpg"`)
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

func openTestDB(t *testing.T, dir string) *StateDB {
	t.Helper()
	st, err := OpenStateDB(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func sha(b string) string {
	s := sha256.Sum256([]byte(b))
	return hex.EncodeToString(s[:])
}

// Lock: second open fails.
func TestStateDBLock(t *testing.T) {
	dir := t.TempDir()
	st := openTestDB(t, dir)
	_, err := OpenStateDB(st.path)
	if err == nil || !strings.Contains(err.Error(), "another transfer is running") {
		t.Fatalf("expected lock error, got %v", err)
	}
}

// upload + batchCreate happy path
func TestGoogleUploadBatchCreate(t *testing.T) {
	f := newFakeGoogle(t)
	g := googleClientFor(f.Server)
	tok, _ := g.upload([]byte("x"), "image/jpeg", "a.jpg")
	created, err := g.batchCreate("alb-1", []struct{ UploadToken, FileName string }{{tok, "a.jpg"}})
	if err != nil || len(created) != 1 || !created[0].OK || created[0].MediaItemID == "" {
		t.Fatalf("%v %+v", err, created)
	}
	if f.uploads[tok] != "a.jpg" {
		t.Fatal("filename header missing")
	}
}

// per-item failure: attempts++ then failed after 3
func TestFinishBatchAttempts(t *testing.T) {
	st := openTestDB(t, t.TempDir())
	st.db.Exec(`INSERT INTO items(moment_id,status,updated_at) VALUES('m','pending',0)`)
	rs := []uploadResult{{momentID: "m", token: "t", fileName: "f", sha256: "s"}}
	for i := 0; i < 3; i++ {
		if err := st.finishBatch("Shutterfly", rs, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	var attempts int
	st.db.QueryRow(`SELECT status, attempts FROM items WHERE moment_id='m'`).Scan(&status, &attempts)
	if status != "failed" || attempts != 3 {
		t.Fatalf("%s %d", status, attempts)
	}
}

// content dedupe via shaDone/linkDup
func TestContentDedupe(t *testing.T) {
	st := openTestDB(t, t.TempDir())
	h := sha("same")
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,sha256,updated_at) VALUES('a','done','mi-1',?,0)`, h)
	_, miid, ok := st.shaDone(h)
	if !ok || miid != "mi-1" {
		t.Fatal("shaDone")
	}
	st.db.Exec(`INSERT INTO items(moment_id,status,updated_at) VALUES('b','pending',0)`)
	if err := st.linkDup("b", miid, h, "dup-of:a"); err != nil {
		t.Fatal(err)
	}
	var st2, mi string
	st.db.QueryRow(`SELECT status, media_item_id FROM items WHERE moment_id='b'`).Scan(&st2, &mi)
	if st2 != "done" || mi != "mi-1" {
		t.Fatal("linkDup")
	}
}

// pipeline dedupe: two moments, same bytes → one upload
func TestPipelineDedupe(t *testing.T) {
	sf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch m := r.URL.Query().Get("method"); {
		case m == "searchMoments":
			fmt.Fprint(w, `{"result":{"success":true,"payload":{"number_matches":2,"moments":["001","002"]}}}`)
		case m == "album.getAlbums":
			fmt.Fprint(w, `{"result":{"success":true,"payload":[[],[],[]]}}`)
		case strings.Contains(r.URL.Path, "/download"):
			w.Header().Set("Content-Disposition", `attachment; filename="same.jpg"`)
			w.Write([]byte("samebytes"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer sf.Close()
	fg := newFakeGoogle(t)
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase, c.AlbumBase, c.DownloadBase = sf.URL, sf.URL, sf.URL
	dbPath := filepath.Join(t.TempDir(), "s.db")
	stats, err := c.ToGoogle(googleClientFor(fg.Server), dbPath, 4, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	// uploads may both happen before dedupe can see the hash, but only ONE
	// media item must be created; the second links to it.
	if fg.batchCreateN != 1 {
		t.Fatalf("expected 1 batchCreate, got %d", fg.batchCreateN)
	}
	if stats.Uploaded != 1 || stats.Linked != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// crash-window reconcile
func TestReconcile(t *testing.T) {
	fg := newFakeGoogle(t)
	fg.mediaItems = []map[string]string{
		{"id": "mi-1", "filename": "a.jpg"},
		{"id": "mi-x", "filename": "b.jpg"},
		{"id": "mi-y", "filename": "b.jpg"},
	}
	g := googleClientFor(fg.Server)
	st := openTestDB(t, t.TempDir())
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		st.db.Exec(`INSERT INTO items(moment_id,status,file_name,updated_at) VALUES(?,'creating',?,0)`, id, id+".jpg")
	}
	st.db.Exec(`UPDATE items SET file_name='a.jpg' WHERE moment_id='c1'`)
	st.db.Exec(`UPDATE items SET file_name='b.jpg' WHERE moment_id IN ('c2','c3')`)
	// c4: filename not present → pending
	if err := Reconcile(g, st); err != nil {
		t.Fatal(err)
	}
	get := func(id string) (string, string) {
		var s, m string
		st.db.QueryRow(`SELECT status, COALESCE(media_item_id,'') FROM items WHERE moment_id=?`, id).Scan(&s, &m)
		return s, m
	}
	if s, m := get("c1"); s != "done" || m != "mi-1" {
		t.Fatalf("c1 %s %s", s, m)
	}
	if s, _ := get("c4"); s != "pending" {
		t.Fatalf("c4 %s", s)
	}
	// c2,c3 ambiguous: 2 remote vs 2 creating — same count → assigned in order
	s2, _ := get("c2")
	s3, _ := get("c3")
	if s2 != "done" || s3 != "done" {
		t.Fatalf("c2 %s c3 %s", s2, s3)
	}
}

// reconcile ambiguous: more creating rows than remote matches stays creating
func TestReconcileAmbiguous(t *testing.T) {
	fg := newFakeGoogle(t)
	fg.mediaItems = []map[string]string{{"id": "mi-1", "filename": "b.jpg"}}
	g := googleClientFor(fg.Server)
	st := openTestDB(t, t.TempDir())
	for _, id := range []string{"c1", "c2"} {
		st.db.Exec(`INSERT INTO items(moment_id,status,file_name,updated_at) VALUES(?,'creating','b.jpg',0)`, id)
	}
	if err := Reconcile(g, st); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM items WHERE status='creating'`).Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 still creating, got %d", n)
	}
}

// memberships: added=1 only after success; error leaves rows
func TestMemberships(t *testing.T) {
	fg := newFakeGoogle(t)
	fg.albums["T"] = "alb-1"
	g := googleClientFor(fg.Server)
	st := openTestDB(t, t.TempDir())
	st.setAlbumID("T", "alb-1")
	for i := 0; i < 60; i++ {
		mid := fmt.Sprintf("m%d", i)
		st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,updated_at) VALUES(?,'done',?,0)`, mid, "mi-"+mid)
		st.db.Exec(`INSERT INTO memberships(moment_id,album_title,added) VALUES(?,'T',0)`, mid)
	}
	if err := addMemberships(g, st); err != nil {
		t.Fatal(err)
	}
	if len(fg.addCalls) != 2 {
		t.Fatalf("%d calls", len(fg.addCalls))
	}
	var added int
	st.db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE added=1`).Scan(&added)
	if added != 60 {
		t.Fatalf("added %d", added)
	}
	// error case
	f2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer f2.Close()
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,updated_at) VALUES('x','done','mi-x',0)`)
	st.db.Exec(`INSERT INTO memberships(moment_id,album_title,added) VALUES('x','T',0)`)
	g2 := &GoogleClient{BaseURL: f2.URL, hc: f2.Client(), maxRetries: 0}
	if err := addMemberships(g2, st); err == nil {
		t.Fatal("expected error")
	}
	var a int
	st.db.QueryRow(`SELECT added FROM memberships WHERE moment_id='x'`).Scan(&a)
	if a != 0 {
		t.Fatal("added set despite failure")
	}
}

// rollover via membership counts
func TestRollover(t *testing.T) {
	old := maxAlbumItems
	maxAlbumItems = 2
	defer func() { maxAlbumItems = old }()
	st := openTestDB(t, t.TempDir())
	st.db.Exec(`INSERT INTO albums(title,google_id) VALUES('Shutterfly','a1')`)
	for _, m := range []string{"1", "2"} {
		st.db.Exec(`INSERT INTO items(moment_id,status,updated_at) VALUES(?,'done',0)`, m)
		st.db.Exec(`INSERT INTO memberships(moment_id,album_title,added) VALUES(?,'Shutterfly',1)`, m)
	}
	title, err := currentMasterTitle(st)
	if err != nil || title != "Shutterfly (2)" {
		t.Fatalf("%v %s", err, title)
	}
}

// status output on seeded db (just exercises the code path)
func TestStatusOutput(t *testing.T) {
	st := openTestDB(t, t.TempDir())
	st.db.Exec(`INSERT INTO items(moment_id,status,updated_at) VALUES('a','done',0),('b','pending',0),('c','failed',0),('d','creating',0)`)
	st.db.Exec(`INSERT INTO albums(title,google_id) VALUES('T','x')`)
	st.db.Exec(`INSERT INTO memberships VALUES('a','T',1),('b','T',0)`)
	if err := st.Status(); err != nil {
		t.Fatal(err)
	}
}

// resume: done item not re-downloaded; membership added
func TestResume(t *testing.T) {
	var downloads sync.Map
	sf := fakeShutterfly(t, []string{"001", "002"}, map[string][]string{"A": {"001", "002"}}, &downloads)
	fg := newFakeGoogle(t)
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "s.db")
	st, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,updated_at) VALUES('1','done','mi-old',0)`)
	st.db.Exec(`INSERT INTO albums(title,google_id) VALUES('Shutterfly - A','alb-a')`)
	st.db.Exec(`INSERT INTO memberships(moment_id,album_title,added) VALUES('1','Shutterfly - A',0)`)
	st.Close()

	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase, c.AlbumBase, c.DownloadBase = sf.URL, sf.URL, sf.URL
	stats, err := c.ToGoogle(googleClientFor(fg.Server), dbPath, 2, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := downloads.Load("001"); ok {
		t.Error("done item re-downloaded")
	}
	if stats.Uploaded != 1 {
		t.Fatalf("stats %+v", stats)
	}
	found := false
	for _, call := range fg.addCalls {
		for _, id := range call {
			if id == "mi-old" {
				found = true
			}
		}
	}
	if !found {
		t.Error("missing membership not added")
	}
}

// 429 stops cleanly with state saved
func TestQuotaStops(t *testing.T) {
	var downloads sync.Map
	sf := fakeShutterfly(t, []string{"001"}, map[string][]string{}, &downloads)
	fg := newFakeGoogle(t)
	fg.failUploads = true
	dbPath := filepath.Join(t.TempDir(), "s.db")
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase, c.AlbumBase, c.DownloadBase = sf.URL, sf.URL, sf.URL
	_, err := c.ToGoogle(googleClientFor(fg.Server), dbPath, 1, 0, "", false)
	if err != ErrQuota {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Error("db not saved")
	}
}

// dry run needs no Google client, creates db
func TestDryRun(t *testing.T) {
	var downloads sync.Map
	sf := fakeShutterfly(t, []string{"001"}, map[string][]string{"A": {"001"}}, &downloads)
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase, c.AlbumBase, c.DownloadBase = sf.URL, sf.URL, sf.URL
	dbPath := filepath.Join(t.TempDir(), "s.db")
	if _, err := c.ToGoogle(nil, dbPath, 1, 0, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Error("db not created")
	}
}

// ---------------------------------------------------------------------------
// Cognito refresh

func makeCognitoToken(expDelta int64) string {
	return makeToken(expDelta, map[string]interface{}{
		"iss":       "https://cognito-idp.us-east-1.amazonaws.com/pool1",
		"aud":       "clientid1",
		"token_use": "id",
	})
}

func TestCognitoRefresh(t *testing.T) {
	var hits int32
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"AuthenticationResult": map[string]string{"IdToken": makeCognitoToken(3600), "RefreshToken": "rotated"}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	rtPath := filepath.Join(dir, "refresh-token")
	os.WriteFile(rtPath, []byte("rt-1\n"), 0600)
	c := NewClient(makeCognitoToken(-10), t.TempDir(), 0, false)
	c.RefreshFile = rtPath
	c.CognitoBase = srv.URL
	c.Stdin = strings.NewReader("")

	// concurrent ensureToken: only one refresh
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.ensureToken() }()
	}
	wg.Wait()
	if hits != 1 {
		t.Fatalf("hits %d", hits)
	}
	if gotBody["ClientId"] != "clientid1" {
		t.Fatalf("%v", gotBody)
	}
	// rotation written 0600
	b, _ := os.ReadFile(rtPath)
	if strings.TrimSpace(string(b)) != "rotated" {
		t.Fatal("rotation not saved")
	}
	fi, _ := os.Stat(rtPath)
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// new token set: expiry is now in the future
	if !time.Now().Before(c.expiry) {
		t.Fatal("token not refreshed")
	}
}

func TestCognitoRefreshError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"__type":"NotAuthorizedException"}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "refresh-token")
	os.WriteFile(rtPath, []byte("rt\n"), 0600)
	c := NewClient(makeCognitoToken(-10), t.TempDir(), 0, false)
	c.RefreshFile = rtPath
	c.CognitoBase = srv.URL
	if _, _, err := c.refreshViaCognitoLocked(); err == nil || !strings.Contains(err.Error(), "NotAuthorizedException") {
		t.Fatalf("%v", err)
	}
}

func TestRefreshPermRefused(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "refresh-token")
	os.WriteFile(rtPath, []byte("rt\n"), 0644)
	c := NewClient(makeCognitoToken(-10), t.TempDir(), 0, false)
	c.RefreshFile = rtPath
	if _, _, err := c.refreshViaCognitoLocked(); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("%v", err)
	}
}

// finishBatch marks master membership in the same tx as done.
func TestFinishBatchMasterMembership(t *testing.T) {
	st := openTestDB(t, t.TempDir())
	st.db.Exec(`INSERT INTO albums(title,google_id) VALUES('Shutterfly','a1')`)
	st.db.Exec(`INSERT INTO items(moment_id,status,updated_at) VALUES('m','creating',0)`)
	st.db.Exec(`INSERT INTO memberships(moment_id,album_title,added) VALUES('m','Shutterfly',0)`)
	rs := []uploadResult{{momentID: "m", token: "t1", fileName: "f"}}
	if err := st.finishBatch("Shutterfly", rs, map[string]string{"t1": "mi-1"}); err != nil {
		t.Fatal(err)
	}
	var added int
	st.db.QueryRow(`SELECT added FROM memberships WHERE moment_id='m' AND album_title='Shutterfly'`).Scan(&added)
	if added != 1 {
		t.Fatal("master membership not marked in same tx")
	}
	// crash-window: no pending membership remains, so nothing re-adds
	ids, _ := st.pendingMemberships("Shutterfly")
	if len(ids) != 0 {
		t.Fatal("would re-add already-added master item")
	}
}

// a linked dup is never batchAdded to master; two dups share one id
func TestDupMembershipsSingleID(t *testing.T) {
	st := openTestDB(t, t.TempDir())
	h := sha("same")
	st.db.Exec(`INSERT INTO albums(title,google_id) VALUES('Shutterfly','a1'),('Shutterfly - A','a2')`)
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,sha256,updated_at) VALUES('a','done','mi-1',?,0)`, h)
	st.db.Exec(`INSERT INTO memberships VALUES('a','Shutterfly',1),('a','Shutterfly - A',1)`)
	// link two dups to the same media item
	for _, m := range []string{"b", "c"} {
		st.db.Exec(`INSERT INTO items(moment_id,status,updated_at) VALUES(?,'pending',0)`, m)
		st.db.Exec(`INSERT INTO memberships VALUES(?,'Shutterfly',0),(?,'Shutterfly - A',0)`, m, m)
		if err := st.linkDup(m, "mi-1", h, "dup-of:a"); err != nil {
			t.Fatal(err)
		}
	}
	// master: dup rows should already be added (inherited)
	for _, title := range []string{"Shutterfly", "Shutterfly - A"} {
		ids, err := st.pendingMemberships(title)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Fatalf("%s: pending %v", title, ids)
		}
	}
	// but if the original's row were still pending, only ONE id per chunk
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,sha256,updated_at) VALUES('d','done','mi-2','h2',0)`)
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,sha256,updated_at) VALUES('e','done','mi-2','h2',0)`)
	st.db.Exec(`INSERT INTO memberships VALUES('d','Shutterfly - A',0),('e','Shutterfly - A',0)`)
	ids, err := st.pendingMemberships("Shutterfly - A")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "mi-2" {
		t.Fatalf("expected one distinct id, got %v", ids)
	}
}
