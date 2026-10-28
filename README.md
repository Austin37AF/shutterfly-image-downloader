# Shutterfly Image Downloader

Go tool to download all photos from Shutterfly. Features:
- **Parallel downloads** - configurable worker count for speed
- **Smart duplicate detection** - compares file content (SHA256), not just filenames
- **EXIF date preservation** - sets file modification time from photo metadata
- **Automatic retries** - handles network failures with exponential backoff
- **Resume support** - skips already-downloaded files, safe to restart
- **Failure logging** - tracks failed downloads for easy retry

## Getting Your Token and Library ID

Shutterfly doesn't have a public API, so you need to grab credentials from your browser:

1. Log into https://photos3.shutterfly.com
2. Open DevTools (F12 or Cmd+Option+I on Mac)
3. Go to the **Network** tab
4. Filter by `photos-api3`
5. Click on any photo/album to trigger API requests
6. Find a request to `photos-api3.shutterfly.com/photos/json?method=searchMoments`
7. Copy the **token** from the `Authorization: Bearer <token>` header
8. Copy the **library ID** from the request payload (second parameter in `params` array)

> ⚠️ Tokens are valid for ~23 hours. If you get auth errors, grab a fresh token.

## Build

```bash
go build -o shutterfly-downloader .
```

## Usage

```bash
# Download all photos
./shutterfly-downloader -token "YOUR_TOKEN" -library-id "YOUR_LIBRARY_ID"

# Or via environment variables
export SHUTTERFLY_TOKEN="YOUR_TOKEN"
export SHUTTERFLY_LIBRARY_ID="YOUR_LIBRARY_ID"
./shutterfly-downloader

# Count photos only
./shutterfly-downloader -count

# Custom output directory
./shutterfly-downloader -output ./my_photos

# Faster downloads (more workers)
./shutterfly-downloader -workers 100

# Retry failed downloads
./shutterfly-downloader -retry -output ./my_photos
```

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-token` | Shutterfly access token (or `SHUTTERFLY_TOKEN` env var) | - |
| `-library-id` | Shutterfly library ID (or `SHUTTERFLY_LIBRARY_ID` env var) | - |
| `-output` | Output directory | `shutterfly_photos` |
| `-workers` | Number of parallel download workers | `10` |
| `-rate-limit` | Delay between requests per worker (seconds) | `0.05` |
| `-count` | Only count photos, don't download | `false` |
| `-retry` | Retry failed downloads from `failed_downloads.txt` | `false` |
| `-verbose` | Debug logging | `false` |

## Output Structure

```
output_dir/
├── IMG_1234.jpg           # Unique photos
├── IMG_5678.png
├── Duplicates/            # Different photos with same filename
│   ├── IMG_1234_123456789.jpg
│   └── IMG_1234_987654321.jpg
└── failed_downloads.txt   # Moment IDs that failed (for -retry)
```

## Duplicate Handling

When a file with the same name already exists:
1. Downloads the new file to memory
2. Compares SHA256 hash with existing file
3. **If identical** → skips (true duplicate)
4. **If different** → saves to `Duplicates/` folder with moment ID appended

## Utilities

```bash
# Fix dates on already-downloaded files (reads EXIF, sets file mtime)
go run fix_dates.go ~/Pictures/Shutterfly

# Find duplicate files by content hash
go run dedupe.go ~/Pictures/Shutterfly --dry-run

# Verify download completeness
go run verify.go "TOKEN" "LIBRARY_ID" ~/Pictures/Shutterfly
```

## Uploading to Google Photos

Google removed the Drive → Photos sync, so use the built-in transfer instead:
see [Google Photos transfer](#google-photos-transfer). It streams straight from
Shutterfly to Google Photos without keeping local copies.

## Notes

- Recommended workers: 10-100 (higher may cause rate limiting)
- File modification dates are set from EXIF DateTimeOriginal when available
- Photos without EXIF data will have today's date
- The downloader is idempotent - safe to run multiple times
## Album mode

Download organized **albums** (the photo collections visible at
https://photos.shutterfly.com) instead of the flat library search. Photos land
in `output/<album name>/`, one directory per album. The album API (`cmd.thislife.com`)
was ported from [JoshBurke/Shutterfly-Album-Downloader](https://github.com/JoshBurke/Shutterfly-Album-Downloader).

### Getting the album-mode token

Album mode uses a different token than library mode. To grab it:

1. Log into https://photos.shutterfly.com
2. Open DevTools → **Network** tab, filter by `cmd.thislife.com`
3. Click on an album to trigger a request to
   `cmd.thislife.com/json?method=album.getAlbums`
4. The token is `params[0]` in the request payload (a URL-encoded string)
5. On pre-2013 accounts, also copy `params[1]` — that's your user ID; pass it
   via `-life-uid` or `LIFE_UID`. When unset, the user ID falls back to
   `SHUTTERFLY_LIBRARY_ID` and then the token claims — for some accounts the
   album API's user ID is the library ID.

> Tested 2026-10: the `Authorization: Bearer` token from photos3.shutterfly.com
> works for both library and album mode. The library ID (and album-mode user
> ID) can be found in the `searchMoments` response's face thumbnail URLs
> (`.../face/<library id>/image/...`); set it as `SHUTTERFLY_LIBRARY_ID`.
>
> ⚠️ Older accounts may still need tokens taken from different pages
> (photos3.shutterfly.com vs photos.shutterfly.com). If your album token
> expires mid-run, drop the new one into `token.txt` in the current directory
> or paste it at the prompt — workers pause and continue with the new token.

### Album-mode flags

| Flag | Description |
|---|---|
| `-albums` | Download all albums |
| `-album NAME` | Download a single album by exact name |
| `-resume-from NAME` | With `-albums`, start at this album |
| `-ignore-albums "A,B"` | Skip albums (also reads `output/ignore_albums.txt`); matches raw or sanitized name |
| `-count` | With `-albums`: per-album photo counts and total |
| `-compare` | Table of server vs local photo counts per album |
| `-fix-incomplete` | Redownload albums missing photos (respects ignores) |
| `-dedupe` | Remove identical `name_N.ext` duplicates in album dirs (keeps shorter name; no token needed) |
| `-thorough` | With `-dedupe`: check all albums, not just over-count ones |
| `-dry-run` | With `-dedupe`: show what would be removed without deleting |
| `-retry` | With `-albums`: retry failures logged as `albumID\tmomentID` in `failed_downloads.txt` |

Same-name photos are compared by content (SHA256, or pixel-for-pixel for
JPEG/PNG): identical content is skipped, distinct content is saved as
`name_N.ext`, so reruns are idempotent.

## Google Photos transfer

Stream the whole library (and per-album copies) to a Google Photos account —
nothing is written to disk except the state file.

### One-time setup

1. Create a Google Cloud project (console.cloud.google.com)
2. Enable the **Photos Library API**
3. Configure the OAuth consent screen: **External**, status **Testing**
4. Add ONLY the Google Photos account's email as a **test user** — Testing
   status blocks every other Google account, which prevents signing into the
   wrong one
5. Create an OAuth client of type **Desktop app** and download its JSON to
   `~/.config/shutterfly/google-client.json`

> ⚠️ In Testing status, refresh tokens expire after 7 days — a long transfer
> may need re-consent on a later run. Quota is 10,000 requests/day; a 429
> stops cleanly and `re-run tomorrow` resumes from state.

### Commands

```bash
export SHUTTERFLY_TOKEN="$(cat token.tmp)" SHUTTERFLY_LIBRARY_ID=<library id>
./shutterfly-downloader -to-google -dry-run                                  # plan only (no Google calls)
./shutterfly-downloader -to-google -album Gunner -google-account you@gmail.com # small test first
./shutterfly-downloader -to-google -google-account you@gmail.com              # full transfer (re-run daily)
./shutterfly-downloader -status                                              # progress (no tokens needed)
```

Flags: `-workers` (default 10), `-limit N`, `-dry-run`, `-album NAME`,
`-google-account EMAIL`, `-yes`, `-google-client`, `-google-state`,
`-status`, `-reconcile`.

### Making sure it's the right Google account

- Sign-in always shows Google's account picker; Testing mode only allows the
  test users you listed.
- The tool prints `Signed in to Google as <email>` and asks for `y` before the
  first upload (`-yes` skips this only together with a matching
  `-google-account`).
- `-google-account` refuses to run if a different account is signed in.
- The confirmed account is pinned in the state DB; later runs signed into
  another account stop before uploading.
- To switch accounts on purpose: delete `~/.config/shutterfly/google-token.json`
  and use a new `-google-state` file (album/media IDs are per account).

Behavior:
- Every photo goes into a Google album `Shutterfly` (rolls over to
  `Shutterfly (2)` past 20,000 items). Each non-empty Shutterfly album also
  gets `Shutterfly - <name>`.
- Photos upload once; album membership uses `batchAddMediaItems`.
- Resume: the state DB (below) records uploaded items and memberships;
  re-running skips done items and fills missing memberships. Lost state
  recovers album IDs by title. Failed IDs go to `google-failed.txt`.
- Shutterfly tokens last ~23h: when one expires mid-run, save a fresh one to
  `token.txt` in the current directory or paste it at the prompt.
- Uploads count against the target account's Google storage; total bytes are
  printed at the end.

Note: if `LIFE_UID` isn't set, album mode falls back to
`SHUTTERFLY_LIBRARY_ID` before token claims — needed for accounts where the
album API's user ID is the library ID.

### State, locking and safety

- Transfer state lives in SQLite at `~/.config/shutterfly/google-state.db`
  (`-google-state`). Tables: `albums`, `items` (pending/creating/done/failed
  with media_item_id, sha256, attempts), `memberships`, `meta`.
- An exclusive flock (`<db>.lock`) prevents concurrent runs.
- `-status` prints counts/bytes/album progress/failures from the DB only.
- `-reconcile` repairs items left `creating` by a crash around batchCreate
  (matches by filename against app-created mediaItems; ambiguous cases are
  reported for manual check and never re-uploaded).
- Duplicates: identical bytes (sha256) upload only once — the second item
  links to the same media item. batchCreate items are marked `creating`
  before the call and `done`/`pending` after, so a crash mid-call is safe.
- Shutterfly token auto-refresh: the ID token is a Cognito token. Save your
  refresh token (DevTools → Application → Local Storage → key ending
  `.refreshToken`) to `~/.config/shutterfly/refresh-token` with `chmod 600`;
  it's used when the token is within 10 min of expiry. `-refresh-test` checks
  it once. Files with looser perms are refused.
  > Note: Cognito auto-refresh is currently blocked by Shutterfly's AWS WAF
  > (403 ForbiddenException from this network), so the token.txt/stdin prompt
  > fallback is the supported path. No WAF-evasion is attempted.
