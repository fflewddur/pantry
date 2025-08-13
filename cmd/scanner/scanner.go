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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	crdbpgx "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/go-enry/go-license-detector/v4/licensedb"
	"github.com/go-enry/go-license-detector/v4/licensedb/api"
	"github.com/go-enry/go-license-detector/v4/licensedb/filer"
	"github.com/jackc/pgx/v5"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type Module struct {
	Id      int64
	Path    string
	Version string
	Readme  string
	Docs    string // Output of 'go doc -all'
	Desc    string // Description of the module, if available
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
		modPaths:   make(chan string, 1),
		toFetch:    make(chan *Module, 1),
		lFmt:       message.NewPrinter(language.Make(os.Getenv("LANG"))),
		scratchDir: scratchDir,
	}
}

func (s *Scanner) Start() {
	defer func() {
		err := s.db.Close(context.Background())
		if err != nil {
			log.Fatalf("Error closing database connection: %v", err)
		}
	}()
	err := os.MkdirAll(s.scratchDir, os.ModePerm) // Ensure the scratch directory exists
	if err != nil {
		log.Fatalf("Failed to create scratch directory %s: %v", s.scratchDir, err)
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(s.scratchDir)) // Clean up the scratch directory after processing
		if err != nil {
			log.Fatalf("Failed to remove scratch directory %s: %v", s.scratchDir, err)
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
		if os.Getenv("PANTRY_TEST_E2E") != "" {
			log.Println("PANTRY_TEST_E2E is set, testing fflewddur/ltbsky module...")
			mod := &Module{
				Path:    "github.com/fflewddur/ltbsky",
				Version: "v0.3.0",
				Time:    time.Now(),
			}
			s.toFetch <- mod
			return // Skip processing if we're in end-to-end test mode
		}
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
				log.Print(s.lFmt.Sprintf("Reached maximum number of modules (%d). Stopping.", counter))
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
	pr, err := s.parseModule(mod, data) // This function also unzips the module to /tmp
	if err != nil {
		return fmt.Errorf("failed to extract content for %s: %w", mod.Path, err)
	}
	err = crdbpgx.ExecuteTx(context.Background(), conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		err := tx.QueryRow(context.Background(), `INSERT INTO mods (path, version, readme, docs, time) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (path) DO UPDATE SET version = $2, readme = $3, docs = $4, time = $5 WHERE excluded.path LIKE $1 RETURNING id;`, mod.Path, mod.Version, mod.Readme, mod.Docs, mod.Time).Scan(&mod.Id)
		return err
	})
	if err != nil {
		log.Printf("length of mod.Readme: %d", len(mod.Readme))
		return fmt.Errorf("failed to insert module %s into database: %w", mod.Path, err)
	}
	err = crdbpgx.ExecuteTx(context.Background(), conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO modsmeta (id, license, licenses) VALUES ($1, $2, $3) ON CONFLICT (id) DO UPDATE SET license = $2, licenses = $3 WHERE excluded.id = $1;`, mod.Id, pr.PrimeLicense, pr.Licenses)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to insert module metadata %s into database: %w", mod.Path, err)
	}
	return nil
}

func (s *Scanner) parseModule(mod *Module, data []byte) (*parseResult, error) {
	// Create a regexp to match README files
	readmeRegex := regexp.MustCompile(`(?i)readme(\.md|\.txt)?$`)
	readmeContents := strings.Builder{}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to create zip reader: %w", err)
	}

	tmpDir, err := os.MkdirTemp(s.scratchDir, "mod-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(tmpDir)) // Clean up the temporary directory after processing
		if err != nil {
			log.Printf("Failed to remove temporary directory %s: %v", tmpDir, err)
		}
	}()

	// Save the module bytes to a temporary file
	zipPath := filepath.Join(tmpDir, "module.zip")
	err = os.WriteFile(zipPath, data, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to write module zip file %s: %w", zipPath, err)
	}
	// Unzip the module contents to the temporary directory
	mv := module.Version{
		Path:    mod.Path,
		Version: mod.Version,
	}
	log.Printf("Unzipping module %s version %s to %s", mod.Path, mod.Version, tmpDir)
	err = modzip.Unzip(filepath.Join(tmpDir, "unzipped"), mv, zipPath) // Unzip the module contents to the temporary directory
	if err != nil {
		return nil, fmt.Errorf("failed to unzip module %s: %w", mod.Path, err)
	}

	// in-memory processing: copy all README files to a single string
	for _, file := range reader.File {
		path := filepath.Join(tmpDir, file.Name)
		// log.Printf("Processing file: %s", path)
		if !strings.HasPrefix(path, filepath.Clean(tmpDir)+string(os.PathSeparator)) {
			fmt.Println("invalid file path")
			continue
		}
		rc, err := file.Open()
		if err != nil {
			log.Printf("Failed to open file %s in zip: %v", file.Name, err)
			continue
		}

		// Build a single string with all README contents
		if readmeRegex.MatchString(file.Name) {
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
		}
	}
	mod.Readme = readmeContents.String() // Set the Readme field to the collected content

	goPath, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("failed to find 'go' executable: %w", err)
	}
	cmd := exec.Command(goPath, "doc", "-all")  // Run 'go doc' command
	cmd.Dir = filepath.Join(tmpDir, "unzipped") // Set the working directory to the temporary directory
	// log.Printf("cmd.Dir: %s", cmd.Dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to run 'go doc' command: %v\n%v", err, string(output))
		// panic("Failed to run 'go doc' command") // Panic if 'go doc' fails
	} else {
		// log.Printf("doc output for module %s:\n%s", mod.Path, string(output))
		mod.Docs = string(output) // Store the output of 'go doc' in mod.Docs
	}

	// log.Printf("Detecting licenses from %s...", cmd.Dir)
	f, err := filer.FromDirectory(cmd.Dir)
	if err != nil {
		return nil, fmt.Errorf("failed to create filer from directory %s: %w", cmd.Dir, err)
	}
	licenses, err := licensedb.Detect(f)
	if err != nil {
		return nil, fmt.Errorf("failed to detect licenses: %w", err)
	}
	// for k, m := range licenses {
	// 	log.Printf("Detected license for module %s: %.2f, %s, %v (key: %s)", mod.Path, m.Confidence, m.File, m.Files, k)
	// }

	parseResult := &parseResult{
		Module:       mod,
		Licenses:     filterLicenses(licenses),
		PrimeLicense: primeLicense(licenses),
	}
	return parseResult, nil
}

type parseResult struct {
	Module       *Module
	Licenses     []string
	PrimeLicense string // The license with the highest confidence
}

const LICENSE_CONFIDENCE_THRESHOLD = 0.9 // Minimum confidence level for a license to be considered

func filterLicenses(licenses map[string]api.Match) []string {
	// Filter out licenses with confidence less than 0.9
	var filtered []string
	for k, m := range licenses {
		if m.Confidence >= LICENSE_CONFIDENCE_THRESHOLD {
			filtered = append(filtered, k)
		}
	}
	return filtered
}

// primeLicense returns the license with the highest confidence from the provided licenses map.
// If there are no licenses, it returns an empty string.
func primeLicense(licenses map[string]api.Match) string {
	// Return the license with the highest confidence
	var prime string
	maxConfidence := float32(0.0)
	for k, m := range licenses {
		if m.Confidence > LICENSE_CONFIDENCE_THRESHOLD && m.Confidence > maxConfidence {
			maxConfidence = m.Confidence
			prime = k
		}
	}
	return prime
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
		id INT64 PRIMARY KEY DEFAULT unique_rowid(),
		path TEXT NOT NULL UNIQUE,
		version TEXT NOT NULL,
		readme TEXT,
		docs TEXT,
		time TIMESTAMP);`)
		if err != nil {
			return fmt.Errorf("failed to create mods table: %w", err)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
	err = crdbpgx.ExecuteTx(context.Background(), conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS modsmeta (
		id INT64 PRIMARY KEY,
		license STRING,
		licenses STRING[]);`)
		if err != nil {
			return fmt.Errorf("failed to create modsmeta table: %w", err)
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
			return fmt.Errorf("failed to create utils table: %w", err)
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

func latestSeenVersion(path string, conn *pgx.Conn) string {
	var version string
	err := conn.QueryRow(context.Background(), "SELECT version FROM mods WHERE path LIKE $1", path).Scan(&version)
	if err != nil {
		return ""
	}
	return version
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
