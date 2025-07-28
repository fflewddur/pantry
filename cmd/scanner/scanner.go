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
	gotResults := true
	since := time.Now().AddDate(0, 0, -1) // Set the time to 7 days ago
	url := "https://index.golang.org/index"
	c := http.Client{}
	for gotResults {
		log.Printf("Fetching modules since %s", since.Format(time.RFC3339))
		resp, err := c.Get(fmt.Sprintf("%s?since=%s", url, since.Format(time.RFC3339)))
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
			// log.Printf("line: %s", line)
			if err := json.Unmarshal(line, &entry); err != nil {
				log.Printf("Error unmarshaling response for line %s: %v", line, err)
				continue
			}
			since = entry.Timestamp
			fmt.Printf("Module: %s, Version: %s, Timestamp: %s\n", entry.Path, entry.Version, entry.Timestamp)
		}
		if count <= 1 {
			gotResults = false // No more data to process
			log.Println("No more results to process, stopping the scanner.")
			continue
		}
	}
	// log.Printf("Received data: %s", data)

	log.Println("Scanner completed")
}

type ModuleEntry struct {
	Path      string    `json:"Path"`
	Version   string    `json:"Version"`
	Timestamp time.Time `json:"Timestamp"`
}
