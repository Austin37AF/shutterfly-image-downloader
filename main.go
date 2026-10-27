package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"shutterfly-image-downloader/internal/shutterfly"
)

func main() {
	token := flag.String("token", "", "Shutterfly access token (or set SHUTTERFLY_TOKEN)")
	libraryID := flag.String("library-id", "", "Shutterfly library ID (or set SHUTTERFLY_LIBRARY_ID)")
	lifeUID := flag.String("life-uid", "", "Shutterfly user ID (or set LIFE_UID)")
	outputDir := flag.String("output", "shutterfly_photos", "Output directory")
	rateLimit := flag.Float64("rate-limit", 0.05, "Delay between requests per worker (seconds)")
	workers := flag.Int("workers", 10, "Number of parallel download workers")
	countOnly := flag.Bool("count", false, "Only count photos (with -albums: per-album counts)")
	retryFailed := flag.Bool("retry", false, "Retry failed downloads from failed_downloads.txt")
	verbose := flag.Bool("verbose", false, "Debug logging")
	albums := flag.Bool("albums", false, "Album mode: download all albums")
	album := flag.String("album", "", "Album mode: download a single album by exact name")
	resumeFrom := flag.String("resume-from", "", "With -albums: resume from this album name")
	ignoreAlbums := flag.String("ignore-albums", "", "Comma-separated album names to ignore (plus output/ignore_albums.txt)")
	compare := flag.Bool("compare", false, "Compare local downloads with server data")
	fixIncomplete := flag.Bool("fix-incomplete", false, "Redownload albums with missing photos")
	dedupe := flag.Bool("dedupe", false, "Find and remove exact duplicate photos in album dirs (no token needed)")
	thorough := flag.Bool("thorough", false, "With -dedupe: check all albums even if counts match")
	dryRun := flag.Bool("dry-run", false, "With -dedupe: print what would be removed without deleting")
	flag.Parse()

	accessToken := *token
	if accessToken == "" {
		accessToken = os.Getenv("SHUTTERFLY_TOKEN")
	}

	libID := *libraryID
	if libID == "" {
		libID = os.Getenv("SHUTTERFLY_LIBRARY_ID")
	}
	if libID != "" {
		os.Setenv("SHUTTERFLY_LIBRARY_ID", libID)
	}

	// Dedupe is the only mode that works without a token.
	if accessToken == "" && !*dedupe {
		fmt.Print("Enter token: ")
		reader := bufio.NewReader(os.Stdin)
		accessToken, _ = reader.ReadString('\n')
		accessToken = strings.TrimSpace(accessToken)
	}
	if accessToken == "" && !*dedupe {
		fmt.Println("Error: token required")
		os.Exit(1)
	}

	d := shutterfly.NewClient(accessToken, *outputDir, *rateLimit, *verbose)
	d.LifeUID = *lifeUID
	if *ignoreAlbums != "" {
		for _, name := range strings.Split(*ignoreAlbums, ",") {
			if name = strings.TrimSpace(name); name != "" {
				d.IgnoreAlbums = append(d.IgnoreAlbums, name)
			}
		}
	}

	switch {
	case *dedupe:
		d.DedupeAll(*thorough, *dryRun)
	case *countOnly && *albums:
		d.CountAlbums()
	case *countOnly:
		d.Count()
	case *compare:
		d.CompareLocalVsServer()
	case *fixIncomplete:
		fmt.Println("\nStarting redownload of incomplete albums...")
		d.RedownloadIncompleteAlbums(*workers)
	case *album != "":
		fmt.Println("\nStarting single album download...")
		success, fail := d.DownloadSingleAlbum(*album, *workers)
		fmt.Printf("\nDownload complete!\nTotal successfully downloaded: %d\nTotal failed downloads: %d\n", success, fail)
	case *albums:
		if *retryFailed {
			fmt.Println("Retrying failed album downloads...")
			success, fail := d.RetryAlbumFailed(*workers)
			fmt.Printf("\nRetry complete! Success: %d, Still failed: %d\n", success, fail)
		} else {
			fmt.Println("\nStarting download...")
			success, fail := d.DownloadAlbums(*resumeFrom, *workers)
			fmt.Printf("\nDownload complete!\nTotal successfully downloaded: %d\nTotal failed downloads: %d\n", success, fail)
		}
	case *retryFailed:
		fmt.Println("Retrying failed downloads...")
		success, fail := d.RetryFailed(*workers)
		fmt.Printf("\nRetry complete! Success: %d, Still failed: %d\n", success, fail)
	default:
		fmt.Println("Starting download...")
		success, fail := d.DownloadAll(*workers)
		fmt.Printf("\nComplete! Success: %d, Failed: %d\n", success, fail)
		if fail > 0 {
			fmt.Printf("Failed IDs logged to: %s/failed_downloads.txt\n", *outputDir)
			fmt.Println("Run with -retry to retry failed downloads")
		}
	}
}
