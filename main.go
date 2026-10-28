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
	dryRun := flag.Bool("dry-run", false, "With -dedupe or -to-google: print what would happen without doing it")
	toGoogle := flag.Bool("to-google", false, "Transfer all photos to Google Photos (streaming, resumable)")
	googleClient := flag.String("google-client", "", "Google OAuth client JSON (default ~/.config/shutterfly/google-client.json)")
	googleState := flag.String("google-state", "", "Transfer state file (default ~/.config/shutterfly/google-state.json)")
	limit := flag.Int("limit", 0, "Limit number of moments transferred (0 = all)")
	shutterflyRefresh := flag.String("shutterfly-refresh", "", "Cognito refresh token file (default ~/.config/shutterfly/refresh-token)")
	refreshTest := flag.Bool("refresh-test", false, "Test the Cognito token refresh and exit")
	statusFlag := flag.Bool("status", false, "Print transfer status from the state DB and exit")
	reconcileFlag := flag.Bool("reconcile", false, "Reconcile items left 'creating' by a crash and exit")
	googleAccount := flag.String("google-account", "", "Expected Google account email; refuses to run if signed in differently")
	yesFlag := flag.Bool("yes", false, "Skip the first-run account confirmation (requires -google-account)")
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

	// Dedupe, -status and -reconcile work without a Shutterfly token.
	needsToken := !*dedupe && !*statusFlag && !*reconcileFlag
	if accessToken == "" && needsToken {
		fmt.Print("Enter token: ")
		reader := bufio.NewReader(os.Stdin)
		accessToken, _ = reader.ReadString('\n')
		accessToken = strings.TrimSpace(accessToken)
	}
	if accessToken == "" && needsToken {
		fmt.Println("Error: token required")
		os.Exit(1)
	}

	d := shutterfly.NewClient(accessToken, *outputDir, *rateLimit, *verbose)
	d.LifeUID = *lifeUID
	if *shutterflyRefresh != "" {
		d.RefreshFile = *shutterflyRefresh
	} else {
		d.RefreshFile = os.Getenv("HOME") + "/.config/shutterfly/refresh-token"
	}
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
	case *refreshTest:
		if err := d.RefreshTest(); err != nil {
			os.Exit(1)
		}
	case *statusFlag:
		statePath := *googleState
		if statePath == "" {
			statePath = os.Getenv("HOME") + "/.config/shutterfly/google-state.db"
		}
		st, err := shutterfly.OpenStateDB(statePath)
		if err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}
		defer st.Close()
		if err := st.Status(); err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}
	case *reconcileFlag:
		statePath := *googleState
		if statePath == "" {
			statePath = os.Getenv("HOME") + "/.config/shutterfly/google-state.db"
		}
		st, err := shutterfly.OpenStateDB(statePath)
		if err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}
		defer st.Close()
		clientPath := *googleClient
		if clientPath == "" {
			clientPath = os.Getenv("HOME") + "/.config/shutterfly/google-client.json"
		}
		g, err := shutterfly.NewGoogleClient(clientPath, os.Getenv("HOME")+"/.config/shutterfly/google-token.json", *verbose)
		if err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}
		if err := shutterfly.Reconcile(g, st); err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}
	case *toGoogle:
		clientPath := *googleClient
		if clientPath == "" {
			clientPath = os.Getenv("HOME") + "/.config/shutterfly/google-client.json"
		}
		statePath := *googleState
		if statePath == "" {
			statePath = os.Getenv("HOME") + "/.config/shutterfly/google-state.db"
		}
		var g *shutterfly.GoogleClient
		if !*dryRun {
			var err error
			g, err = shutterfly.NewGoogleClient(clientPath, os.Getenv("HOME")+"/.config/shutterfly/google-token.json", *verbose)
			if err != nil {
				fmt.Println("Error:", err)
				os.Exit(1)
			}
		}
		stats, err := d.ToGoogle(g, statePath, *workers, *limit, *album, *dryRun, *googleAccount, *yesFlag)
		if err != nil {
			if stats == nil {
				fmt.Println("Error:", err)
			}
			os.Exit(1)
		}
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
