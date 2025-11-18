package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	maxFileSize int64 = 100 << 20 // 100 MB per file (default)
)

type FileRecord struct {
	Hash       string    `json:"hash"`
	Filename   string    `json:"filename"`
	UploadTime time.Time `json:"upload_time"`
	FilePath   string    `json:"file_path"`
	TTLSeconds int64     `json:"ttl_seconds,omitempty"`
	Permanent  bool      `json:"permanent,omitempty"`
}

type URLRecord struct {
	ShortCode   string    `json:"short_code"`
	OriginalURL string    `json:"original_url"`
	CreatedTime time.Time `json:"created_time"`
}

type Server struct {
	filesJSON  string
	filesMu    sync.RWMutex
	urlsJSON   string
	urlsMu     sync.RWMutex
	uploadDir  string
	ttl        time.Duration
	indexTmpl  *template.Template
	sessions   map[string]time.Time
	sessionsMu sync.RWMutex
	username   string
	password   string
}

func loadCredentials(configPath string) (string, string, string, int64, error) {
	var username, password, port string
	var maxFileSizeMB int64 = 100 // defaults

	// Config file must exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", "", "", 0, fmt.Errorf("config file not found at %s", configPath)
	}

	file, err := os.Open(configPath)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("failed to open config file: %w", err)
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
		case "port":
			port = value
		case "max_file_size_mb":
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				maxFileSizeMB = v
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", "", 0, fmt.Errorf("error reading config file: %w", err)
	}

	if username == "" {
		return "", "", "", 0, fmt.Errorf("username not found in config file")
	}

	if password == "" {
		return "", "", "", 0, fmt.Errorf("password not found in config file")
	}

	// Default port if not specified
	if port == "" {
		port = "8080"
	}

	// Normalize port format (ensure it starts with ":")
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	return username, password, port, maxFileSizeMB, nil
}

func NewServer(uploadDir string, ttl time.Duration, username, password string) (*Server, error) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse index.html: %w", err)
	}

	filesJSON := filepath.Join(uploadDir, "files.json")
	urlsJSON := filepath.Join(uploadDir, "urls.json")

	server := &Server{
		filesJSON: filesJSON,
		urlsJSON:  urlsJSON,
		uploadDir: uploadDir,
		ttl:       ttl,
		indexTmpl: tmpl,
		sessions:  make(map[string]time.Time),
		username:  strings.TrimSpace(username),
		password:  strings.TrimSpace(password),
	}

	// Initialize JSON files if they don't exist
	if err := server.initFiles(); err != nil {
		return nil, fmt.Errorf("failed to initialize files storage: %w", err)
	}
	if err := server.initURLs(); err != nil {
		return nil, fmt.Errorf("failed to initialize URLs storage: %w", err)
	}

	// Load existing files from JSON and start cleanup goroutines
	if err := server.loadExistingFiles(); err != nil {
		log.Printf("Warning: failed to load existing files: %v", err)
	}

	// Load existing URLs from JSON and start cleanup goroutines
	if err := server.loadExistingURLs(); err != nil {
		log.Printf("Warning: failed to load existing URLs: %v", err)
	}

	// Start session cleanup goroutine
	go server.cleanupSessions()

	return server, nil
}

func (s *Server) initFiles() error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()

	// If file doesn't exist, create it with empty array
	if _, err := os.Stat(s.filesJSON); os.IsNotExist(err) {
		return s.saveFilesLocked([]FileRecord{})
	}
	return nil
}

func (s *Server) initURLs() error {
	s.urlsMu.Lock()
	defer s.urlsMu.Unlock()

	// If file doesn't exist, create it with empty array
	if _, err := os.Stat(s.urlsJSON); os.IsNotExist(err) {
		return s.saveURLsLocked([]URLRecord{})
	}
	return nil
}

func (s *Server) loadFiles() ([]FileRecord, error) {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()

	return s.loadFilesLocked()
}

func (s *Server) loadFilesLocked() ([]FileRecord, error) {
	// If file doesn't exist, return empty array
	if _, err := os.Stat(s.filesJSON); os.IsNotExist(err) {
		return []FileRecord{}, nil
	}

	data, err := os.ReadFile(s.filesJSON)
	if err != nil {
		return nil, err
	}

	var files []FileRecord
	if len(data) == 0 {
		return []FileRecord{}, nil
	}

	if err := json.Unmarshal(data, &files); err != nil {
		return nil, err
	}

	return files, nil
}

func (s *Server) saveFiles(files []FileRecord) error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	return s.saveFilesLocked(files)
}

func (s *Server) saveFilesLocked(files []FileRecord) error {
	data, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.filesJSON, data, 0644)
}

func (s *Server) loadURLs() ([]URLRecord, error) {
	s.urlsMu.RLock()
	defer s.urlsMu.RUnlock()

	return s.loadURLsLocked()
}

func (s *Server) loadURLsLocked() ([]URLRecord, error) {
	// If file doesn't exist, return empty array
	if _, err := os.Stat(s.urlsJSON); os.IsNotExist(err) {
		return []URLRecord{}, nil
	}

	data, err := os.ReadFile(s.urlsJSON)
	if err != nil {
		return nil, err
	}

	var urls []URLRecord
	if len(data) == 0 {
		return []URLRecord{}, nil
	}

	if err := json.Unmarshal(data, &urls); err != nil {
		return nil, err
	}

	return urls, nil
}

func (s *Server) saveURLs(urls []URLRecord) error {
	s.urlsMu.Lock()
	defer s.urlsMu.Unlock()
	return s.saveURLsLocked(urls)
}

func (s *Server) saveURLsLocked(urls []URLRecord) error {
	data, err := json.MarshalIndent(urls, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.urlsJSON, data, 0644)
}

func (s *Server) loadExistingFiles() error {
	files, err := s.loadFiles()
	if err != nil {
		return err
	}

	now := time.Now()
	var validFiles []FileRecord

	for _, file := range files {
		var expiresAt time.Time
		if file.Permanent {
			// No expiry
		} else if file.TTLSeconds > 0 {
			expiresAt = file.UploadTime.Add(time.Duration(file.TTLSeconds) * time.Second)
		} else {
			// Backward compatibility: use server default TTL if not set
			expiresAt = file.UploadTime.Add(s.ttl)
		}

		// Check if file still exists on disk
		if _, err := os.Stat(file.FilePath); os.IsNotExist(err) {
			// File doesn't exist, skip it (will be removed)
			continue
		}

		validFiles = append(validFiles, file)

		// If file hasn't expired yet, schedule deletion
		if !file.Permanent && !expiresAt.IsZero() && expiresAt.After(now) {
			remainingTTL := expiresAt.Sub(now)
			go s.scheduleDelete(file.Hash, remainingTTL)
		} else if !file.Permanent && !expiresAt.IsZero() {
			// File has expired, delete it immediately
			go s.deleteFile(file.Hash)
		}
	}

	// Save back the valid files (removing ones that don't exist on disk)
	if len(validFiles) != len(files) {
		return s.saveFiles(validFiles)
	}

	return nil
}

func (s *Server) loadExistingURLs() error {
	// URLs are always permanent, no cleanup needed
	return nil
}

func (s *Server) generateHash(filename string) string {
	data := fmt.Sprintf("%s-%d", filename, time.Now().UnixNano())
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:8]
}

func (s *Server) generateShortCode() string {
	data := fmt.Sprintf("%d", time.Now().UnixNano())
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:6]
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

func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"maxFileSizeBytes": maxFileSize,
	})
}

func (s *Server) listFilesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	files, err := s.loadFiles()
	if err != nil {
		log.Printf("Error loading files: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to load files",
		})
		return
	}

	// Calculate remaining TTL for each file
	type FileInfo struct {
		Hash         string    `json:"hash"`
		Filename     string    `json:"filename"`
		UploadTime   time.Time `json:"upload_time"`
		Permanent    bool      `json:"permanent"`
		TTLSeconds   int64     `json:"ttl_seconds,omitempty"`
		RemainingTTL int64     `json:"remaining_ttl,omitempty"`
		FilePath     string    `json:"file_path"`
	}

	var fileInfos []FileInfo
	now := time.Now()

	for _, file := range files {
		info := FileInfo{
			Hash:       file.Hash,
			Filename:   file.Filename,
			UploadTime: file.UploadTime,
			Permanent:  file.Permanent,
			TTLSeconds: file.TTLSeconds,
			FilePath:   file.FilePath,
		}

		if !file.Permanent {
			expiresAt := file.UploadTime.Add(time.Duration(file.TTLSeconds) * time.Second)
			remaining := expiresAt.Sub(now).Seconds()
			if remaining > 0 {
				info.RemainingTTL = int64(remaining)
			} else {
				info.RemainingTTL = 0
			}
		}

		fileInfos = append(fileInfos, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"files":   fileInfos,
	})
}

func (s *Server) listURLsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	urls, err := s.loadURLs()
	if err != nil {
		log.Printf("Error loading URLs: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to load URLs",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"urls":    urls,
	})
}

func (s *Server) deleteFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract hash from URL path (e.g., "/api/files/abc123" -> hash="abc123")
	hash := strings.TrimPrefix(r.URL.Path, "/api/files/")
	if hash == "" || hash == r.URL.Path {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Hash is required",
		})
		return
	}

	// Find and delete the file
	files, err := s.loadFiles()
	if err != nil {
		log.Printf("Error loading files: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to load files",
		})
		return
	}

	var found bool
	var filePath, filename string
	var updatedFiles []FileRecord

	for _, file := range files {
		if file.Hash == hash {
			found = true
			filePath = file.FilePath
			filename = file.Filename
		} else {
			updatedFiles = append(updatedFiles, file)
		}
	}

	if !found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "File not found",
		})
		return
	}

	// Delete file from disk
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		log.Printf("Error deleting file %s: %v", filePath, err)
	}

	// Save updated file list
	if err := s.saveFiles(updatedFiles); err != nil {
		log.Printf("Error saving files: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to delete file",
		})
		return
	}

	log.Printf("Admin deleted file: %s (%s)", filename, hash)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "File deleted successfully",
	})
}

func (s *Server) deleteURLHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract short code from URL path (e.g., "/api/urls/abc123" -> code="abc123")
	shortCode := strings.TrimPrefix(r.URL.Path, "/api/urls/")
	if shortCode == "" || shortCode == r.URL.Path {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Short code is required",
		})
		return
	}

	// Find and delete the URL
	urls, err := s.loadURLs()
	if err != nil {
		log.Printf("Error loading URLs: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to load URLs",
		})
		return
	}

	var found bool
	var originalURL string
	var updatedURLs []URLRecord

	for _, url := range urls {
		if url.ShortCode == shortCode {
			found = true
			originalURL = url.OriginalURL
		} else {
			updatedURLs = append(updatedURLs, url)
		}
	}

	if !found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "URL not found",
		})
		return
	}

	// Save updated URL list
	if err := s.saveURLs(updatedURLs); err != nil {
		log.Printf("Error saving URLs: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to delete URL",
		})
		return
	}

	log.Printf("Admin deleted URL: %s (%s)", originalURL, shortCode)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "URL deleted successfully",
	})
}

func (s *Server) shortenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		URL        string `json:"url"`
		CustomCode string `json:"custom_code,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid request",
		})
		return
	}

	// Validate URL
	if req.URL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "URL is required",
		})
		return
	}

	// Parse and validate URL format
	parsedURL, err := url.Parse(req.URL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid URL format",
		})
		return
	}

	// Ensure URL has a scheme
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "URL must use http or https scheme",
		})
		return
	}

	// Load existing URLs
	urls, err := s.loadURLs()
	if err != nil {
		log.Printf("Error loading URLs: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Internal server error",
		})
		return
	}

	var shortCode string
	if req.CustomCode != "" {
		// Validate custom code
		customCode := strings.TrimSpace(req.CustomCode)
		if len(customCode) < 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Custom code must be at least 3 characters long",
			})
			return
		}
		if len(customCode) > 20 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Custom code must be at most 20 characters long",
			})
			return
		}
		// Only allow alphanumeric characters, hyphens, and underscores
		for _, c := range customCode {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Custom code can only contain letters, numbers, hyphens, and underscores",
				})
				return
			}
		}

		// Check if custom code is already taken
		for _, existingURL := range urls {
			if existingURL.ShortCode == customCode {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Custom code is already taken",
				})
				return
			}
		}

		shortCode = customCode
	} else {
		// Generate short code
		shortCode = s.generateShortCode()

		// Check for collisions (very unlikely but handle it)
		for _, existingURL := range urls {
			if existingURL.ShortCode == shortCode {
				// Regenerate if collision
				shortCode = s.generateShortCode()
				break
			}
		}
	}

	createdTime := time.Now()

	// Create URL record (always permanent)
	urlRecord := URLRecord{
		ShortCode:   shortCode,
		OriginalURL: req.URL,
		CreatedTime: createdTime,
	}

	// Save URL
	urls = append(urls, urlRecord)
	if err := s.saveURLs(urls); err != nil {
		log.Printf("Error saving URL: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to save URL",
		})
		return
	}

	// Return success with short URL
	shortURL := fmt.Sprintf("%s://%s/s/%s", func() string {
		if r.TLS != nil {
			return "https"
		}
		return "http"
	}(), r.Host, shortCode)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"short_url":    shortURL,
		"short_code":   shortCode,
		"original_url": req.URL,
	})
}

func (s *Server) redirectHandler(w http.ResponseWriter, r *http.Request) {
	// Extract short code from path (e.g., "/s/abc123" -> "abc123")
	path := strings.TrimPrefix(r.URL.Path, "/s/")
	if path == "" || path == r.URL.Path {
		http.NotFound(w, r)
		return
	}

	// Remove any trailing slashes or query parameters
	shortCode := strings.Split(path, "/")[0]
	shortCode = strings.Split(shortCode, "?")[0]

	if shortCode == "" {
		http.NotFound(w, r)
		return
	}

	// Load URLs
	urls, err := s.loadURLs()
	if err != nil {
		log.Printf("Error loading URLs: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Find matching URL
	var originalURL string
	var found bool
	for _, urlRecord := range urls {
		if urlRecord.ShortCode == shortCode {
			originalURL = urlRecord.OriginalURL
			found = true
			break
		}
	}

	if !found {
		http.NotFound(w, r)
		return
	}

	// Redirect to original URL
	http.Redirect(w, r, originalURL, http.StatusFound)
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	// Handle URL shortener redirects
	if strings.HasPrefix(r.URL.Path, "/s/") {
		s.redirectHandler(w, r)
		return
	}

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

	// Allow larger request bodies
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize*10)

	// Set max memory for parsing form
	r.ParseMultipartForm(maxFileSize * 10)

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
	}

	type UploadError struct {
		Filename string `json:"filename"`
		Error    string `json:"error"`
	}

	var uploadedFiles []UploadedFile
	var uploadErrors []UploadError

	// Parse optional TTL and permanence from form
	permanent := false
	if pv := r.FormValue("permanent"); pv != "" {
		// Accept "true"/"1"/"on"
		switch strings.ToLower(pv) {
		case "true", "1", "on", "yes":
			permanent = true
		}
	}
	var perFileTTL time.Duration
	if !permanent {
		if ttlStr := r.FormValue("ttl_seconds"); ttlStr != "" {
			if secs, err := strconv.ParseInt(ttlStr, 10, 64); err == nil && secs > 0 {
				perFileTTL = time.Duration(secs) * time.Second
			}
		}
		// If not provided or invalid, fallback to server default
		if perFileTTL <= 0 {
			perFileTTL = s.ttl
		}
	}

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

		// Store file info in JSON
		fileRecord := FileRecord{
			Hash:       hash,
			Filename:   fileHeader.Filename,
			UploadTime: uploadTime,
			FilePath:   filePath,
			TTLSeconds: func() int64 {
				if permanent {
					return 0
				}
				return int64(perFileTTL.Seconds())
			}(),
			Permanent: permanent,
		}

		files, err := s.loadFiles()
		if err != nil {
			os.Remove(filePath)
			log.Printf("Error loading files: %v", err)
			continue
		}

		files = append(files, fileRecord)
		if err := s.saveFiles(files); err != nil {
			os.Remove(filePath)
			log.Printf("Error storing file info: %v", err)
			continue
		}

		uploadedFiles = append(uploadedFiles, UploadedFile{
			Hash:      hash,
			Filename:  fileHeader.Filename,
			Extension: ext,
		})

		if !permanent {
			go s.scheduleDelete(hash, perFileTTL)
		}
		log.Printf("Uploaded file. Written to:  %v", filePath)
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

	files, err := s.loadFiles()
	if err != nil {
		log.Printf("Error loading files: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var filename, filePath string
	var found bool
	for _, file := range files {
		if file.Hash == hash {
			filename = file.Filename
			filePath = file.FilePath
			found = true
			break
		}
	}

	if !found {
		http.NotFound(w, r)
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
		// File doesn't exist, remove from JSON
		s.removeFile(hash)
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

func (s *Server) removeFile(hash string) {
	files, err := s.loadFiles()
	if err != nil {
		log.Printf("Error loading files for removal: %v", err)
		return
	}

	var updatedFiles []FileRecord
	var filename string
	for _, file := range files {
		if file.Hash == hash {
			filename = file.Filename
			// Skip this file (remove it)
		} else {
			updatedFiles = append(updatedFiles, file)
		}
	}

	// If we didn't find the file, it's already removed
	if filename == "" {
		return
	}

	if err := s.saveFiles(updatedFiles); err != nil {
		log.Printf("Error saving files after removal: %v", err)
	}
}

func (s *Server) deleteFile(hash string) {
	files, err := s.loadFiles()
	if err != nil {
		log.Printf("Error loading files for deletion: %v", err)
		return
	}

	var filename, filePath string
	var found bool
	for _, file := range files {
		if file.Hash == hash {
			filename = file.Filename
			filePath = file.FilePath
			found = true
			break
		}
	}

	if !found {
		return // Already deleted
	}

	// Delete file from disk
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		log.Printf("Error deleting file %s: %v", filePath, err)
	}

	// Delete from JSON
	s.removeFile(hash)
	log.Printf("Deleted expired file: %s (%s)", filename, hash)
}

func (s *Server) scheduleDeleteURL(shortCode string, ttl time.Duration) {
	time.Sleep(ttl)
	s.deleteURL(shortCode)
}

func (s *Server) removeURL(shortCode string) {
	urls, err := s.loadURLs()
	if err != nil {
		log.Printf("Error loading URLs for removal: %v", err)
		return
	}

	var updatedURLs []URLRecord
	for _, url := range urls {
		if url.ShortCode != shortCode {
			updatedURLs = append(updatedURLs, url)
		}
	}

	if err := s.saveURLs(updatedURLs); err != nil {
		log.Printf("Error saving URLs after removal: %v", err)
	}
}

func (s *Server) deleteURL(shortCode string) {
	urls, err := s.loadURLs()
	if err != nil {
		log.Printf("Error loading URLs for deletion: %v", err)
		return
	}

	var originalURL string
	var found bool
	for _, url := range urls {
		if url.ShortCode == shortCode {
			originalURL = url.OriginalURL
			found = true
			break
		}
	}

	if !found {
		return // Already deleted
	}

	// Delete from JSON
	s.removeURL(shortCode)
	log.Printf("Deleted expired URL: %s (%s)", originalURL, shortCode)
}

func (s *Server) cleanupRoutine() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		files, err := s.loadFiles()
		if err != nil {
			log.Printf("Error loading files for cleanup: %v", err)
			continue
		}

		for _, file := range files {
			if file.Permanent {
				continue
			}
			var expiresAt time.Time
			if file.TTLSeconds > 0 {
				expiresAt = file.UploadTime.Add(time.Duration(file.TTLSeconds) * time.Second)
			} else {
				expiresAt = file.UploadTime.Add(s.ttl)
			}
			if !expiresAt.IsZero() && expiresAt.Before(now) {
				// File has expired, delete it
				// Delete file from disk
				if err := os.Remove(file.FilePath); err != nil && !os.IsNotExist(err) {
					log.Printf("Error deleting expired file %s: %v", file.FilePath, err)
				}

				// Delete from JSON
				s.removeFile(file.Hash)
				log.Printf("Cleaned up expired file: %s (%s)", file.Filename, file.Hash)
			}
		}

		// URLs are permanent, no cleanup needed
	}
}

func main() {
	// Load credentials from config file
	configPath := ".env"

	username, password, port, fileSizeMB, err := loadCredentials(configPath)
	if err != nil {
		log.Fatal("Failed to load credentials:", err)
	}

	// Set global variables from config
	maxFileSize = fileSizeMB << 20

	log.Printf("Loaded credentials: USERNAME=%s, PASSWORD=%s, PORT=%s", username, password, port)
	log.Printf("File size limit: %d MB", maxFileSize/(1<<20))

	uploadDir := "./uploads"
	if uploadEnv := os.Getenv("UPLOAD_DIR"); uploadEnv != "" {
		uploadDir = uploadEnv
	}
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("Failed to create upload directory:", err)
	}

	ttl := 3 * time.Hour

	server, err := NewServer(uploadDir, ttl, username, password)
	if err != nil {
		log.Fatal("Failed to create server:", err)
	}

	go server.cleanupRoutine()

	http.HandleFunc("/", server.indexHandler)
	http.HandleFunc("/upload", server.requireAuth(server.uploadHandler))
	http.HandleFunc("/shorten", server.requireAuth(server.shortenHandler))
	http.HandleFunc("/login", server.loginHandler)
	http.HandleFunc("/logout", server.logoutHandler)
	http.HandleFunc("/check-auth", server.checkAuthHandler)
	http.HandleFunc("/config", server.configHandler)
	http.HandleFunc("/api/files", server.requireAuth(server.listFilesHandler))
	http.HandleFunc("/api/urls", server.requireAuth(server.listURLsHandler))
	http.HandleFunc("/api/files/", server.requireAuth(server.deleteFileHandler))
	http.HandleFunc("/api/urls/", server.requireAuth(server.deleteURLHandler))

	log.Printf("Server starting on %s", port)
	log.Printf("Files will expire after %v", ttl)
	log.Fatal(http.ListenAndServe(port, nil))
}
