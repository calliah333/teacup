package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxFileSize = 100 << 20 // 100 MB per file
	maxFormSize = 500 << 20 // 500 MB total form size (allows multiple files)
)

type Server struct {
	db         *sql.DB
	uploadDir  string
	baseURL    string
	ttl        time.Duration
	indexTmpl  *template.Template
	sessions   map[string]time.Time
	sessionsMu sync.RWMutex
	username   string
	password   string
}

func loadCredentials(configPath string) (string, string, error) {
	var username, password string

	// Config file must exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", "", fmt.Errorf("config file not found at %s", configPath)
	}

	file, err := os.Open(configPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse key=value format
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch strings.ToLower(key) {
		case "username":
			username = value
		case "password":
			password = value
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("error reading config file: %w", err)
	}

	if username == "" {
		return "", "", fmt.Errorf("username not found in config file")
	}

	if password == "" {
		return "", "", fmt.Errorf("password not found in config file")
	}

	return username, password, nil
}

func NewServer(uploadDir, baseURL string, ttl time.Duration, username, password string) (*Server, error) {
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
		sessions:  make(map[string]time.Time),
		username:  strings.TrimSpace(username),
		password:  strings.TrimSpace(password),
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

	// Start session cleanup goroutine
	go server.cleanupSessions()

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

func (s *Server) generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) isValidSession(sessionToken string) bool {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	expiry, exists := s.sessions[sessionToken]
	return exists && expiry.After(time.Now())
}

func (s *Server) createSession() (string, error) {
	token, err := s.generateSessionToken()
	if err != nil {
		return "", err
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.sessions[token] = time.Now().Add(24 * time.Hour)
	return token, nil
}

func (s *Server) deleteSession(sessionToken string) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	delete(s.sessions, sessionToken)
}

func (s *Server) cleanupSessions() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.sessionsMu.Lock()
		now := time.Now()
		for token, expiry := range s.sessions {
			if expiry.Before(now) {
				delete(s.sessions, token)
			}
		}
		s.sessionsMu.Unlock()
	}
}

func (s *Server) getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *Server) requireAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionToken := s.getSessionToken(r)
		if !s.isValidSession(sessionToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Authentication required",
			})
			return
		}
		fn(w, r)
	}
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if creds.Username != s.username || creds.Password != s.password {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid username or password",
		})
		return
	}

	sessionToken, err := s.createSession()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    sessionToken,
		Path:     "/",
		MaxAge:   86400, // 24 hours
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	sessionToken := s.getSessionToken(r)
	if sessionToken != "" {
		s.deleteSession(sessionToken)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

func (s *Server) checkAuthHandler(w http.ResponseWriter, r *http.Request) {
	sessionToken := s.getSessionToken(r)
	authenticated := s.isValidSession(sessionToken)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authenticated": authenticated,
	})
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

	// Set max memory for parsing form
	r.ParseMultipartForm(maxFormSize)

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "No files uploaded",
		})
		return
	}

	type UploadedFile struct {
		Hash      string `json:"hash"`
		Filename  string `json:"filename"`
		Extension string `json:"extension"`
		URL       string `json:"url"`
	}

	type UploadError struct {
		Filename string `json:"filename"`
		Error    string `json:"error"`
	}

	var uploadedFiles []UploadedFile
	var uploadErrors []UploadError

	for _, fileHeader := range files {
		// Check file size before processing
		if fileHeader.Size > maxFileSize {
			uploadErrors = append(uploadErrors, UploadError{
				Filename: fileHeader.Filename,
				Error:    fmt.Sprintf("File size (%d MB) exceeds maximum allowed size of %d MB", fileHeader.Size/(1<<20), maxFileSize/(1<<20)),
			})
			continue
		}

		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("Error opening file: %v", err)
			uploadErrors = append(uploadErrors, UploadError{
				Filename: fileHeader.Filename,
				Error:    "Failed to open file",
			})
			continue
		}

		hash := s.generateHash(fileHeader.Filename)
		ext := filepath.Ext(fileHeader.Filename)
		filePath := filepath.Join(s.uploadDir, hash+ext)

		dst, err := os.Create(filePath)
		if err != nil {
			file.Close()
			log.Printf("Error creating file: %v", err)
			uploadErrors = append(uploadErrors, UploadError{
				Filename: fileHeader.Filename,
				Error:    "Failed to create file",
			})
			continue
		}

		// Use LimitedReader to enforce size limit during copy
		limitedReader := io.LimitReader(file, maxFileSize+1)
		n, err := io.Copy(dst, limitedReader)
		file.Close()
		dst.Close()

		if err != nil {
			os.Remove(filePath)
			log.Printf("Error saving file: %v", err)
			uploadErrors = append(uploadErrors, UploadError{
				Filename: fileHeader.Filename,
				Error:    "Failed to save file",
			})
			continue
		}

		// Check if file exceeded limit during copy
		if n > maxFileSize {
			os.Remove(filePath)
			uploadErrors = append(uploadErrors, UploadError{
				Filename: fileHeader.Filename,
				Error:    fmt.Sprintf("File size exceeds maximum allowed size of %d MB", maxFileSize/(1<<20)),
			})
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

	response := map[string]interface{}{
		"success": true,
		"files":   uploadedFiles,
	}

	if len(uploadErrors) > 0 {
		response["errors"] = uploadErrors
	}

	json.NewEncoder(w).Encode(response)
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
	// Load credentials from config file
	configPath := "config"

	username, password, err := loadCredentials(configPath)
	if err != nil {
		log.Fatal("Failed to load credentials:", err)
	}

	uploadDir := "./uploads"
	if uploadEnv := os.Getenv("UPLOAD_DIR"); uploadEnv != "" {
		uploadDir = uploadEnv
	}
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("Failed to create upload directory:", err)
	}

	baseURL := "http://localhost:8080"
	if baseEnv := os.Getenv("BASE_URL"); baseEnv != "" {
		baseURL = baseEnv
	}
	ttl := 3 * time.Hour

	server, err := NewServer(uploadDir, baseURL, ttl, username, password)
	if err != nil {
		log.Fatal("Failed to create server:", err)
	}
	defer server.db.Close()

	go server.cleanupRoutine()

	http.HandleFunc("/", server.indexHandler)
	http.HandleFunc("/upload", server.requireAuth(server.uploadHandler))
	http.HandleFunc("/login", server.loginHandler)
	http.HandleFunc("/logout", server.logoutHandler)
	http.HandleFunc("/check-auth", server.checkAuthHandler)

	port := ":8080"
	log.Printf("Server starting on %s", port)
	log.Printf("Files will expire after %v", ttl)
	log.Fatal(http.ListenAndServe(port, nil))
}
