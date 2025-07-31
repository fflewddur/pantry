package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Module struct {
	Id      int64
	Path    string
	Version string
	Readme  string
	Time    time.Time
}

func main() {
	log.Println("Starting the scanner...")
	modules := make(map[string]*ModuleEntry)
	gotResults := true
	maxModules := 10                      // Limit the number of modules to fetch
	since := time.Now().AddDate(0, 0, -1) // Set the time to 1 day ago
	urlBase := "https://index.golang.org/index"
	// Initialize the database connection
	var db *sql.DB
	db, err := sql.Open("sqlite", "file:./data/mods.db?mode=rw&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS mods (
		id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL UNIQUE,
		version TEXT NOT NULL,
		readme TEXT,
		time DATETIME NOT NULL
	);`)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
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
	latest := fetchLatest(modules)
	downloadModules(latest, db)
	log.Printf("Processed %d modules since %s", len(modules), since.Format(time.RFC3339))
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

func downloadModules(latest map[string]*Info, db *sql.DB) {
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
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body for %s: %v", path, err)
			continue
		}
		mod := &Module{
			Path:    path,
			Version: info.Version,
			Time:    info.Time,
		}
		txt, err := extractContent(data)
		if err != nil {
			log.Printf("Failed to extract content for %s: %v", path, err)
			continue
		}
		mod.Readme = txt
		db.Exec(`INSERT OR REPLACE INTO mods (path, version, readme, time) VALUES (?, ?, ?, ?)`,
			mod.Path, mod.Version, mod.Readme, mod.Time)
	}
}

func extractContent(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to create zip reader: %w", err)
	}
	// Create a regexp to match README files
	readmeRegex := regexp.MustCompile(`(?i)readme(\.md|\.txt)?$`)
	foundReadme := false
	readmeContents := strings.Builder{}
	for _, file := range reader.File {
		// Just look at the README files
		if !readmeRegex.MatchString(file.Name) {
			continue
		}
		foundReadme = true
		log.Printf("Extracting file: %s (compressed size: %d, uncompressed size: %d)", file.Name, file.CompressedSize64, file.UncompressedSize64)
		rc, err := file.Open()
		if err != nil {
			log.Printf("Failed to open file %s in zip: %v", file.Name, err)
			continue
		}
		defer func() {
			err = errors.Join(err, rc.Close())
		}()
		content, err := io.ReadAll(rc)
		if err != nil {
			log.Printf("Failed to read content of file %s: %v", file.Name, err)
			continue
		}
		readmeContents.Write(content)
		readmeContents.WriteByte('\n') // Add a newline for separation
		// log.Printf("Content of %s: %s\n", file.Name, content) // For demonstration purposes
	}
	if !foundReadme {
		log.Printf("No README files found in the zip archive:")
		for _, file := range reader.File {
			log.Printf("  - %s (compressed size: %d, uncompressed size: %d)", file.Name, file.CompressedSize64, file.UncompressedSize64)
		}
	}
	log.Println("Finished extracting content from zip file.")
	return readmeContents.String(), nil
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
