package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiVersion = 1

	defaultHashLength = 13 // base32 characters (~65 bits); override with HASH_LENGTH
	shortCodeLength   = 13 // base32 characters

	// Bounds for HASH_LENGTH. Below the minimum the ID space is small enough
	// that collision retries could spin; 64 characters is 320 bits.
	minHashLength = 4
	maxHashLength = 64

	// Multipart parts above this size are spooled to temporary files.
	multipartMemory = 32 << 20
	// Allowance for multipart boundaries, part headers and form fields.
	multipartOverhead = 1 << 20

	downloadCSP = "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'"
)

// Raster image types that are safe to render inline. Everything else,
// including SVG and HTML, is served as an attachment.
var inlineContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/avif": true,
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

type Config struct {
	Username           string
	Password           string
	APIKey             string // accepted in the x-api-key header; empty disables it
	Port               string
	MaxFileSize        int64 // bytes per file
	MaxFilesPerRequest int
	DefaultTTL         time.Duration
	MaxTTL             time.Duration // 0 means no maximum
	AllowPermanent     bool
	HashLength         int // characters in generated file IDs
}

func defaultConfig() Config {
	return Config{
		Port:               ":8080",
		MaxFileSize:        100 << 20,
		MaxFilesPerRequest: 20,
		DefaultTTL:         3 * time.Hour,
		AllowPermanent:     true,
		HashLength:         defaultHashLength,
	}
}

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
	cfg        Config
	indexTmpl  *template.Template
	sessions   map[string]time.Time
	sessionsMu sync.RWMutex
	tempAuth   map[string]tempCredential
}

type tempCredential struct {
	password string
	expires  time.Time
}

func loadConfig(configPath string) (Config, error) {
	cfg := defaultConfig()

	file, err := os.Open(configPath)
	if os.IsNotExist(err) {
		return Config{}, fmt.Errorf("config file not found at %s", configPath)
	}
	if err != nil {
		return Config{}, fmt.Errorf("failed to open config file: %w", err)
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
		if value == "" {
			continue
		}

		var parseErr error
		switch strings.ToLower(key) {
		case "username":
			cfg.Username = value
		case "password":
			cfg.Password = value
		case "api_key":
			cfg.APIKey = value
		case "port":
			cfg.Port = value
		case "max_file_size_mb":
			var mb int
			mb, parseErr = parsePositiveInt(value, 1<<20) // up to 1 TB
			cfg.MaxFileSize = int64(mb) << 20
		case "max_files_per_request":
			cfg.MaxFilesPerRequest, parseErr = parsePositiveInt(value, 10000)
		case "default_ttl_hours":
			cfg.DefaultTTL, parseErr = parseHours(value)
		case "max_ttl_hours":
			cfg.MaxTTL, parseErr = parseHours(value)
		case "allow_permanent":
			cfg.AllowPermanent, parseErr = strconv.ParseBool(value)
		case "hash_length":
			cfg.HashLength, parseErr = parsePositiveInt(value, maxHashLength)
			if parseErr == nil && cfg.HashLength < minHashLength {
				parseErr = fmt.Errorf("out of range")
			}
		}
		if parseErr != nil {
			return Config{}, fmt.Errorf("invalid value %q for %s", value, key)
		}
	}

	if err := scanner.Err(); err != nil {
		return Config{}, fmt.Errorf("error reading config file: %w", err)
	}

	if cfg.Username == "" {
		return Config{}, fmt.Errorf("username not found in config file")
	}

	if cfg.Password == "" {
		return Config{}, fmt.Errorf("password not found in config file")
	}

	if cfg.MaxTTL > 0 && cfg.DefaultTTL > cfg.MaxTTL {
		return Config{}, fmt.Errorf("DEFAULT_TTL_HOURS exceeds MAX_TTL_HOURS")
	}

	// Normalize port format (ensure it starts with ":")
	if !strings.HasPrefix(cfg.Port, ":") {
		cfg.Port = ":" + cfg.Port
	}

	return cfg, nil
}

func parsePositiveInt(value string, max int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 || n > max {
		return 0, fmt.Errorf("out of range")
	}
	return n, nil
}

func parseHours(value string) (time.Duration, error) {
	hours, err := strconv.ParseFloat(value, 64)
	// The upper bound (about 114 years) keeps the duration from overflowing.
	if err != nil || !(hours > 0 && hours <= 1e6) {
		return 0, fmt.Errorf("out of range")
	}
	return time.Duration(hours * float64(time.Hour)), nil
}

func NewServer(uploadDir string, cfg Config) (*Server, error) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse index.html: %w", err)
	}

	filesJSON := filepath.Join(uploadDir, "files.json")
	urlsJSON := filepath.Join(uploadDir, "urls.json")

	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.Password = strings.TrimSpace(cfg.Password)

	server := &Server{
		filesJSON: filesJSON,
		urlsJSON:  urlsJSON,
		uploadDir: uploadDir,
		cfg:       cfg,
		indexTmpl: tmpl,
		sessions:  make(map[string]time.Time),
		tempAuth:  make(map[string]tempCredential),
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

// addFileRecord appends a record under a single lock so concurrent uploads
// can't overwrite each other's records.
func (s *Server) addFileRecord(record FileRecord) error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()

	files, err := s.loadFilesLocked()
	if err != nil {
		return err
	}
	return s.saveFilesLocked(append(files, record))
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
			expiresAt = file.UploadTime.Add(s.cfg.DefaultTTL)
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

// randomID returns length characters of lowercase unpadded base32 drawn from crypto/rand.
func randomID(length int) string {
	b := make([]byte, (length*5+7)/8)
	rand.Read(b) // Never fails since Go 1.24; it crashes the program instead.
	return idEncoding.EncodeToString(b)[:length]
}

func (s *Server) newFileID() (string, error) {
	files, err := s.loadFiles()
	if err != nil {
		return "", err
	}
	for {
		id := randomID(s.cfg.HashLength)
		if !slices.ContainsFunc(files, func(f FileRecord) bool { return f.Hash == id }) {
			return id, nil
		}
	}
}

func shortCodeTaken(urls []URLRecord, code string) bool {
	return slices.ContainsFunc(urls, func(u URLRecord) bool { return u.ShortCode == code })
}

// equal compares secrets in constant time.
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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

// authorized reports whether r carries a session cookie, HTTP Basic Auth
// credentials or the configured API key.
func (s *Server) authorized(r *http.Request) bool {
	return s.isValidSession(s.getSessionToken(r)) || s.validBasicAuth(r) || s.validAPIKey(r)
}

func (s *Server) requireAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false,
				"error":   "Authentication required",
			})
			return
		}
		fn(w, r)
	}
}

// validAPIKey checks the chibisafe-style x-api-key header. API keys are
// disabled when API_KEY is unset.
func (s *Server) validAPIKey(r *http.Request) bool {
	return s.cfg.APIKey != "" && equal(r.Header.Get("X-Api-Key"), s.cfg.APIKey)
}

func (s *Server) validBasicAuth(r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	// Evaluate both comparisons so timing doesn't reveal a correct username.
	userOK := equal(username, s.cfg.Username)
	passOK := equal(password, s.cfg.Password)
	if userOK && passOK {
		return true
	}

	s.sessionsMu.RLock()
	credential, exists := s.tempAuth[username]
	s.sessionsMu.RUnlock()
	return exists && equal(password, credential.password) && credential.expires.After(time.Now())
}

func (s *Server) curlCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	username, err := s.generateSessionToken()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	password, err := s.generateSessionToken()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	username = "temp-" + username[:12]
	password = password[:32]
	expires := time.Now().Add(5 * time.Minute)
	s.sessionsMu.Lock()
	s.tempAuth[username] = tempCredential{password: password, expires: expires}
	s.sessionsMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"username": username, "password": password, "expires": expires})
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

	// Evaluate both comparisons so timing doesn't reveal a correct username.
	userOK := equal(creds.Username, s.cfg.Username)
	passOK := equal(creds.Password, s.cfg.Password)
	if !userOK || !passOK {
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

func (s *Server) capabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var maxTTLSeconds *int64 // null means no maximum
	if s.cfg.MaxTTL > 0 {
		secs := int64(s.cfg.MaxTTL / time.Second)
		maxTTLSeconds = &secs
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=60")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiVersion":         apiVersion,
		"maxFileSizeBytes":   s.cfg.MaxFileSize,
		"maxFilesPerRequest": s.cfg.MaxFilesPerRequest,
		"defaultTtlSeconds":  int64(s.cfg.DefaultTTL / time.Second),
		"maxTtlSeconds":      maxTTLSeconds,
		"permanentAllowed":   s.cfg.AllowPermanent,
		"uploadFields":       []string{"file", "files"},
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
		if shortCodeTaken(urls, customCode) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Custom code is already taken",
			})
			return
		}

		shortCode = customCode
	} else {
		shortCode = randomID(shortCodeLength)
		for shortCodeTaken(urls, shortCode) {
			shortCode = randomID(shortCodeLength)
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

func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// uploadFailure is an upload error and the HTTP status it maps to.
type uploadFailure struct {
	status  int
	message string
}

type uploadedFile struct {
	Hash      string `json:"hash"`
	Filename  string `json:"filename"`
	Extension string `json:"extension"`
	URL       string `json:"url"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// parseUploadForm caps the body at maxFiles full-size files and parses it.
// On success the caller must call r.MultipartForm.RemoveAll.
func (s *Server) parseUploadForm(w http.ResponseWriter, r *http.Request, maxFiles int) *uploadFailure {
	maxBody := s.cfg.MaxFileSize*int64(maxFiles) + multipartOverhead
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &uploadFailure{http.StatusRequestEntityTooLarge, fmt.Sprintf("Request exceeds maximum size of %d MB", maxBody>>20)}
		}
		return &uploadFailure{http.StatusBadRequest, "Invalid multipart form"}
	}
	return nil
}

// uploadOptions reads the optional permanent and ttl_seconds form fields.
// The TTL is zero for permanent uploads.
func (s *Server) uploadOptions(r *http.Request) (time.Duration, bool, *uploadFailure) {
	permanent := false
	switch strings.ToLower(r.FormValue("permanent")) {
	case "true", "1", "on", "yes":
		permanent = true
	}
	if permanent {
		if !s.cfg.AllowPermanent {
			return 0, false, &uploadFailure{http.StatusBadRequest, "Permanent files are not allowed"}
		}
		return 0, true, nil
	}
	// Fall back to the server default when missing or invalid
	ttl := s.cfg.DefaultTTL
	if ttlStr := r.FormValue("ttl_seconds"); ttlStr != "" {
		if secs, err := strconv.ParseInt(ttlStr, 10, 64); err == nil && secs > 0 && secs <= math.MaxInt64/int64(time.Second) {
			ttl = time.Duration(secs) * time.Second
		}
	}
	if s.cfg.MaxTTL > 0 && ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	return ttl, false, nil
}

// storeFile saves one uploaded part, records it and schedules its expiry.
// Failures are 413 when the file is too large and 500 when storage fails.
func (s *Server) storeFile(r *http.Request, fileHeader *multipart.FileHeader, ttl time.Duration, permanent bool) (uploadedFile, *uploadFailure) {
	maxFileSize := s.cfg.MaxFileSize
	tooLarge := &uploadFailure{http.StatusRequestEntityTooLarge, fmt.Sprintf("File size exceeds maximum allowed size of %d MB", maxFileSize>>20)}
	// Check file size before processing
	if fileHeader.Size > maxFileSize {
		tooLarge.message = fmt.Sprintf("File size (%d MB) exceeds maximum allowed size of %d MB", fileHeader.Size>>20, maxFileSize>>20)
		return uploadedFile{}, tooLarge
	}

	file, err := fileHeader.Open()
	if err != nil {
		log.Printf("Error opening file: %v", err)
		return uploadedFile{}, &uploadFailure{http.StatusInternalServerError, "Failed to open file"}
	}
	defer file.Close()

	hash, err := s.newFileID()
	if err != nil {
		log.Printf("Error loading files: %v", err)
		return uploadedFile{}, &uploadFailure{http.StatusInternalServerError, "Failed to store file"}
	}
	ext := filepath.Ext(fileHeader.Filename)
	filePath := filepath.Join(s.uploadDir, hash+ext)

	dst, err := os.Create(filePath)
	if err != nil {
		log.Printf("Error creating file: %v", err)
		return uploadedFile{}, &uploadFailure{http.StatusInternalServerError, "Failed to create file"}
	}

	// Use LimitedReader to enforce size limit during copy
	n, err := io.Copy(dst, io.LimitReader(file, maxFileSize+1))
	dst.Close()
	if err != nil {
		os.Remove(filePath)
		log.Printf("Error saving file: %v", err)
		return uploadedFile{}, &uploadFailure{http.StatusInternalServerError, "Failed to save file"}
	}
	// Check if file exceeded limit during copy
	if n > maxFileSize {
		os.Remove(filePath)
		return uploadedFile{}, tooLarge
	}

	fileRecord := FileRecord{
		Hash:       hash,
		Filename:   fileHeader.Filename,
		UploadTime: time.Now(),
		FilePath:   filePath,
		TTLSeconds: int64(ttl / time.Second),
		Permanent:  permanent,
	}
	if err := s.addFileRecord(fileRecord); err != nil {
		os.Remove(filePath)
		log.Printf("Error storing file info: %v", err)
		return uploadedFile{}, &uploadFailure{http.StatusInternalServerError, "Failed to store file"}
	}

	if !permanent {
		go s.scheduleDelete(hash, ttl)
	}
	log.Printf("Uploaded file. Written to:  %v", filePath)
	return uploadedFile{
		Hash:      hash,
		Filename:  fileHeader.Filename,
		Extension: ext,
		URL:       absoluteURL(r, "/"+hash+ext),
	}, nil
}

func (s *Server) uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fail := func(f *uploadFailure) {
		writeJSON(w, f.status, map[string]any{"success": false, "error": f.message})
	}

	if f := s.parseUploadForm(w, r, s.cfg.MaxFilesPerRequest); f != nil {
		fail(f)
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := slices.Concat(r.MultipartForm.File["files"], r.MultipartForm.File["file"])
	if len(files) == 0 {
		fail(&uploadFailure{http.StatusBadRequest, "No files uploaded"})
		return
	}
	if len(files) > s.cfg.MaxFilesPerRequest {
		fail(&uploadFailure{http.StatusBadRequest, fmt.Sprintf("Too many files: %d (maximum %d per request)", len(files), s.cfg.MaxFilesPerRequest)})
		return
	}
	ttl, permanent, f := s.uploadOptions(r)
	if f != nil {
		fail(f)
		return
	}

	type UploadError struct {
		Filename string `json:"filename"`
		Error    string `json:"error"`
	}
	uploadedFiles := []uploadedFile{}
	var uploadErrors []UploadError
	storageFailed := false
	for _, fileHeader := range files {
		uploaded, f := s.storeFile(r, fileHeader, ttl, permanent)
		if f != nil {
			storageFailed = storageFailed || f.status >= http.StatusInternalServerError
			uploadErrors = append(uploadErrors, UploadError{Filename: fileHeader.Filename, Error: f.message})
			continue
		}
		uploadedFiles = append(uploadedFiles, uploaded)
	}

	status := http.StatusOK
	response := map[string]any{
		"success": len(uploadedFiles) > 0,
		"files":   uploadedFiles,
	}
	if len(uploadedFiles) == 0 {
		status = http.StatusBadRequest
		if storageFailed {
			status = http.StatusInternalServerError
		}
		response["error"] = "No files were uploaded"
	}
	if len(uploadErrors) > 0 {
		response["errors"] = uploadErrors
	}
	writeJSON(w, status, response)
}

// chibisafeUploadHandler implements chibisafe's POST /api/upload so clients
// made for chibisafe (ShareX configs, browser extensions, scripts) work as-is:
// one file per request, x-api-key auth, and chibisafe's response and error
// shapes. Chunked uploads (chibi-* headers) are rejected rather than stored
// as partial files.
func (s *Server) chibisafeUploadHandler(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, message string) {
		writeJSON(w, status, map[string]any{
			"statusCode": status,
			"error":      http.StatusText(status),
			"message":    message,
		})
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if !s.authorized(r) {
		fail(http.StatusUnauthorized, "Invalid authorization")
		return
	}
	if r.Header.Get("chibi-uuid") != "" || r.Header.Get("chibi-chunk-number") != "" || r.Header.Get("chibi-chunks-total") != "" {
		fail(http.StatusBadRequest, "Chunked uploads are not supported")
		return
	}

	if f := s.parseUploadForm(w, r, 1); f != nil {
		fail(f.status, f.message)
		return
	}
	defer r.MultipartForm.RemoveAll()

	// chibisafe clients send "file[]", but chibisafe accepts any field name.
	var files []*multipart.FileHeader
	for _, headers := range r.MultipartForm.File {
		files = append(files, headers...)
	}
	if len(files) != 1 {
		fail(http.StatusBadRequest, fmt.Sprintf("Expected exactly one file, got %d", len(files)))
		return
	}
	ttl, permanent, f := s.uploadOptions(r)
	if f != nil {
		fail(f.status, f.message)
		return
	}
	uploaded, f := s.storeFile(r, files[0], ttl, permanent)
	if f != nil {
		fail(f.status, f.message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":  uploaded.Hash + uploaded.Extension,
		"uuid":  uploaded.Hash,
		"url":   uploaded.URL,
		"thumb": "",
	})
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
	mediaType, _, _ := mime.ParseMediaType(contentType)

	// Only raster images render inline; anything that could run script
	// (SVG, HTML, ...) is downloaded, and the CSP sandbox disables script
	// even if a browser renders it anyway.
	disposition := "attachment"
	if inlineContentTypes[mediaType] {
		disposition = "inline"
	}
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": filename}); v != "" {
		disposition = v
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", disposition)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", downloadCSP)

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
				expiresAt = file.UploadTime.Add(s.cfg.DefaultTTL)
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

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.indexHandler)
	mux.HandleFunc("/upload", s.requireAuth(s.uploadHandler))
	mux.HandleFunc("/api/upload", s.chibisafeUploadHandler)
	mux.HandleFunc("/api/curl-credentials", s.requireAuth(s.curlCredentialsHandler))
	mux.HandleFunc("/shorten", s.requireAuth(s.shortenHandler))
	mux.HandleFunc("/login", s.loginHandler)
	mux.HandleFunc("/logout", s.logoutHandler)
	mux.HandleFunc("/check-auth", s.checkAuthHandler)
	mux.HandleFunc("/api/capabilities", s.capabilitiesHandler)
	mux.HandleFunc("/api/files", s.requireAuth(s.listFilesHandler))
	mux.HandleFunc("/api/urls", s.requireAuth(s.listURLsHandler))
	mux.HandleFunc("/api/files/", s.requireAuth(s.deleteFileHandler))
	mux.HandleFunc("/api/urls/", s.requireAuth(s.deleteURLHandler))
	return mux
}

func main() {
	cfg, err := loadConfig(".env")
	if err != nil {
		log.Fatal("Failed to load config: ", err)
	}

	log.Printf("Loaded config: USERNAME=%s, PORT=%s", cfg.Username, cfg.Port)
	log.Printf("File size limit: %d MB, max %d files per request", cfg.MaxFileSize>>20, cfg.MaxFilesPerRequest)

	uploadDir := "./uploads"
	if uploadEnv := os.Getenv("UPLOAD_DIR"); uploadEnv != "" {
		uploadDir = uploadEnv
	}
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("Failed to create upload directory:", err)
	}

	server, err := NewServer(uploadDir, cfg)
	if err != nil {
		log.Fatal("Failed to create server:", err)
	}

	go server.cleanupRoutine()

	log.Printf("Server starting on %s", cfg.Port)
	log.Printf("Files expire after %v by default", cfg.DefaultTTL)
	log.Fatal(http.ListenAndServe(cfg.Port, server.routes()))
}
