package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	log.Println("Starting the scanner...")
	modules := make(map[string]*ModuleEntry)
	gotResults := true
	maxModules := 100                     // Limit the number of modules to fetch
	since := time.Now().AddDate(0, 0, -1) // Set the time to 1 day ago
	urlBase := "https://index.golang.org/index"
	c := http.Client{}

	for gotResults && len(modules) < maxModules {
		log.Printf("Fetching modules since %s (%d of %d)", since.Format(time.RFC3339), len(modules), maxModules)

		url := fmt.Sprintf("%s?since=%s&limit=%d", urlBase, since.Format(time.RFC3339), maxModules-len(modules))
		resp, err := c.Get(url)
		if err != nil {
			log.Fatalf("Failed to make request: %v", err)
		}
		defer func() {
			err = errors.Join(err, resp.Body.Close())
		}()
		if resp.StatusCode != http.StatusOK {
			log.Fatalf("Unexpected status code: %d", resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Fatalf("Failed to read response body: %v", err)
		}
		count := 0
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			if len(line) == 0 {
				continue // Skip empty lines
			}
			count++
			var entry ModuleEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				log.Printf("Error unmarshaling response for line %s: %v", line, err)
				continue
			}
			_, found := modules[entry.Path]
			if !found {
				modules[entry.Path] = &entry
			}
			since = entry.Timestamp
		}
		if count <= 1 {
			gotResults = false // No more data to process
			log.Println("No more results to process, stopping the scanner.")
			continue
		}
	}
	log.Printf("Processed %d modules since %s", len(modules), since.Format(time.RFC3339))

	latest := fetchLatest(modules)
	downloadModules(latest)
}

func fetchLatest(modules map[string]*ModuleEntry) map[string]*Info {
	// https://proxy.golang.org/cached-only/github.com/fflewddur/bsky/@latest
	count := 0
	latest := make(map[string]*Info, len(modules))
	for _, mod := range modules {
		count++
		log.Printf("Fetching latest version for module: %s (%d of %d)", mod.Path, count, len(modules))
		url := fmt.Sprintf("https://proxy.golang.org/cached-only/%s/@latest", mod.Path)
		resp, err := http.Get(url)
		if err != nil {
			log.Printf("Failed to fetch latest version for %s: %v", mod.Path, err)
			continue
		}
		defer func() {
			err = errors.Join(err, resp.Body.Close())
		}()
		if resp.StatusCode != http.StatusOK {
			log.Printf("Unexpected status code for %s: %d", mod.Path, resp.StatusCode)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body for %s: %v", mod.Path, err)
			continue
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			log.Printf("Error unmarshaling response for %s: %v", mod.Path, err)
			continue
		}
		latest[mod.Path] = &info
		// log.Printf("Latest version for module %s is %s at %s", mod.Path, info.Version, info.Time.Format(time.RFC3339))
	}
	return latest
}

func downloadModules(latest map[string]*Info) {
	count := 0
	for path, info := range latest {
		count++
		log.Printf("Downloading module %s (%d of %d)", path, count, len(latest))
		url := fmt.Sprintf("https://proxy.golang.org/cached-only/%s/@v/%s.zip", path, info.Version)
		resp, err := http.Get(url)
		if err != nil {
			log.Printf("Failed to download module %s: %v", path, err)
			continue
		}
		defer func() {
			err = errors.Join(err, resp.Body.Close())
		}()
		if resp.StatusCode != http.StatusOK {
			log.Printf("Unexpected status code for %s: %d", path, resp.StatusCode)
			continue
		}
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body for %s: %v", path, err)
			continue
		}
		// TODO: parse the zip file and extract the contents
	}
}

type Info struct {
	Version string    // version string
	Time    time.Time // commit time
}

// ModuleEntry represents a single module entry in the Go index.
type ModuleEntry struct {
	Path      string    `json:"Path"`
	Version   string    `json:"Version"`
	Timestamp time.Time `json:"Timestamp"`
}
