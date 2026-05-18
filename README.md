# Shutterfly Image Downloader

Simple Go tool to download all photos from Shutterfly. Features:
- Parallel downloads for speed
- Skips files that already exist
- Preserves original photo dates from EXIF metadata

## Getting Your Token and Library ID

Shutterfly doesn't have a public API, so you need to grab credentials from your browser:

1. Log into https://photos3.shutterfly.com
2. Open DevTools (F12 or Cmd+Option+I on Mac)
3. Go to the **Network** tab
4. Filter by `thislife` or `photos-api3`
5. Click on any photo/album to trigger API requests
6. Find a request to `photos-api3.shutterfly.com/photos/json?method=searchMoments`
7. Copy the **token** from the `Authorization: Bearer <token>` header
8. Copy the **library ID** from the request payload (second parameter in `params` array)

> ⚠️ Tokens expire after a few hours. If you get auth errors, grab a fresh token.

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

# Faster downloads (more workers, less delay)
./shutterfly-downloader -workers 15 -rate-limit 0.01
```

## Flags

- `-token` - Shutterfly access token (or set `SHUTTERFLY_TOKEN` env var)
- `-library-id` - Shutterfly library ID (or set `SHUTTERFLY_LIBRARY_ID` env var)
- `-output` - Output directory (default: `shutterfly_photos`)
- `-workers` - Number of parallel download workers (default: 10)
- `-rate-limit` - Delay between requests per worker in seconds (default: 0.05)
- `-count` - Only count photos, don't download
- `-verbose` - Debug logging

## Notes

- File modification dates are set from EXIF DateTimeOriginal when available
- Photos without EXIF data (screenshots, edited images) will have today's date
- The downloader automatically skips already-downloaded files, so it's safe to restart