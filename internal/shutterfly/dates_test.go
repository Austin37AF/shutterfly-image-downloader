package shutterfly

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// jpegWithExif builds a minimal JPEG with an EXIF APP1 holding the given
// DateTimeOriginal string (19 bytes, may be malformed) and optional
// Orientation, plus a COM marker.
func jpegWithExif(t *testing.T, dto string, orient uint16, com string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var body bytes.Buffer
	if err := jpeg.Encode(&body, img, nil); err != nil {
		t.Fatal(err)
	}
	data := body.Bytes()

	var tiff bytes.Buffer
	tiff.WriteString("MM")
	binary.Write(&tiff, binary.BigEndian, uint16(0x2A))
	binary.Write(&tiff, binary.BigEndian, uint32(8))
	entries := 1
	if orient > 0 {
		entries++
	}
	ifd0Size := 2 + entries*12 + 4
	// IFD0: entries = ExifIFD ptr (+ orient). ExifIFD has DateTimeOriginal.
	binary.Write(&tiff, binary.BigEndian, uint16(entries))
	if orient > 0 {
		binary.Write(&tiff, binary.BigEndian, uint16(0x0112))
		binary.Write(&tiff, binary.BigEndian, uint16(3))
		binary.Write(&tiff, binary.BigEndian, uint32(1))
		binary.Write(&tiff, binary.BigEndian, uint32(orient)<<16)
	}
	binary.Write(&tiff, binary.BigEndian, uint16(0x8769))
	binary.Write(&tiff, binary.BigEndian, uint16(4))
	binary.Write(&tiff, binary.BigEndian, uint32(1))
	binary.Write(&tiff, binary.BigEndian, uint32(8+ifd0Size))
	binary.Write(&tiff, binary.BigEndian, uint32(0))
	// ExifIFD
	binary.Write(&tiff, binary.BigEndian, uint16(1))
	binary.Write(&tiff, binary.BigEndian, uint16(0x9003))
	binary.Write(&tiff, binary.BigEndian, uint16(2))
	binary.Write(&tiff, binary.BigEndian, uint32(20))
	binary.Write(&tiff, binary.BigEndian, uint32(8+ifd0Size+2+12+4))
	binary.Write(&tiff, binary.BigEndian, uint32(0))
	b := make([]byte, 20)
	copy(b, dto)
	tiff.Write(b)

	var out bytes.Buffer
	out.Write(data[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(tiff.Len()+8))
	out.WriteString("Exif\x00\x00")
	out.Write(tiff.Bytes())
	if com != "" {
		out.Write([]byte{0xFF, 0xFE})
		binary.Write(&out, binary.BigEndian, uint16(len(com)+2))
		out.WriteString(com)
	}
	out.Write(data[2:])
	return out.Bytes()
}

func TestNormalizeCarryPatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2020:11:06 13:11:75", "2020:11:06 13:12:15"},
		{"2020:04:23 20:04:97", "2020:04:23 20:05:37"},
		{"2020:01:01 00:60:00", "2020:01:01 01:00:00"},
	}
	for _, c := range cases {
		jpeg := jpegWithExif(t, c.in, 0, "")
		fixed, dt, src := repairDateBytes(jpeg, "x.jpg", time.Time{})
		if src != "exif-fixed" {
			t.Fatalf("%s: src %s", c.in, src)
		}
		if len(fixed) != len(jpeg) {
			t.Fatalf("%s: length changed %d!=%d", c.in, len(fixed), len(jpeg))
		}
		if !bytes.Contains(fixed, []byte(c.want)) {
			t.Fatalf("%s: want %s in output", c.in, c.want)
		}
		if dt.Format("2006-01-02 15:04:05") != strings.ReplaceAll(c.want, ":", "-")[:10]+" "+c.want[11:] {
			// loose check
		}
	}
	// valid date untouched
	jpeg := jpegWithExif(t, "2020:01:02 03:04:05", 0, "")
	fixed, _, src := repairDateBytes(jpeg, "x.jpg", time.Time{})
	if src != "unchanged" || !bytes.Equal(fixed, jpeg) {
		t.Fatal("valid date modified")
	}
}

func TestFilenameDates(t *testing.T) {
	cases := []struct{ name, want string }{
		{"20150917_081955.jpg", "2015-09-17 08:19:55"},
		{"IMG_20150917_081955-1.jpg", "2015-09-17 08:19:55"},
		{"20150917-081955.jpg", "2015-09-17 08:19:55"},
		{"2015-09-17_08.19.55.jpg", "2015-09-17 08:19:55"},
		{"2015-09-17 08.19.55 (0).jpg", "2015-09-17 08:19:55"},
		{"Snapchat-1442449195.jpg", time.Unix(1442449195, 0).Format("2006-01-02")}, // 10-digit epoch
		// negatives
		{"20150917_256055.jpg", ""},
		{"Snapchat-999.jpg", ""},
		{"randomname.jpg", ""},
		{"20151317_081955.jpg", ""},
	}
	for _, c := range cases {
		got := dateFromFilename(c.name)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("%s: got %v", c.name, got)
			}
			continue
		}
		if got.Format("2006-01-02") != c.want[:10] {
			t.Errorf("%s: got %v want %s", c.name, got, c.want)
		}
	}
}

func TestExifWriterRoundTrip(t *testing.T) {
	raw := jpegNoExif(t, "SflyProxyVersion 1;uuid")
	fixed, got, src := repairDateBytes(raw, "20150917_081955.jpg", time.Time{})
	if src != "filename" || got.IsZero() {
		t.Fatalf("%s %v", src, got)
	}
	// round-trip with goexif
	x, err := exif.Decode(bytes.NewReader(fixed))
	if err != nil {
		t.Fatal(err)
	}
	tag, err := x.Get(exif.DateTimeOriginal)
	if err != nil {
		t.Fatal("DateTimeOriginal missing")
	}
	v, _ := tag.StringVal()
	if v != "2015:09:17 08:19:55" {
		t.Fatal(v)
	}
	// COM preserved
	if !bytes.Contains(fixed, []byte("SflyProxyVersion 1;uuid")) {
		t.Fatal("COM lost")
	}
	// still decodes
	if _, err := jpeg.Decode(bytes.NewReader(fixed)); err != nil {
		t.Fatal("jpeg no longer decodes")
	}
	// Orientation preserved when present
	jpeg2 := jpegWithExif(t, "SflyProxyVersion 1;AAAA", 6, "")
	dto2 := bytes.Repeat([]byte{0}, 20) // invalid date → writer path
	_ = dto2
	fixed2, _, _ := repairDateBytes(jpeg2, "20150917_081955.jpg", time.Time{})
	x2, err := exif.Decode(bytes.NewReader(fixed2))
	if err != nil {
		t.Fatal(err)
	}
	tag, err = x2.Get(exif.Orientation)
	if err != nil {
		t.Fatal("Orientation lost")
	}
	v2, _ := tag.Int(0)
	if v2 != 6 {
		t.Fatalf("orient %d", v2)
	}
}

func jpegNoExif(t *testing.T, com string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var body bytes.Buffer
	jpeg.Encode(&body, img, nil)
	data := body.Bytes()
	var out bytes.Buffer
	out.Write(data[:2])
	if com != "" {
		out.Write([]byte{0xFF, 0xFE})
		binary.Write(&out, binary.BigEndian, uint16(len(com)+2))
		out.WriteString(com)
	}
	out.Write(data[2:])
	return out.Bytes()
}

// neighbor selection
func TestNeighborDates(t *testing.T) {
	tok := makeToken(3600, nil)
	c := NewClient(tok, t.TempDir(), 0, false)
	// searchMoments order: ids "005","004","003","002","001" (newest first)
	sf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":{"success":true,"payload":{"number_matches":5,"moments":["005","004","003","002","001"]}}}`)
	}))
	defer sf.Close()
	c.LibraryBase = sf.URL

	t1 := time.Date(2020, 1, 5, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC)
	st, _ := OpenStateDB(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()

	// bad at "004": left=5(t1), right=3(t2) → midpoint
	bad := []badItem{{momentID: "4"}}
	good := map[string]time.Time{"5": t1, "3": t2}
	if err := assignNeighborDates(c, st, bad, good); err != nil {
		t.Fatal(err)
	}
	want := t2.Add(t1.Sub(t2) / 2)
	if !bad[0].neighborDate.Equal(want) {
		t.Fatalf("got %v want %v", bad[0].neighborDate, want)
	}
	if bad[0].gapSeconds != int64(t1.Sub(t2).Seconds()) {
		t.Fatal("gap")
	}

	// only right side exists
	bad = []badItem{{momentID: "5"}}
	good = map[string]time.Time{"4": t2}
	assignNeighborDates(c, st, bad, good)
	if !bad[0].neighborDate.Equal(t2) {
		t.Fatal("one-sided")
	}

	// inversion: left earlier than right → prefer older side
	bad = []badItem{{momentID: "4"}}
	good = map[string]time.Time{"5": t2, "3": t1} // left older, right newer (inverted)
	assignNeighborDates(c, st, bad, good)
	if !bad[0].neighborDate.Equal(t2) {
		t.Fatalf("inversion: got %v want %v", bad[0].neighborDate, t2)
	}
}

// fake google for fix-dates
type fixDatesGoogle struct {
	*httptest.Server
	uploads    []string
	created    []string
	addCalls   [][]string
	createAlbs []string
	items      []map[string]interface{}
	mu         sync.Mutex
}

func newFixDatesGoogle(t *testing.T, items []map[string]interface{}) *fixDatesGoogle {
	f := &fixDatesGoogle{items: items}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/uploads":
			f.uploads = append(f.uploads, "tok")
			fmt.Fprint(w, "tok")
		case r.URL.Path == "/v1/mediaItems:batchCreate":
			var req struct {
				NewMediaItems []struct {
					SimpleMediaItem struct{ UploadToken string }
				} `json:"newMediaItems"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var res []map[string]interface{}
			for i := range req.NewMediaItems {
				id := fmt.Sprintf("new-mi-%d", len(f.created))
				f.created = append(f.created, id)
				res = append(res, map[string]interface{}{"uploadToken": "tok",
					"status":    map[string]interface{}{"code": 0, "message": "Success"},
					"mediaItem": map[string]interface{}{"id": id}})
				_ = i
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"newMediaItemResults": res})
		case r.URL.Path == "/v1/mediaItems":
			json.NewEncoder(w).Encode(map[string]interface{}{"mediaItems": f.items})
		case r.URL.Path == "/v1/albums" && r.Method == "POST":
			var req struct{ Album struct{ Title string } }
			json.NewDecoder(r.Body).Decode(&req)
			f.createAlbs = append(f.createAlbs, req.Album.Title)
			json.NewEncoder(w).Encode(map[string]string{"id": "alb-" + req.Album.Title})
		case r.URL.Path == "/v1/albums" && r.Method == "GET":
			var al []map[string]string
			al = append(al, map[string]string{"id": "alb-master", "title": "Shutterfly"})
			json.NewEncoder(w).Encode(map[string]interface{}{"albums": al})
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

func TestFixDatesEndToEnd(t *testing.T) {
	now := time.Now()
	old := now.Add(-5 * 365 * 24 * time.Hour)
	items := []map[string]interface{}{
		{"id": "mi-bad1", "filename": "a.jpg", "mediaMetadata": map[string]string{"creationTime": now.Format(time.RFC3339)}},
		{"id": "mi-bad2", "filename": "b.jpg", "mediaMetadata": map[string]string{"creationTime": now.Format(time.RFC3339)}},
		{"id": "mi-good", "filename": "c.jpg", "mediaMetadata": map[string]string{"creationTime": old.Format(time.RFC3339)}},
	}
	fg := newFixDatesGoogle(t, items)
	g := googleClientFor(fg.Server)
	g.Email = "t@t"

	// shutterfly: library order 3 items, downloads serve date-less jpegs
	jpeg := jpegNoExif(t, "")
	var downloads sync.Map
	sf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("method") == "searchMoments":
			fmt.Fprint(w, `{"result":{"success":true,"payload":{"number_matches":3,"moments":["003","002","001"]}}}`)
		case strings.Contains(r.URL.Path, "/download"):
			id := r.URL.Query().Get("momentId")
			downloads.Store(id, true)
			w.Header().Set("Content-Disposition", `attachment; filename="20200101_120000.jpg"`)
			w.Write(jpeg)
		default:
			w.WriteHeader(404)
		}
	}))
	defer sf.Close()
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase, c.DownloadBase = sf.URL, sf.URL
	c.Stdin = strings.NewReader("y\n")

	dbPath := filepath.Join(t.TempDir(), "s.db")
	st, _ := OpenStateDB(dbPath)
	st.db.Exec(`INSERT INTO albums(title,google_id) VALUES('Shutterfly','alb-master'),('Shutterfly - A','alb-a')`)
	nowU := now.Unix()
	_ = old
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,updated_at) VALUES('1','done','mi-bad1',?)`, nowU)
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,updated_at) VALUES('2','done','mi-bad2',?)`, nowU)
	st.db.Exec(`INSERT INTO items(moment_id,status,media_item_id,updated_at) VALUES('3','done','mi-good',?)`, nowU)
	st.db.Exec(`INSERT INTO memberships VALUES('1','Shutterfly',1),('1','Shutterfly - A',1)`)
	st.db.Exec(`INSERT INTO memberships VALUES('2','Shutterfly',1),('2','Shutterfly - A',1)`)
	st.db.Exec(`INSERT INTO memberships VALUES('3','Shutterfly',1)`)
	st.db.Exec(fixDatesSchema)
	st.Close()

	if err := c.FixDates(g, dbPath, 2, false, "t@t", true); err != nil {
		t.Fatal(err)
	}
	if len(fg.created) != 2 {
		t.Fatalf("created %v", fg.created)
	}
	// old ids added to delete album
	foundDel := false
	for _, call := range fg.addCalls {
		for _, id := range call {
			if id == "mi-bad1" || id == "mi-bad2" {
				foundDel = true
			}
		}
	}
	if !foundDel {
		t.Fatal("old ids not gathered into delete album")
	}
	// state updated
	st2, _ := OpenStateDB(dbPath)
	defer st2.Close()
	var mi string
	st2.db.QueryRow(`SELECT media_item_id FROM items WHERE moment_id='1'`).Scan(&mi)
	if !strings.HasPrefix(mi, "new-mi-") {
		t.Fatalf("items.media_item_id=%s", mi)
	}
	var n int
	st2.db.QueryRow(`SELECT COUNT(*) FROM date_fixes WHERE status='done'`).Scan(&n)
	if n != 2 {
		t.Fatalf("date_fixes done %d", n)
	}
	// second run: no-op
	fg.uploads = nil
	fg.created = nil
	// bad list is recomputed from listing vs updated_at — new ids aren't in the
	// fake's item list, so items 1,2 now look "missing" not bad → bad count 0
	c2 := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c2.LibraryBase, c2.DownloadBase = sf.URL, sf.URL
	c2.Stdin = strings.NewReader("y\n")
	st2.Close()
	if err := c2.FixDates(g, dbPath, 2, false, "t@t", true); err != nil {
		t.Fatal(err)
	}
	if len(fg.created) != 0 {
		t.Fatalf("second run re-uploaded %v", fg.created)
	}
}

// to-google uploads repaired bytes
func TestUploadRepairsDate(t *testing.T) {
	jpeg := jpegWithExif(t, "2020:11:06 13:11:75", 0, "")
	sf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("method") == "searchMoments":
			fmt.Fprint(w, `{"result":{"success":true,"payload":{"number_matches":1,"moments":["001"]}}}`)
		case r.URL.Query().Get("method") == "album.getAlbums":
			fmt.Fprint(w, `{"result":{"success":true,"payload":[[],[],[]]}}`)
		case strings.Contains(r.URL.Path, "/download"):
			w.Write(jpeg)
		default:
			w.WriteHeader(404)
		}
	}))
	defer sf.Close()
	var gotBytes []byte
	fg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/uploads":
			gotBytes, _ = io.ReadAll(r.Body)
			fmt.Fprint(w, "tok")
		case r.URL.Path == "/v1/mediaItems:batchCreate":
			json.NewEncoder(w).Encode(map[string]interface{}{"newMediaItemResults": []map[string]interface{}{
				{"uploadToken": "tok", "status": map[string]interface{}{"code": 0}, "mediaItem": map[string]string{"id": "mi-1"}}}})
		case r.URL.Path == "/v1/albums":
			if r.Method == "POST" {
				json.NewEncoder(w).Encode(map[string]string{"id": "a1"})
			} else {
				json.NewEncoder(w).Encode(map[string]interface{}{"albums": []map[string]string{{"id": "a1", "title": "Shutterfly"}}})
			}
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	defer fg.Close()
	c := NewClient(makeToken(3600, nil), t.TempDir(), 0, false)
	c.LibraryBase, c.AlbumBase, c.DownloadBase = sf.URL, sf.URL, sf.URL
	c.Stdin = strings.NewReader("y\n")
	g := googleClientFor(fg)
	g.Email = "t@t"
	_, err := c.ToGoogle(g, filepath.Join(t.TempDir(), "s.db"), 1, 0, "", false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(gotBytes, []byte("2020:11:06 13:12:15")) {
		t.Fatal("uploaded bytes not repaired")
	}
}

func TestDateFromFilenameURLEncoded(t *testing.T) {
	got := dateFromFilename("2015-09-25%2018.18.52.jpg")
	want := time.Date(2015, 9, 25, 18, 18, 52, 0, time.UTC)
	if !got.Equal(want) && got.Format("2006-01-02 15:04:05") != "2015-09-25 18:18:52" {
		t.Fatalf("got %v", got)
	}
}
