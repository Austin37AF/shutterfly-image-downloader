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

## Notes

- Recommended workers: 10-100 (higher may cause rate limiting)
- File modification dates are set from EXIF DateTimeOriginal when available
- Photos without EXIF data will have today's date
- The downloader is idempotent - safe to run multiple times