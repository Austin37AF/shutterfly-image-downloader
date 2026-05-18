# Shutterfly Image Downloader

Simple Go tool to download all photos from Shutterfly. Skips files that already exist.

## Getting Your Token

Shutterfly doesn't have a public API, so you need to grab the token from your browser:

1. Log into https://photos.shutterfly.com
2. Open DevTools (F12 or Cmd+Option+I on Mac)
3. Go to the **Network** tab
4. Click on any album to trigger API requests
5. Find a request to `cmd.thislife.com`
6. In the request payload, copy the `accessToken` value

**Alternative:** Check **Application tab → Local Storage → `https://photos.shutterfly.com`** for a key containing "token".

> ⚠️ Tokens expire after a few hours. If you get auth errors, grab a fresh token.

## Build

```bash
go build -o shutterfly-downloader .
```

## Usage

```bash
# Download all albums
./shutterfly-downloader -token "YOUR_TOKEN"

# Or via environment variable
export SHUTTERFLY_TOKEN="YOUR_TOKEN"
./shutterfly-downloader

# Count albums/photos only
./shutterfly-downloader -count

# Custom output directory
./shutterfly-downloader -output ./my_photos
```

## Flags

- `-token` - Shutterfly access token (or set `SHUTTERFLY_TOKEN` env var)
- `-output` - Output directory (default: `shutterfly_photos`)
- `-rate-limit` - Delay between requests in seconds (default: 0.1)
- `-count` - Only count albums and photos, don't download
- `-verbose` - Debug logging