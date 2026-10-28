package shutterfly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const googleAPIBase = "https://photoslibrary.googleapis.com"

var googleScopes = []string{
	"https://www.googleapis.com/auth/photoslibrary.appendonly",
	"https://www.googleapis.com/auth/photoslibrary.edit.appcreateddata",
	"https://www.googleapis.com/auth/photoslibrary.readonly.appcreateddata",
}

// ErrQuota is returned on HTTP 429: the daily Photos Library API quota is
// exhausted. Callers should save state and exit non-zero.
var ErrQuota = errors.New("google photos quota reached")

// GoogleClient wraps the Photos Library API over an OAuth2-authenticated
// http.Client. BaseURL is configurable for httptest.
type GoogleClient struct {
	BaseURL    string
	hc         *http.Client
	verbose    bool
	maxRetries int
}

func (g *GoogleClient) log(format string, args ...interface{}) {
	if g.verbose {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

// do performs a request with backoff on 5xx; 429 maps to ErrQuota.
func (g *GoogleClient) do(method, path string, headers map[string]string, body io.Reader) (*http.Response, error) {
	for retry := 0; retry <= g.maxRetries; retry++ {
		if retry > 0 {
			wait := time.Duration(1<<uint(retry))*time.Second + time.Duration(rand.Float64()*500)*time.Millisecond
			fmt.Printf("Retrying in %.1fs...\n", wait.Seconds())
			time.Sleep(wait)
		}
		var b io.Reader
		if body != nil {
			b = body
		}
		req, err := http.NewRequest(method, g.BaseURL+path, b)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		g.log("%s %s", method, path)
		resp, err := g.hc.Do(req)
		if err != nil {
			continue
		}
		switch {
		case resp.StatusCode == 429:
			resp.Body.Close()
			return nil, ErrQuota
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp, nil
		case resp.StatusCode >= 500:
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			fmt.Printf("Request failed: %d %s\n", resp.StatusCode, strings.TrimSpace(string(data)))
			continue
		default:
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			return nil, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
		}
	}
	return nil, fmt.Errorf("max retries exceeded")
}

// upload streams raw photo bytes and returns the upload token.
func (g *GoogleClient) upload(data []byte, mime, filename string) (string, error) {
	resp, err := g.do("POST", "/v1/uploads", map[string]string{
		"Content-Type":               "application/octet-stream",
		"X-Goog-Upload-Content-Type": mime,
		"X-Goog-Upload-Protocol":     "raw",
		"X-Goog-Upload-File-Name":    filename,
	}, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	token, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(token)), err
}

// batchCreate creates media items in albumID (max 50 per call). Returns
// per-item results; callers check each item's success.
type createdItem struct {
	UploadToken string
	MediaItemID string
	OK          bool
}

func (g *GoogleClient) batchCreate(albumID string, items []struct {
	UploadToken, FileName string
}) ([]createdItem, error) {
	type newMediaItem struct {
		SimpleMediaItem struct {
			UploadToken string `json:"uploadToken"`
			FileName    string `json:"fileName"`
		} `json:"simpleMediaItem"`
	}
	body := struct {
		AlbumID       string         `json:"albumId,omitempty"`
		NewMediaItems []newMediaItem `json:"newMediaItems"`
	}{AlbumID: albumID}
	for _, it := range items {
		var nmi newMediaItem
		nmi.SimpleMediaItem.UploadToken = it.UploadToken
		nmi.SimpleMediaItem.FileName = it.FileName
		body.NewMediaItems = append(body.NewMediaItems, nmi)
	}
	raw, _ := json.Marshal(body)
	resp, err := g.do("POST", "/v1/mediaItems:batchCreate", map[string]string{"Content-Type": "application/json"}, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		NewMediaItemResults []struct {
			UploadToken string `json:"uploadToken"`
			Status      struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"status"`
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		} `json:"newMediaItemResults"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	out := make([]createdItem, len(result.NewMediaItemResults))
	for i, r := range result.NewMediaItemResults {
		ok := r.Status.Code == 0 || r.Status.Message == "Success" || r.Status.Message == "OK"
		out[i] = createdItem{UploadToken: r.UploadToken, MediaItemID: r.MediaItem.ID, OK: ok}
	}
	return out, nil
}

// batchAddMediaItems adds up to 50 item IDs to an album (all-or-nothing).
func (g *GoogleClient) batchAddMediaItems(albumID string, ids []string) error {
	raw, _ := json.Marshal(map[string]interface{}{"mediaItemIds": ids})
	resp, err := g.do("POST", "/v1/albums/"+albumID+":batchAddMediaItems", map[string]string{"Content-Type": "application/json"}, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (g *GoogleClient) createAlbum(title string) (string, error) {
	raw, _ := json.Marshal(map[string]interface{}{"album": map[string]string{"title": title}})
	resp, err := g.do("POST", "/v1/albums", map[string]string{"Content-Type": "application/json"}, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

// listAlbums returns title → id for app-created albums (handles pagination).
func (g *GoogleClient) listAlbums() (map[string]string, error) {
	albums := map[string]string{}
	pageToken := ""
	for {
		path := "/v1/albums?pageSize=50"
		if pageToken != "" {
			path += "&pageToken=" + url.QueryEscape(pageToken)
		}
		resp, err := g.do("GET", path, nil, nil)
		if err != nil {
			return nil, err
		}
		var result struct {
			Albums []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"albums"`
			NextPageToken string `json:"nextPageToken"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, a := range result.Albums {
			albums[a.Title] = a.ID
		}
		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken
	}
	return albums, nil
}

// listMediaItems pages app-created media items (readonly.appcreateddata).
// Returns (id, filename) pairs and the next page token.
func (g *GoogleClient) listMediaItems(pageToken string) ([]struct{ ID, FileName string }, string, error) {
	path := "/v1/mediaItems?pageSize=100"
	if pageToken != "" {
		path += "&pageToken=" + url.QueryEscape(pageToken)
	}
	resp, err := g.do("GET", path, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var result struct {
		MediaItems []struct {
			ID       string `json:"id"`
			FileName string `json:"filename"`
		} `json:"mediaItems"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", err
	}
	items := make([]struct{ ID, FileName string }, len(result.MediaItems))
	for i, m := range result.MediaItems {
		items[i].ID = m.ID
		items[i].FileName = m.FileName
	}
	return items, result.NextPageToken, nil
}

// ---------------------------------------------------------------------------
// OAuth (installed-app loopback flow with PKCE)

type googleClientJSON struct {
	Installed struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
	} `json:"installed"`
}

func oauthConfigFromFile(path string) (*oauth2.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cj googleClientJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return nil, fmt.Errorf("invalid client JSON %s: %w", path, err)
	}
	if cj.Installed.ClientID == "" {
		return nil, fmt.Errorf("invalid client JSON %s: missing installed.client_id", path)
	}
	return &oauth2.Config{
		ClientID:     cj.Installed.ClientID,
		ClientSecret: cj.Installed.ClientSecret,
		Scopes:       googleScopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		},
	}, nil
}

// persistTokenSource saves each refreshed token (0600, dir 0700).
type persistTokenSource struct {
	path string
	src  oauth2.TokenSource
	last *oauth2.Token
}

func (s *persistTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	if s.last == nil || t.AccessToken != s.last.AccessToken {
		if err := saveTokenFile(s.path, t); err != nil {
			fmt.Printf("warning: couldn't save token: %v\n", err)
		}
		s.last = t
	}
	return t, nil
}

func saveTokenFile(path string, t *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadOrAuthorize returns an OAuth2 http.Client: loads the saved token if any,
// otherwise runs the loopback+PKCE consent flow.
func loadOrAuthorize(cfg *oauth2.Config, tokenPath string) (*http.Client, bool, error) {
	data, err := os.ReadFile(tokenPath)
	if err == nil {
		var tok oauth2.Token
		if json.Unmarshal(data, &tok) == nil && tok.AccessToken != "" {
			src := &persistTokenSource{path: tokenPath, src: cfg.TokenSource(context.Background(), &tok)}
			return oauth2.NewClient(context.Background(), src), true, nil
		}
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}

	// Loopback flow on a random port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, false, err
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	verifier := oauth2.GenerateVerifier()
	authURL := cfg.AuthCodeURL("state",
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent select_account"),
		oauth2.SetAuthURLParam("redirect_uri", redirect),
	)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			errCh <- fmt.Errorf("oauth error: %s", e)
			fmt.Fprintln(w, "Authorization failed. You can close this tab.")
			return
		}
		codeCh <- r.URL.Query().Get("code")
		fmt.Fprintln(w, "Authorization received. You can close this tab and return to the terminal.")
	})}
	go srv.Serve(l)
	defer srv.Shutdown(context.Background())

	fmt.Println("\nGoogle Photos authorization required.")
	fmt.Println("Open this URL in a browser and sign in with the Google Photos account:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	exec.Command("open", authURL).Start()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, false, err
	case <-time.After(5 * time.Minute):
		return nil, false, fmt.Errorf("timed out waiting for authorization")
	}

	tok, err := cfg.Exchange(context.Background(), code,
		oauth2.VerifierOption(verifier),
		oauth2.SetAuthURLParam("redirect_uri", redirect),
	)
	if err != nil {
		return nil, false, fmt.Errorf("token exchange: %w", err)
	}
	if err := saveTokenFile(tokenPath, tok); err != nil {
		return nil, false, err
	}
	src := &persistTokenSource{path: tokenPath, src: cfg.TokenSource(context.Background(), tok), last: tok}
	return oauth2.NewClient(context.Background(), src), true, nil
}

// NewGoogleClient loads the OAuth client config and produces an authenticated
// GoogleClient.
func NewGoogleClient(clientPath, tokenPath string, verbose bool) (*GoogleClient, error) {
	cfg, err := oauthConfigFromFile(clientPath)
	if err != nil {
		return nil, fmt.Errorf("can't read Google client JSON: %w\nDownload it from Google Cloud (see README) to %s", err, clientPath)
	}
	hc, _, err := loadOrAuthorize(cfg, tokenPath)
	if err != nil {
		return nil, err
	}
	return &GoogleClient{BaseURL: googleAPIBase, hc: hc, verbose: verbose, maxRetries: 3}, nil
}
