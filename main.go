package main

import (
	"log"
	"net/http"

	"github.com/yourname/go-web-crawler/handlers"
)

func main() {
	http.HandleFunc("/", handlers.IndexHandler)
	http.HandleFunc("/submit", handlers.CrawlHandler)
	http.HandleFunc("/results", handlers.ResultsHandler)
	log.Println("Server running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
