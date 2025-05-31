package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/brotli"
	"github.com/joho/godotenv"

	"html/template"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Page struct {
	URL       string    `bson:"url"`
	Keywords  []string  `bson:"keywords"`
	Scores    []Score   `bson:"scores"`
	HTML      string    `bson:"html"`
	CrawlTime time.Time `bson:"crawl_time"`
}

type Score struct {
	Keyword string `bson:"keyword"`
	Count   int    `bson:"count"`
}

var mongoClient *mongo.Client

func init() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found")
	}

	// Get MongoDB URI from environment variable or use default
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
		log.Println("Using default MongoDB URI")
	}

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

	// Get and process keywords
	keywordsRaw := r.FormValue("keywords")
	if keywordsRaw == "" {
		log.Println("Keywords are empty")
		http.Error(w, "Keywords are required", http.StatusBadRequest)
		return
	}

	// Split keywords and clean them
	keywords := []string{}
	for _, k := range strings.Split(keywordsRaw, ",") {
		keyword := strings.ToLower(strings.TrimSpace(k))
		if keyword != "" {
			keywords = append(keywords, keyword)
		}
	}
	log.Printf("Processing keywords: %v", keywords)

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

	if resp.StatusCode != http.StatusOK {
		log.Printf("Error: received status code %d for URL: %s", resp.StatusCode, url)
		http.Error(w, fmt.Sprintf("Failed to fetch URL: status code %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	// Handle different encodings
	var reader io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			log.Printf("Error creating gzip reader: %v", err)
			http.Error(w, "Failed to decompress gzip response", http.StatusInternalServerError)
			return
		}
		defer gzReader.Close()
		reader = gzReader
	case "br":
		reader = brotli.NewReader(resp.Body)
		log.Printf("Using Brotli decompression")
	case "deflate":
		reader = resp.Body // net/http automatically handles deflate
	default:
		reader = resp.Body
	}

	// Read the entire response body
	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("Error reading response body: %v", err)
		http.Error(w, "Failed to read response body", http.StatusInternalServerError)
		return
	}

	// Create a new reader from the bytes for goquery
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("Error parsing HTML: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse HTML: %v", err), http.StatusInternalServerError)
		return
	}

	// Get the HTML content
	htmlContent, err := doc.Html()
	if err != nil {
		log.Printf("Error getting HTML: %v", err)
		htmlContent = string(bodyBytes) // fallback to raw bytes
	}

	// Get all text content
	textContent := doc.Find("body").Text()
	textContent = strings.ToLower(textContent)

	// Calculate scores for each keyword
	var scores []Score
	log.Printf("\n========== KEYWORD MATCHES ==========")
	for _, keyword := range keywords {
		count := strings.Count(textContent, keyword)
		scores = append(scores, Score{
			Keyword: keyword,
			Count:   count,
		})
		log.Printf("Keyword '%s' found %d times", keyword, count)
	}
	log.Printf("========== END KEYWORD MATCHES ==========\n")

	// Create page record
	page := Page{
		URL:       url,
		Keywords:  keywords,
		Scores:    scores,
		HTML:      htmlContent,
		CrawlTime: time.Now(),
	}

	// Save to MongoDB
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database("crawler").Collection("pages")
	_, err = collection.InsertOne(ctx, page)
	if err != nil {
		log.Printf("Error saving to MongoDB: %v", err)
		http.Error(w, fmt.Sprintf("Failed to save to database: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully saved page to database with %d keywords", len(keywords))

	// Print page statistics
	log.Printf("\n========== PAGE STATISTICS ==========")
	log.Printf("Total number of HTML elements: %d", doc.Find("*").Length())
	log.Printf("Number of links (a tags): %d", doc.Find("a").Length())
	log.Printf("Number of images (img tags): %d", doc.Find("img").Length())
	log.Printf("Number of paragraphs (p tags): %d", doc.Find("p").Length())
	log.Printf("Number of divs: %d", doc.Find("div").Length())
	log.Printf("Number of spans: %d", doc.Find("span").Length())
	log.Printf("Number of headers (h1-h6): %d", doc.Find("h1, h2, h3, h4, h5, h6").Length())
	log.Printf("Number of forms: %d", doc.Find("form").Length())
	log.Printf("========== END STATISTICS ==========\n")

	http.Redirect(w, r, "/results", http.StatusSeeOther)
	log.Println("Redirecting to results page")
}

func ResultsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database("crawler").Collection("pages")

	// Find the last 10 results, sorted by crawl time descending
	opts := options.Find().
		SetSort(bson.M{"crawl_time": -1}).
		SetLimit(10)

	cur, err := collection.Find(ctx, bson.M{}, opts)
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
