package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	crdbpgx "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
)

func main() {
	log.Println("The server is starting...")
	server := NewServer()
	server.Start()
	log.Println("Server started on :8080")
}

type Server struct {
	db *pgx.Conn
}

func NewServer() *Server {
	db, err := initDB()
	// db, err := sql.Open("sqlite", "file:./data/mods.db?mode=rw&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close(context.Background())
	// _, err = db.Exec(`CREATE TABLE IF NOT EXISTS mods (
	// 	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
	// 	path TEXT NOT NULL UNIQUE,
	// 	version TEXT NOT NULL,
	// 	readme TEXT,
	// 	time DATETIME NOT NULL
	// );`)
	if err != nil {
		log.Fatalf("Failed to create default database: %v", err)
	}
	return &Server{db: db}
}

func (s *Server) Start() {
	http.HandleFunc("/", s.rootHandler)
	http.HandleFunc("/search", s.searchHandler)
	http.HandleFunc("/mod/", s.modHandler)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func (s *Server) rootHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for search page")
	tmpl, err := template.New("search.html").ParseFiles("templates/search.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("Error writing response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

func (s *Server) searchHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for search results")
	q := r.URL.Query().Get("q")
	searchResults := &SearchResults{
		Query: q,
	}

	// TODO: Implement actual search logic here

	tmpl, err := template.New("results.html").ParseFiles("templates/results.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	err = tmpl.Execute(w, searchResults)
	if err != nil {
		log.Printf("Error writing response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

type SearchResults struct {
	Query string
}

func (s *Server) modHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for mod page")
	path := r.URL.Path[len("/mod/"):] // Extract the path after /mod/
	var version string
	var readme sql.NullString
	var t time.Time
	err := s.db.QueryRow(context.Background(), "SELECT version, time, readme FROM mods WHERE path = ?", path).Scan(&version, &t, &readme)
	if err != nil {
		if err == sql.ErrNoRows {
			log.Printf("Module %s not found", path)
			http.NotFound(w, r)
			return
		}
		log.Printf("Error querying database for module %s: %v", path, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	log.Printf("Module %s found: version=%s, time=%s", path, version, t.Format(time.RFC3339))
	modPageData := &ModPageData{
		Path:    path,
		Version: version,
		Readme:  readme.String,
		Time:    t,
	}
	tmpl, err := template.New("mod.html").ParseFiles("templates/mod.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	err = tmpl.Execute(w, modPageData)
	if err != nil {
		log.Printf("Error writing response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

type ModPageData struct {
	Path    string
	Version string
	Readme  string
	Time    time.Time
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
		path TEXT NOT NULL PRIMARY KEY,
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
