package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Server struct {
	db        *sql.DB
	uploadDir string
	baseURL   string
	ttl       time.Duration
	indexTmpl *template.Template
}

func NewServer(uploadDir, baseURL string, ttl time.Duration) (*Server, error) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse index.html: %w", err)
	}

	// Initialize database
	dbPath := filepath.Join(uploadDir, "files.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	server := &Server{
		db:        db,
		uploadDir: uploadDir,
		baseURL:   baseURL,
		ttl:       ttl,
		indexTmpl: tmpl,
	}

	// Initialize database schema
	if err := server.initDB(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	// Load existing files from database and start cleanup goroutines
	if err := server.loadExistingFiles(); err != nil {
		log.Printf("Warning: failed to load existing files: %v", err)
	}

	return server, nil
}

func (s *Server) initDB() error {
	query := `
	CREATE TABLE IF NOT EXISTS files (
		hash TEXT PRIMARY KEY,
		filename TEXT NOT NULL,
		upload_time INTEGER NOT NULL,
		file_path TEXT NOT NULL
	)`
	_, err := s.db.Exec(query)
	return err
}

func (s *Server) loadExistingFiles() error {
	query := `SELECT hash, filename, upload_time, file_path FROM files`
	rows, err := s.db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var hash, filename, filePath string
		var uploadTimeUnix int64

		if err := rows.Scan(&hash, &filename, &uploadTimeUnix, &filePath); err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}

		uploadTime := time.Unix(uploadTimeUnix, 0)
		expiresAt := uploadTime.Add(s.ttl)

		// Check if file still exists on disk
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			// File doesn't exist, remove from database
			s.db.Exec("DELETE FROM files WHERE hash = ?", hash)
			continue
		}

		// If file hasn't expired yet, schedule deletion
		if expiresAt.After(now) {
			remainingTTL := expiresAt.Sub(now)
			go s.scheduleDelete(hash, remainingTTL)
		} else {
			// File has expired, delete it immediately
			go s.deleteFile(hash)
		}
	}

	return rows.Err()
}

func (s *Server) generateHash(filename string) string {
	data := fmt.Sprintf("%s-%d", filename, time.Now().UnixNano())
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:8]
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.downloadHandler(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	s.indexTmpl.Execute(w, nil)
}

func (s *Server) uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(100 << 20) // 100 MB max
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "No files uploaded", http.StatusBadRequest)
		return
	}

	type UploadedFile struct {
		Hash      string `json:"hash"`
		Filename  string `json:"filename"`
		Extension string `json:"extension"`
		URL       string `json:"url"`
	}

	var uploadedFiles []UploadedFile

	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("Error opening file: %v", err)
			continue
		}

		hash := s.generateHash(fileHeader.Filename)
		ext := filepath.Ext(fileHeader.Filename)
		filePath := filepath.Join(s.uploadDir, hash+ext)

		dst, err := os.Create(filePath)
		if err != nil {
			file.Close()
			log.Printf("Error creating file: %v", err)
			continue
		}

		_, err = io.Copy(dst, file)
		file.Close()
		dst.Close()

		if err != nil {
			os.Remove(filePath)
			log.Printf("Error saving file: %v", err)
			continue
		}

		uploadTime := time.Now()

		// Store file info in database
		_, err = s.db.Exec(
			"INSERT INTO files (hash, filename, upload_time, file_path) VALUES (?, ?, ?, ?)",
			hash, fileHeader.Filename, uploadTime.Unix(), filePath,
		)
		if err != nil {
			os.Remove(filePath)
			log.Printf("Error storing file info in database: %v", err)
			continue
		}

		uploadedFiles = append(uploadedFiles, UploadedFile{
			Hash:      hash,
			Filename:  fileHeader.Filename,
			Extension: ext,
			URL:       fmt.Sprintf("%s/%s%s", s.baseURL, hash, ext),
		})

		go s.scheduleDelete(hash, s.ttl)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success": true, "files": [`)
	for i, f := range uploadedFiles {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"hash": "%s", "filename": "%s", "extension": "%s", "url": "%s"}`,
			f.Hash, f.Filename, f.Extension, f.URL)
	}
	fmt.Fprint(w, `]}`)
}

func (s *Server) downloadHandler(w http.ResponseWriter, r *http.Request) {
	// Extract hash and extension from URL path (e.g., "/abc123.jpg" -> hash="abc123", ext=".jpg")
	// URLs without extensions (e.g., "/abc123") should not resolve
	path := r.URL.Path[1:] // Remove leading "/"
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Find the last dot to separate hash from extension
	ext := filepath.Ext(path)
	if ext == "" {
		// No extension means this URL should not resolve
		http.NotFound(w, r)
		return
	}

	hash := path[:len(path)-len(ext)]
	if hash == "" {
		http.NotFound(w, r)
		return
	}

	var filename, filePath string
	err := s.db.QueryRow(
		"SELECT filename, file_path FROM files WHERE hash = ?",
		hash,
	).Scan(&filename, &filePath)

	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("Error querying database: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Verify that the extension in the URL matches the stored file's extension
	storedExt := filepath.Ext(filePath)
	if storedExt != ext {
		// Extension mismatch - URL doesn't match stored file
		http.NotFound(w, r)
		return
	}

	// Check if file still exists on disk
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		// File doesn't exist, remove from database
		s.db.Exec("DELETE FROM files WHERE hash = ?", hash)
		http.NotFound(w, r)
		return
	}

	// Determine content type based on extension
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)

	// Check if file is an image - images should be rendered inline, not downloaded
	isImage := strings.HasPrefix(contentType, "image/")
	if isImage {
		// For images, serve inline so they render in the browser
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
	} else {
		// For other files, force download
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}

	http.ServeFile(w, r, filePath)
}

func (s *Server) scheduleDelete(hash string, ttl time.Duration) {
	time.Sleep(ttl)
	s.deleteFile(hash)
}

func (s *Server) deleteFile(hash string) {
	var filename, filePath string
	err := s.db.QueryRow(
		"SELECT filename, file_path FROM files WHERE hash = ?",
		hash,
	).Scan(&filename, &filePath)

	if err == sql.ErrNoRows {
		return // Already deleted
	}
	if err != nil {
		log.Printf("Error querying file for deletion: %v", err)
		return
	}

	// Delete file from disk
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		log.Printf("Error deleting file %s: %v", filePath, err)
	}

	// Delete from database
	_, err = s.db.Exec("DELETE FROM files WHERE hash = ?", hash)
	if err != nil {
		log.Printf("Error deleting file record from database: %v", err)
	} else {
		log.Printf("Deleted expired file: %s (%s)", filename, hash)
	}
}

func (s *Server) cleanupRoutine() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now().Unix()
		ttlSeconds := int64(s.ttl.Seconds())

		// Find expired files
		query := `SELECT hash, filename, file_path FROM files WHERE ? - upload_time > ?`
		rows, err := s.db.Query(query, now, ttlSeconds)
		if err != nil {
			log.Printf("Error querying expired files: %v", err)
			continue
		}

		for rows.Next() {
			var hash, filename, filePath string
			if err := rows.Scan(&hash, &filename, &filePath); err != nil {
				log.Printf("Error scanning expired file: %v", err)
				continue
			}

			// Delete file from disk
			if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
				log.Printf("Error deleting expired file %s: %v", filePath, err)
			}

			// Delete from database
			if _, err := s.db.Exec("DELETE FROM files WHERE hash = ?", hash); err != nil {
				log.Printf("Error deleting expired file record: %v", err)
			} else {
				log.Printf("Cleaned up expired file: %s (%s)", filename, hash)
			}
		}
		rows.Close()
	}
}

func main() {
	uploadDir := "./uploads"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("Failed to create upload directory:", err)
	}

	baseURL := "http://localhost:8080"
	ttl := 3 * time.Hour

	server, err := NewServer(uploadDir, baseURL, ttl)
	if err != nil {
		log.Fatal("Failed to create server:", err)
	}
	defer server.db.Close()

	go server.cleanupRoutine()

	http.HandleFunc("/", server.indexHandler)
	http.HandleFunc("/upload", server.uploadHandler)

	port := ":8080"
	log.Printf("Server starting on %s", port)
	log.Printf("Files will expire after %v", ttl)
	log.Fatal(http.ListenAndServe(port, nil))
}
