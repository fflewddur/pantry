package main

import (
	"html/template"
	"log"
	"net/http"
)

func main() {
	log.Println("The server is starting...")
	http.HandleFunc("/", rootHandler)
	http.HandleFunc("/search", searchHandler)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
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

func searchHandler(w http.ResponseWriter, r *http.Request) {
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
