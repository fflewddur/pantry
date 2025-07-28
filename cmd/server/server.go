package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	log.Println("The server is starting...")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := fmt.Fprintf(w, "Hello, World!")
		if err != nil {
			log.Printf("Error writing response: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
