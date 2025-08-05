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
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	crdbpgx "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type Module struct {
	Id      int64
	Path    string
	Version string
	Readme  string
	Time    time.Time
}

type Scanner struct {
	db         *pgx.Conn
	httpClient *http.Client
	modPaths   chan string
	toFetch    chan *Module
	lFmt       *message.Printer // For localized messages
	scratchDir string           // Temporary directory for downloaded modules
}

const modIndexLimit = 500

func NewScanner() *Scanner {
	log.Println("Initializing scanner...")
	db, err := initDB()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	scratchDir := os.Getenv("PANTRY_SCRATCH")
	if scratchDir == "" {
		scratchDir = filepath.Join(os.TempDir(), "pantry")
	}
	return &Scanner{
		db:         db,
		httpClient: &http.Client{},
		modPaths:   make(chan string, 100),
		toFetch:    make(chan *Module, 100),
		lFmt:       message.NewPrinter(language.Make(os.Getenv("LANG"))),
		scratchDir: scratchDir,
	}
}

func (s *Scanner) Start() {
	defer func() {
		err := s.db.Close(context.Background())
		if err != nil {
			log.Printf("Error closing database connection: %v", err)
		}
	}()
	err := os.MkdirAll(s.scratchDir, os.ModePerm) // Ensure the scratch directory exists
	if err != nil {
		log.Fatalf("Failed to create scratch directory %s: %v", s.scratchDir, err)
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(s.scratchDir)) // Clean up the scratch directory after processing
		if err != nil {
			log.Printf("Failed to remove scratch directory %s: %v", s.scratchDir, err)
		}
	}()
	counter := 0
	maxModules := 10_000 // Limit the number of modules to fetch

	// Start up workers
	go func() {
		// Watch for modules paths and determine if we should download the latest version of each
		conn, err := initDB()
		if err != nil {
			log.Fatalf("Failed to initialize database connection: %v", err)
		}
		defer func() {
			err = errors.Join(err, conn.Close(context.Background()))
		}()
		seen := make(map[string]bool)
		for path := range s.modPaths {
			log.Printf("Processing module path: %s", path)
			if seen[path] {
				continue // Skip if we've already seen this path
			}
			seen[path] = true
			vSeen := latestSeenVersion(path, conn) // Check the latest version for this module path
			info, err := getLatestModInfo(path)
			if err != nil {
				log.Printf("Error fetching latest version for %s: %v", path, err)
				continue // Skip this module if we can't fetch the latest version
			}
			if info.Version == vSeen {
				log.Printf("Module %s is already at the latest version %s, skipping.", path, info.Version)
				continue // Skip if the latest version is already seen
			}
			mod := &Module{
				Path:    path,
				Version: info.Version,
				Time:    info.Time,
			}
			s.toFetch <- mod
		}
	}()

	go func() {
		// Watch for modules to fetch and download them
		conn, err := initDB()
		if err != nil {
			log.Fatalf("Failed to initialize database connection: %v", err)
		}
		defer func() {
			err = errors.Join(err, conn.Close(context.Background()))
		}()
		for mod := range s.toFetch {
			err := s.downloadModule(mod, conn)
			if err != nil {
				log.Printf("Error downloading module %s: %v", mod.Path, err)
				continue // Skip this module if we can't download it
			}
			m := s.lFmt.Sprintf("Successfully parsed module %s version %s (%d of %d)", mod.Path, mod.Version, counter, maxModules)
			log.Print(m)
			counter++
		}
	}()

	// modules := make(map[string]*ModuleEntry)
	since := s.getMostRecentFetchTime()
	urlBase := "https://index.golang.org/index"

	for counter < maxModules {
		url := fmt.Sprintf("%s?since=%s&limit=%d", urlBase, since.Format(time.RFC3339), modIndexLimit)
		// log.Printf("Fetching modules since %s (%d of %d)", since.Format(time.RFC3339), len(modules), maxModules)
		log.Printf("Requesting URL: %s", url)
		resp, err := s.httpClient.Get(url)
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
		// log.Printf("Bytes received: %s", string(data))
		lines := bytes.Split(data, []byte("\n"))
		emptyLines := 0
		for _, line := range lines {
			if counter >= maxModules {
				m := s.lFmt.Sprintf("Reached maximum number of modules (%d). Stopping.", counter)
				log.Print(m)
				break // Stop if we reached the maximum number of modules
			}
			if len(line) == 0 {
				emptyLines++
				continue // Skip empty lines
			}
			var entry ModuleEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				log.Printf("Error unmarshaling response for line %s: %v", line, err)
				continue // Skip errors when unmarshaling
			}
			s.modPaths <- entry.Path
			since = entry.Timestamp
		}

		_, err = s.db.Exec(context.Background(), `INSERT INTO utils (key, value) VALUES ('since', $1) ON 
		CONFLICT (key) DO UPDATE SET value = $1 WHERE excluded.key LIKE 'since';`, since.Format(time.RFC3339))
		if err != nil {
			log.Printf("Error updating 'since' in database: %v", err)
		}

		// log.Printf("len(lines): %d, limit: %d, len(modules): %d, empty lines: %d", len(lines), limit, len(modules), emptyLines)
		if (len(lines) - emptyLines) < modIndexLimit {
			log.Printf("Received %d modules, which is less than the limit of %d. Stopping.", len(lines)-emptyLines, modIndexLimit)
			break // Stop if we received fewer modules than requested
		}
	}
	m := s.lFmt.Sprintf("Processed %d modules up to %s", counter, since.Format(time.RFC3339))
	log.Print(m)
}

func (s *Scanner) getMostRecentFetchTime() time.Time {
	var t time.Time
	var str string
	err := s.db.QueryRow(context.Background(), `SELECT value FROM utils WHERE key LIKE 'since'`).Scan(&str)
	if err != nil {
		if err == pgx.ErrNoRows {
			log.Println("No modules found in the database. Starting from 0.")
			return time.Time{}
		}
		log.Printf("Error querying latest fetch time: %v", err)
		return time.Time{} // Return zero time if there's an error
	}

	log.Printf("Most recent fetch time: %s", str)
	t, err = time.Parse(time.RFC3339, str)
	if err != nil {
		log.Printf("Error parsing time from database: %v", err)
		return time.Time{} // Return zero time if parsing fails
	}
	return t
}

func main() {
	log.Println("Starting the scanner...")
	scanner := NewScanner()
	scanner.Start()
	log.Println("Scanner finished.")
	os.Exit(0) // Exit with success code
}

func initDB() (*pgx.Conn, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://pantry:whatever@localhost:26257/pantry"
	}
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
	err = crdbpgx.ExecuteTx(context.Background(), conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS utils (
		key STRING NOT NULL PRIMARY KEY,
		value STRING);`)
		if err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
	return conn, nil
}

func getLatestModInfo(path string) (*Info, error) {
	url := fmt.Sprintf("https://proxy.golang.org/cached-only/%s/@latest", path)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch latest version for %s: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code for %s: %d", path, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body for %s: %w", path, err)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("error unmarshaling response for %s: %w", path, err)
	}
	return &info, nil
}

func (s *Scanner) downloadModule(mod *Module, conn *pgx.Conn) error {
	url := fmt.Sprintf("https://proxy.golang.org/cached-only/%s/@v/%s.zip", mod.Path, mod.Version)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download module %s: %w", mod.Path, err)
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code for %s: %d", mod.Path, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body for %s: %w", mod.Path, err)
	}
	txt, err := s.extractContent(data) // This function also unzips the module to /tmp
	if err != nil {
		return fmt.Errorf("failed to extract content for %s: %w", mod.Path, err)
	}
	mod.Readme = txt
	err = crdbpgx.ExecuteTx(context.Background(), conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO mods (path, version, readme, time) VALUES ($1, $2, $3, $4) ON CONFLICT (path) DO UPDATE SET version = $2, readme = $3, time = $4 WHERE excluded.path LIKE $1;`, mod.Path, mod.Version, mod.Readme, mod.Time)
		return err
	})
	if err != nil {
		log.Printf("length of mod.Readme: %d", len(mod.Readme))
		return fmt.Errorf("failed to insert module %s into database: %w", mod.Path, err)
	}
	return nil
}

func latestSeenVersion(path string, conn *pgx.Conn) string {
	var version string
	err := conn.QueryRow(context.Background(), "SELECT version FROM mods WHERE path LIKE $1", path).Scan(&version)
	if err != nil {
		return ""
	}
	return version
}

func (s *Scanner) extractContent(data []byte) (string, error) {
	// Create a regexp to match README files
	readmeRegex := regexp.MustCompile(`(?i)readme(\.md|\.txt)?$`)
	readmeContents := strings.Builder{}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to create zip reader: %w", err)
	}
	tmpDir, err := os.MkdirTemp(s.scratchDir, "mod-")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(tmpDir)) // Clean up the temporary directory after processing
		if err != nil {
			log.Printf("Failed to remove temporary directory %s: %v", tmpDir, err)
		}
	}()

	for _, file := range reader.File {
		path := filepath.Join(tmpDir, file.Name)
		log.Printf("Processing file: %s", path)
		if !strings.HasPrefix(path, filepath.Clean(tmpDir)+string(os.PathSeparator)) {
			fmt.Println("invalid file path")
			continue
		}
		if file.FileInfo().IsDir() {
			fmt.Println("creating directory...")
			err := os.MkdirAll(path, os.ModePerm)
			if err != nil {
				log.Printf("Failed to create directory %s: %v", path, err)
				continue
			}
		} else {
			dirPath := filepath.Dir(path)
			err := os.MkdirAll(dirPath, os.ModePerm)
			if err != nil {
				log.Printf("Failed to create directory %s: %v", dirPath, err)
				continue
			}
		}

		rc, err := file.Open()
		if err != nil {
			log.Printf("Failed to open file %s in zip: %v", file.Name, err)
			continue
		}
		defer func() {
			err = errors.Join(err, rc.Close())
		}()

		dstFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			log.Printf("Failed to create file %s: %v", file.Name, err)
			continue
		}
		defer func() {
			err = errors.Join(err, dstFile.Close())
		}()

		_, err = io.Copy(dstFile, rc)
		if err != nil {
			log.Printf("Failed to copy content of file %s: %v", file.Name, err)
			continue
		}

		// Build a single string with all README contents
		if readmeRegex.MatchString(file.Name) {
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
		}
	}
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
