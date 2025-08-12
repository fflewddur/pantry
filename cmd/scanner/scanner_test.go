package main

import (
	"log"
	"os"
	"testing"
)

func TestEndToEnd(t *testing.T) {
	if os.Getenv("PANTRY_TEST_E2E") == "" {
		t.Skip("Skipping end-to-end test; set PANTRY_TEST_E2E to run it")
		return
	}

	scanner := NewScanner()
	if scanner == nil {
		t.Fatal("Failed to create scanner")
	}
	log.Println("Starting end-to-end test for scanner...")
	scanner.Start()
	// TODO: Add a method to scan one specific URL with the scanner
}
