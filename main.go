package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type FileInfo struct {
	Hash       string
	FileName   string
	UploadTime time.Time
	FilePath   string
}

type Server struct {
	files     map[string]*FileInfo
	mu        sync.RWMutex
	uploadDir string
	baseURL   string
	ttl       time.Duration
	indexTmpl *template.Template
}

func NewServer(uploadDir, baseURL string, ttl time.Duration) *Server {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		log.Fatal("Failed to parse index.html:", err)
	}

	return &Server{
		files:     make(map[string]*FileInfo),
		uploadDir: uploadDir,
		baseURL:   baseURL,
		ttl:       ttl,
		indexTmpl: tmpl,
	}
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
		Hash     string `json:"hash"`
		Filename string `json:"filename"`
	}

	var uploadedFiles []UploadedFile

	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("Error opening file: %v", err)
			continue
		}

		hash := s.generateHash(fileHeader.Filename)
		filePath := filepath.Join(s.uploadDir, hash)

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

		fileInfo := &FileInfo{
			Hash:       hash,
			FileName:   fileHeader.Filename,
			UploadTime: time.Now(),
			FilePath:   filePath,
		}

		s.mu.Lock()
		s.files[hash] = fileInfo
		s.mu.Unlock()

		uploadedFiles = append(uploadedFiles, UploadedFile{
			Hash:     hash,
			Filename: fileHeader.Filename,
		})

		go s.scheduleDelete(hash)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success": true, "files": [`)
	for i, f := range uploadedFiles {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"hash": "%s", "filename": "%s"}`, f.Hash, f.Filename)
	}
	fmt.Fprint(w, `]}`)
}

func (s *Server) downloadHandler(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Path[1:]

	s.mu.RLock()
	fileInfo, exists := s.files[hash]
	s.mu.RUnlock()

	if !exists {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileInfo.FileName))
	http.ServeFile(w, r, fileInfo.FilePath)
}

func (s *Server) scheduleDelete(hash string) {
	time.Sleep(s.ttl)

	s.mu.Lock()
	fileInfo, exists := s.files[hash]
	if exists {
		os.Remove(fileInfo.FilePath)
		delete(s.files, hash)
		log.Printf("Deleted expired file: %s (%s)", fileInfo.FileName, hash)
	}
	s.mu.Unlock()
}

func (s *Server) cleanupRoutine() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for hash, fileInfo := range s.files {
			if now.Sub(fileInfo.UploadTime) > s.ttl {
				os.Remove(fileInfo.FilePath)
				delete(s.files, hash)
				log.Printf("Cleaned up expired file: %s (%s)", fileInfo.FileName, hash)
			}
		}
		s.mu.Unlock()
	}
}

func main() {
	uploadDir := "./uploads"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("Failed to create upload directory:", err)
	}

	baseURL := "http://localhost:8080"
	ttl := 3 * time.Hour

	server := NewServer(uploadDir, baseURL, ttl)

	go server.cleanupRoutine()

	http.HandleFunc("/", server.indexHandler)
	http.HandleFunc("/upload", server.uploadHandler)

	port := ":8080"
	log.Printf("Server starting on %s", port)
	log.Printf("Files will expire after %v", ttl)
	log.Fatal(http.ListenAndServe(port, nil))
}
