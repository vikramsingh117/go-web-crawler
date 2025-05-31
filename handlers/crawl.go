package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"html/template"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var mongoURI = "mongodb://localhost:27017"
var mongoClient *mongo.Client

func init() {
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongoClient, err = mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatal("Failed to connect to MongoDB:", err)
	}

	// Ping the database
	err = mongoClient.Ping(ctx, nil)
	if err != nil {
		log.Fatal("Failed to ping MongoDB:", err)
	}
}

type Page struct {
	URL   string `bson:"url"`
	Score int    `bson:"score"`
}

func IndexHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles("templates/index.html")
	if err != nil {
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}
	err = tmpl.Execute(w, nil)
	if err != nil {
		http.Error(w, "Failed to execute template", http.StatusInternalServerError)
		return
	}
}

func CrawlHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("Invalid method: %s, expected POST", r.Method)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	url := r.FormValue("url")
	if url == "" {
		log.Println("URL is empty")
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}
	log.Printf("Attempting to crawl URL: %s", url)

	keyword := strings.ToLower(r.FormValue("keyword"))
	if keyword == "" {
		log.Println("Keyword is empty")
		http.Error(w, "Keyword is required", http.StatusBadRequest)
		return
	}
	log.Printf("Searching for keyword: %s", keyword)

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // Allow redirects
		},
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	// Add more realistic headers
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	log.Printf("Sending request with headers: %+v", req.Header)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error fetching URL: %v", err)
		http.Error(w, fmt.Sprintf("Failed to fetch URL: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	log.Printf("Response status code: %d", resp.StatusCode)
	log.Printf("Response headers: %+v", resp.Header)

	if resp.StatusCode == http.StatusForbidden {
		log.Printf("Access forbidden (403) for URL: %s. This website might be blocking web crawlers.", url)
		http.Error(w, "This website is blocking our crawler. Please try a different website.", http.StatusBadRequest)
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("Error: received status code %d for URL: %s", resp.StatusCode, url)
		http.Error(w, fmt.Sprintf("Failed to fetch URL: status code %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	// Read the body with proper handling of compression
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			log.Printf("Error creating gzip reader: %v", err)
			http.Error(w, "Failed to decompress response", http.StatusInternalServerError)
			return
		}
		defer gzReader.Close()
		reader = gzReader
	}

	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("Error reading response body: %v", err)
		http.Error(w, "Failed to read response body", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully read %d bytes from response", len(bodyBytes))
	log.Printf("First 100 bytes of raw response: %q", bodyBytes[:min(100, len(bodyBytes))])

	// Create a new reader from the bytes for goquery
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("Error parsing HTML: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse HTML: %v", err), http.StatusInternalServerError)
		return
	}

	// Get the text content, but clean it up first
	var body string
	if doc.Find("#mw-content-text").Length() > 0 {
		// For Wikipedia pages
		body = doc.Find("#mw-content-text").Text()
	} else if doc.Find("article, [role='main'], main, #main, .main-content").Length() > 0 {
		// For other common content areas
		body = doc.Find("article, [role='main'], main, #main, .main-content").First().Text()
	} else {
		// Fallback to body but exclude common navigation elements
		doc.Find("nav, header, footer, .navigation, #navigation, .menu, #menu").Remove()
		body = doc.Find("body").Text()
	}

	body = strings.ToLower(body) // Convert to lowercase for case-insensitive search
	score := strings.Count(body, keyword)
	log.Printf("Keyword %q appears %d times in the text", keyword, score)

	page := Page{
		URL:   url,
		Score: score,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database("crawler").Collection("pages")
	_, err = collection.InsertOne(ctx, page)
	if err != nil {
		log.Printf("Error saving to MongoDB: %v", err)
		http.Error(w, fmt.Sprintf("Failed to save to database: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully saved page to database with score %d", score)

	http.Redirect(w, r, "/results", http.StatusSeeOther)
	log.Println("Redirecting to results page")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ResultsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database("crawler").Collection("pages")

	cur, err := collection.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"score": -1}))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch results: %v", err), http.StatusInternalServerError)
		return
	}
	defer cur.Close(ctx)

	var results []Page
	if err = cur.All(ctx, &results); err != nil {
		http.Error(w, fmt.Sprintf("Failed to decode results: %v", err), http.StatusInternalServerError)
		return
	}

	tmpl, err := template.ParseFiles("templates/results.html")
	if err != nil {
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, results)
	if err != nil {
		http.Error(w, "Failed to execute template", http.StatusInternalServerError)
		return
	}
}
