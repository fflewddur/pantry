package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	crdbpgx "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
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
	maxModules := 100                     // Limit the number of modules to fetch
	since := time.Now().AddDate(-1, 0, 0) // Set the time to 1 year ago
	urlBase := "https://index.golang.org/index"
	db, err := initDB()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close(context.Background())
	c := http.Client{}

	for len(modules) < maxModules {
		limit := min(2000, maxModules-len(modules))
		url := fmt.Sprintf("%s?since=%s&limit=%d", urlBase, since.Format(time.RFC3339), limit)
		log.Printf("Fetching modules since %s (%d of %d)", since.Format(time.RFC3339), len(modules), maxModules)
		log.Printf("Requesting URL: %s", url)
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
		log.Printf("Bytes received: %s", string(data))
		lines := bytes.Split(data, []byte("\n"))
		emptyLines := 0
		for _, line := range lines {
			if len(line) == 0 {
				emptyLines++
				continue // Skip empty lines
			}
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
		log.Printf("len(lines): %d, limit: %d, len(modules): %d, empty lines: %d", len(lines), limit, len(modules), emptyLines)
		if (len(lines)-emptyLines) <= limit && limit <= 1 {
			log.Printf("Received %d modules, which is less than the limit of %d. Stopping.", len(lines), limit)
			break // Stop if we received fewer modules than requested
		}
	}
	latest := fetchLatest(modules)
	downloadModules(latest, db)
	log.Printf("Processed %d modules since %s", len(modules), since.Format(time.RFC3339))
}

func initDB() (*pgx.Conn, error) {
	log.Println("Initializing database connection...")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://pantry:whatever@localhost:26257/pantry"
	}
	log.Printf("Using DATABASE_URL: %s", dbURL)
	config, err := pgx.ParseConfig(dbURL)
	if err != nil {
		log.Fatalf("Failed to parse DATABASE_URL: %v", err)
	}
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	err = crdbpgx.ExecuteTx(context.Background(), conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS mods (
		id INT64 DEFAULT unique_rowid(),
		path TEXT NOT NULL UNIQUE,
		version TEXT NOT NULL,
		readme TEXT,
		time TIMESTAMP);`)
		if err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
	log.Println("Database initialized successfully.")
	return conn, nil
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

func downloadModules(latest map[string]*Info, db *pgx.Conn) {
	count := 0
	for path, info := range latest {
		count++
		// TODO: If we already have the latest version of this module, don't download it again.
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
		err = crdbpgx.ExecuteTx(context.Background(), db, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO mods (path, version, readme, time) VALUES ($1, $2, $3, $4) ON CONFLICT (path) DO UPDATE SET version = $2, readme = $3, time = $4 WHERE excluded.path LIKE $1;`, mod.Path, mod.Version, mod.Readme, mod.Time)
			if err != nil {
				return fmt.Errorf("failed to insert row (%v): %w", mod, err)
			}
			return nil
		})
		log.Printf("length of mod.Readme: %d", len(mod.Readme))
		if err != nil {
			log.Fatalf("Failed to insert row: %v", err)
		}
		// _, err = db.Exec(`INSERT OR REPLACE INTO mods (path, version, readme, time) VALUES (?, ?, ?, ?)`,
		// 	mod.Path, mod.Version, mod.Readme, mod.Time)
		// if err != nil {
		// 	log.Printf("Failed to insert module %s into database: %v", mod.Path, err)
		// 	continue
		// }
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
		if !utf8.Valid(content) {
			log.Printf("File %s is not valid UTF-8, skipping", file.Name)
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
