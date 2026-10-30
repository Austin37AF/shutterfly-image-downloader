// Package shutterfly implements the Shutterfly library and album APIs.
// The album API is ported from JoshBurke/Shutterfly-Album-Downloader
// (downloader.py) — same JSON-RPC bodies, headers and semantics.
package shutterfly

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Default endpoints; overridable on Client for tests.
const (
	LibraryBaseURL  = "https://photos-api3.shutterfly.com"
	AlbumBaseURL    = "https://cmd.thislife.com"
	DownloadBaseURL = "https://io.thislife.com"
)

type headerProfile int

const (
	libraryProfile headerProfile = iota // Bearer + x-api-key, photos3 origin
	albumProfile                        // photos.shutterfly.com origin, no auth headers
)

// Client talks to the Shutterfly APIs. Safe for concurrent use.
type Client struct {
	LibraryBase       string // e.g. LibraryBaseURL
	AlbumBase         string // e.g. AlbumBaseURL
	DownloadBase      string // e.g. DownloadBaseURL
	OutputDir         string
	RateLimit         time.Duration
	MaxRetries        int
	Verbose           bool
	RetryFailedGoogle bool      // -to-google -retry: put failed items back to pending
	LifeUID           string    // -life-uid override
	RefreshFile       string    // Cognito refresh token file (0600)
	CognitoBase       string    // override Cognito endpoint (tests)
	IgnoreAlbums      []string  // -ignore-albums names (matched raw or sanitized)
	TokenFile         string    // file consulted when refreshing the token (default "token.txt")
	Stdin             io.Reader // for the token prompt (default os.Stdin)

	mu          sync.Mutex // guards token/claims/expiry and serializes the refresh prompt
	token       string
	claims      map[string]interface{}
	expiry      time.Time
	stdinReader *bufio.Reader

	dirLocks sync.Map // album dir -> *sync.Mutex, serializes same-name checks per dir
}

func NewClient(token, outputDir string, rateLimit float64, verbose bool) *Client {
	c := &Client{
		LibraryBase:  LibraryBaseURL,
		AlbumBase:    AlbumBaseURL,
		DownloadBase: DownloadBaseURL,
		OutputDir:    outputDir,
		RateLimit:    time.Duration(rateLimit * float64(time.Second)),
		MaxRetries:   3,
		Verbose:      verbose,
		TokenFile:    "token.txt",
		Stdin:        os.Stdin,
	}
	c.setToken(token)
	os.MkdirAll(outputDir, 0755)
	return c
}

func (c *Client) log(format string, args ...interface{}) {
	if c.Verbose {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// setToken replaces the token and re-parses claims/expiry.
// Callers that already hold c.mu should use setTokenLocked.
func (c *Client) setToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setTokenLocked(token)
}

func (c *Client) setTokenLocked(token string) {
	c.token = token
	c.claims, c.expiry = parseToken(token)
}

func parseToken(token string) (map[string]interface{}, time.Time) {
	claims := make(map[string]interface{})
	defaultExpiry := time.Now().Add(time.Hour)

	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		payload := parts[1]
		if m := len(payload) % 4; m != 0 {
			payload += strings.Repeat("=", 4-m)
		}
		for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.StdEncoding} {
			if decoded, err := enc.DecodeString(payload); err == nil {
				if json.Unmarshal(decoded, &claims) == nil {
					if exp, ok := claims["exp"].(float64); ok {
						return claims, time.Unix(int64(exp), 0)
					}
				}
			}
		}
	}

	if decoded, err := url.QueryUnescape(token); err == nil {
		if m := len(decoded) % 4; m != 0 {
			decoded += strings.Repeat("=", 4-m)
		}
		if raw, err := base64.StdEncoding.DecodeString(decoded); err == nil {
			if json.Unmarshal(raw, &claims) == nil {
				if exp, ok := claims["exp"].(float64); ok {
					return claims, time.Unix(int64(exp), 0)
				}
				return claims, defaultExpiry
			}
		}
	}

	return claims, defaultExpiry
}

// userID resolves the Shutterfly user ID: -life-uid flag, then LIFE_UID env,
// then SHUTTERFLY_LIBRARY_ID env, then token claims (sfly3_uid preferred,
// then sfly_uid). The library-ID fallback covers accounts where the album
// API's user ID is the library ID and neither claim is present.
func (c *Client) userID() string {
	if c.LifeUID != "" {
		return c.LifeUID
	}
	if uid := os.Getenv("LIFE_UID"); uid != "" {
		return uid
	}
	if uid := os.Getenv("SHUTTERFLY_LIBRARY_ID"); uid != "" {
		return uid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if uid, ok := c.claims["sfly3_uid"].(string); ok {
		c.log("Using sfly3_uid: %s", uid)
		return uid
	}
	if uid, ok := c.claims["sfly_uid"].(string); ok {
		c.log("Using sfly_uid: %s", uid)
		return uid
	}
	return ""
}

// ensureToken refreshes the access token when it has expired, prompting once
// under c.mu so concurrent callers reuse the new token. Ported from the
// Python make_request TokenExpiredError branch.
func (c *Client) ensureToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Try a Cognito refresh when expired or within 10 min of expiry.
	if c.RefreshFile != "" && time.Now().Add(10*time.Minute).After(c.expiry) {
		if _, statErr := os.Stat(c.RefreshFile); statErr == nil {
			if tok, _, err := c.refreshViaCognitoLocked(); err == nil && tok != "" {
				c.setTokenLocked(tok)
				fmt.Println("Access token refreshed via Cognito")
				return nil
			} else if err != nil {
				fmt.Println("refresh failed:", err)
			}
		}
	}

	if time.Now().Before(c.expiry) {
		return nil
	}

	fmt.Println("\nAccess token has expired. Please provide a new token.")
	fmt.Println("Options:")
	fmt.Println("1. Save token to a file named 'token.txt' and press Enter")
	fmt.Println("2. Paste token directly (macOS terminals cut pasted input at 1024 characters, so use option 1 for long tokens)")

	var newToken string
	if data, err := os.ReadFile(c.TokenFile); err == nil {
		os.Remove(c.TokenFile)
		newToken = strings.TrimSpace(string(data))
		if newToken != "" {
			fmt.Println("Successfully read token from token.txt")
		}
	}
	if newToken == "" {
		if c.stdinReader == nil {
			c.stdinReader = bufio.NewReader(c.Stdin)
		}
		fmt.Print("Enter token: ")
		line, err := c.stdinReader.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("no token provided: %w", err)
		}
		newToken = strings.TrimSpace(line)
	}
	if newToken == "" {
		return fmt.Errorf("no token provided")
	}

	c.setTokenLocked(newToken)
	fmt.Println("Access token updated successfully")
	return nil
}

// apiCall describes one request; URL and body are (re)built per attempt so
// they always carry the current token.
type apiCall struct {
	profile headerProfile
	method  string
	url     func(token string) string
	body    func(token string) io.Reader
}

// request performs call with exponential-backoff retries; non-2xx and
// transport errors count as failures. Refreshes an expired token first.
func (c *Client) request(call apiCall) (*http.Response, error) {
	var lastErr error
	for retry := 0; retry <= c.MaxRetries; retry++ {
		if err := c.ensureToken(); err != nil {
			return nil, err
		}

		if retry > 0 {
			wait := time.Duration(1<<uint(retry))*time.Second + time.Duration(rand.Float64()*500)*time.Millisecond
			fmt.Printf("Retrying in %.1fs...\n", wait.Seconds())
			time.Sleep(wait)
		}

		var body io.Reader
		if call.body != nil {
			body = call.body(c.currentToken())
		}
		req, err := http.NewRequest(call.method, call.url(c.currentToken()), body)
		if err != nil {
			return nil, err
		}
		setProfileHeaders(req, call.profile, c.currentToken())

		c.log("Request: %s %s", call.method, req.URL)
		resp, err := c.httpClient().Do(req)
		if err != nil {
			lastErr = err
			fmt.Printf("Request failed: %v\n", err)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		resp.Body.Close()
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		fmt.Printf("Request failed: %d\n", resp.StatusCode)
	}
	return nil, fmt.Errorf("max retries exceeded: %v", lastErr)
}

func (c *Client) httpClient() *http.Client {
	// Constructed lazily so tests can rely on the zero-ish path too; the
	// timeout matches the original tool.
	return &http.Client{Timeout: 300 * time.Second}
}

func setProfileHeaders(req *http.Request, p headerProfile, token string) {
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	switch p {
	case albumProfile:
		req.Header.Set("Origin", "https://photos.shutterfly.com")
		req.Header.Set("Referer", "https://photos.shutterfly.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Safari/605.1.15")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	default: // libraryProfile
		req.Header.Set("Origin", "https://photos3.shutterfly.com")
		req.Header.Set("Referer", "https://photos3.shutterfly.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("x-api-key", "zlrHU7h6cTGTApk3IbOU0aVuxlEJkPeA")
		req.Header.Set("Sec-Fetch-Site", "same-site")
	}
}

// libraryCall is a POST to the photos-api3 endpoint; the JSON body is sent as
// a raw string with the form-urlencoded content type, like the Python does.
func (c *Client) libraryCall(method string, urlStr string, buildBody func(token string) io.Reader) (*http.Response, error) {
	return c.request(apiCall{
		profile: libraryProfile,
		method:  method,
		url:     func(string) string { return urlStr },
		body:    buildBody,
	})
}

// jsonBody marshals v into the raw-JSON-string body expected by the APIs.
func jsonBody(v interface{}) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// dirMutex returns the per-directory mutex used to serialize same-name
// checks in album downloads.
func (c *Client) dirMutex(dir string) *sync.Mutex {
	l, _ := c.dirLocks.LoadOrStore(dir, &sync.Mutex{})
	return l.(*sync.Mutex)
}
