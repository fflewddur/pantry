package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	log.Println("The server is starting...")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, World!")
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
