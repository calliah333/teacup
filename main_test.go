package main

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type testFile struct {
	field, name, content string
}

func testConfig() Config {
	cfg := defaultConfig()
	cfg.Username = "admin"
	cfg.Password = "secret"
	return cfg
}

// newTestServer runs the server from a temporary directory holding a copy of index.html.
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	index, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), index, 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	uploadDir := filepath.Join(dir, "uploads")
	if err := os.Mkdir(uploadDir, 0755); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(uploadDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func multipartBody(t *testing.T, fields map[string]string, files ...testFile) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for _, f := range files {
		field := f.field
		if field == "" {
			field = "file"
		}
		w, err := mw.CreateFormFile(field, f.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f.content))
	}
	mw.Close()
	return &body, mw.FormDataContentType()
}

type uploadResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Files   []struct {
		Hash      string `json:"hash"`
		Extension string `json:"extension"`
	} `json:"files"`
	Errors []struct {
		Filename string `json:"filename"`
		Error    string `json:"error"`
	} `json:"errors"`
}

func upload(t *testing.T, s *Server, fields map[string]string, files ...testFile) (int, uploadResponse) {
	t.Helper()
	body, contentType := multipartBody(t, fields, files...)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding upload response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func TestDownloadHeaders(t *testing.T) {
	s := newTestServer(t, testConfig())
	tests := []struct {
		name, content, disposition string
	}{
		{"x.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, "attachment"},
		{"x.html", `<script>alert(1)</script>`, "attachment"},
		{`pic "1".png`, "\x89PNG\r\n\x1a\n", "inline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := upload(t, s, nil, testFile{name: tc.name, content: tc.content})
			if code != http.StatusOK || len(resp.Files) != 1 {
				t.Fatalf("upload: status %d, %+v", code, resp)
			}
			f := resp.Files[0]
			rec := httptest.NewRecorder()
			s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+f.Hash+f.Extension, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("download status %d", rec.Code)
			}
			h := rec.Header()
			disposition, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
			if err != nil || disposition != tc.disposition || params["filename"] != tc.name {
				t.Errorf("Content-Disposition %q: got %q %v (err %v)", h.Get("Content-Disposition"), disposition, params, err)
			}
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := h.Get("Content-Security-Policy"); !strings.HasPrefix(got, "sandbox;") {
				t.Errorf("Content-Security-Policy = %q", got)
			}
		})
	}
}

func TestBasicAuth(t *testing.T) {
	s := newTestServer(t, testConfig())
	try := func(user, pass string) int {
		body, contentType := multipartBody(t, nil, testFile{name: "a.txt", content: "a"})
		req := httptest.NewRequest(http.MethodPost, "/upload", body)
		req.Header.Set("Content-Type", contentType)
		req.SetBasicAuth(user, pass)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := try("admin", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong password: status %d", code)
	}
	if code := try("wrong", "secret"); code != http.StatusUnauthorized {
		t.Errorf("wrong username: status %d", code)
	}
	if code := try("admin", "secret"); code != http.StatusOK {
		t.Errorf("correct credentials: status %d", code)
	}

	// Temporary curl credentials work until they expire.
	req := httptest.NewRequest(http.MethodPost, "/api/curl-credentials", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var temp struct{ Username, Password string }
	json.Unmarshal(rec.Body.Bytes(), &temp)
	if code := try(temp.Username, temp.Password); code != http.StatusOK {
		t.Errorf("temporary credentials: status %d", code)
	}
	if code := try(temp.Username, temp.Password+"x"); code != http.StatusUnauthorized {
		t.Errorf("wrong temporary password: status %d", code)
	}
	s.sessionsMu.Lock()
	s.tempAuth[temp.Username] = tempCredential{password: temp.Password, expires: time.Now().Add(-time.Second)}
	s.sessionsMu.Unlock()
	if code := try(temp.Username, temp.Password); code != http.StatusUnauthorized {
		t.Errorf("expired temporary credentials: status %d", code)
	}
}

func TestLogin(t *testing.T) {
	s := newTestServer(t, testConfig())
	login := func(user, pass string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body)))
		return rec
	}
	if rec := login("admin", "nope"); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Errorf("wrong password: status %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	if rec := login("admin", "secret"); rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Errorf("correct login: status %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
}

func TestRandomIDs(t *testing.T) {
	tests := []struct {
		bytes int
		re    *regexp.Regexp
	}{
		{fileIDBytes, regexp.MustCompile(`^[a-z2-7]{13}$`)},
		{shortCodeBytes, regexp.MustCompile(`^[a-z2-7]{13}$`)},
	}
	for _, tc := range tests {
		seen := make(map[string]bool)
		for range 10000 {
			id := randomID(tc.bytes)
			if !tc.re.MatchString(id) {
				t.Fatalf("id %q does not match %s", id, tc.re)
			}
			if seen[id] {
				t.Fatalf("duplicate id %q", id)
			}
			seen[id] = true
		}
	}
}

func getCapabilities(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "max-age=60" {
		t.Fatalf("status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var caps map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	return caps
}

func TestCapabilities(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		caps := getCapabilities(t, newTestServer(t, testConfig()))
		want := map[string]any{
			"apiVersion":         1.0,
			"maxFileSizeBytes":   104857600.0,
			"maxFilesPerRequest": 20.0,
			"defaultTtlSeconds":  10800.0,
			"maxTtlSeconds":      nil,
			"permanentAllowed":   true,
			"uploadFields":       []any{"file", "files"},
		}
		gotJSON, _ := json.Marshal(caps)
		wantJSON, _ := json.Marshal(want)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("got %s, want %s", gotJSON, wantJSON)
		}
	})

	t.Run("from .env", func(t *testing.T) {
		env := filepath.Join(t.TempDir(), ".env")
		os.WriteFile(env, []byte("USERNAME=a\nPASSWORD=b\nMAX_FILE_SIZE_MB=5\nDEFAULT_TTL_HOURS=1.5\nMAX_TTL_HOURS=24\nALLOW_PERMANENT=false\nMAX_FILES_PER_REQUEST=3\n"), 0644)
		cfg, err := loadConfig(env)
		if err != nil {
			t.Fatal(err)
		}
		caps := getCapabilities(t, newTestServer(t, cfg))
		if caps["maxFileSizeBytes"] != float64(5<<20) || caps["maxFilesPerRequest"] != 3.0 ||
			caps["defaultTtlSeconds"] != 5400.0 || caps["maxTtlSeconds"] != 86400.0 || caps["permanentAllowed"] != false {
			t.Errorf("unexpected capabilities %v", caps)
		}
	})

	t.Run("invalid .env", func(t *testing.T) {
		for _, extra := range []string{"MAX_FILES_PER_REQUEST=0", "DEFAULT_TTL_HOURS=abc", "ALLOW_PERMANENT=maybe", "DEFAULT_TTL_HOURS=5\nMAX_TTL_HOURS=2"} {
			env := filepath.Join(t.TempDir(), ".env")
			os.WriteFile(env, []byte("USERNAME=a\nPASSWORD=b\n"+extra+"\n"), 0644)
			if _, err := loadConfig(env); err == nil {
				t.Errorf("%q: expected error", extra)
			}
		}
	})
}

func TestUploadLimits(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFilesPerRequest = 2
	cfg.MaxTTL = time.Hour
	cfg.AllowPermanent = false
	cfg.MaxFileSize = 1 << 10
	s := newTestServer(t, cfg)

	f := func(name string) testFile { return testFile{name: name, content: "x"} }

	// "file" and "files" fields count together.
	code, resp := upload(t, s, nil, f("a.txt"), testFile{field: "files", name: "b.txt", content: "x"}, f("c.txt"))
	if code != http.StatusBadRequest || resp.Success {
		t.Errorf("too many files: status %d, %+v", code, resp)
	}

	code, resp = upload(t, s, map[string]string{"permanent": "true"}, f("a.txt"))
	if code != http.StatusBadRequest || resp.Success {
		t.Errorf("permanent not allowed: status %d, %+v", code, resp)
	}

	code, resp = upload(t, s, nil, testFile{name: "big.txt", content: strings.Repeat("x", 2<<10)})
	if code != http.StatusBadRequest || resp.Success || len(resp.Errors) != 1 {
		t.Errorf("only oversized file: status %d, %+v", code, resp)
	}

	if files, _ := s.loadFiles(); len(files) != 0 {
		t.Fatalf("rejected uploads stored %d records", len(files))
	}

	code, resp = upload(t, s, map[string]string{"ttl_seconds": "7200"}, f("a.txt"))
	if code != http.StatusOK || !resp.Success {
		t.Fatalf("ttl upload: status %d, %+v", code, resp)
	}
	files, _ := s.loadFiles()
	if len(files) != 1 || files[0].TTLSeconds != 3600 {
		t.Errorf("ttl not capped: %+v", files)
	}
}

func TestUploadRecordSaveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	s := newTestServer(t, testConfig())
	if err := os.Chmod(s.filesJSON, 0444); err != nil {
		t.Fatal(err)
	}

	code, resp := upload(t, s, nil, testFile{name: "a.txt", content: "a"}, testFile{name: "b.txt", content: "b"})
	if code != http.StatusInternalServerError || resp.Success {
		t.Errorf("status %d, %+v", code, resp)
	}
	if len(resp.Errors) != 2 || resp.Errors[0].Filename != "a.txt" || resp.Errors[0].Error != "Failed to store file" {
		t.Errorf("errors = %+v", resp.Errors)
	}
	entries, _ := os.ReadDir(s.uploadDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("orphaned upload %s left on disk", e.Name())
		}
	}
}

func TestUploadBodyTooLarge(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFileSize = 1 << 10
	cfg.MaxFilesPerRequest = 1
	s := newTestServer(t, cfg)

	code, resp := upload(t, s, nil, testFile{name: "big.bin", content: strings.Repeat("x", 2<<20)})
	if code != http.StatusRequestEntityTooLarge || resp.Success {
		t.Errorf("status %d, %+v", code, resp)
	}
}

func TestChibisafeUpload(t *testing.T) {
	cfg := testConfig()
	cfg.APIKey = "key"
	cfg.MaxFileSize = 1 << 10
	s := newTestServer(t, cfg)

	type chibiResponse struct {
		StatusCode      int
		Name, UUID, URL string
		Error, Message  string
	}
	post := func(apiKey string, headers map[string]string, files ...testFile) (int, chibiResponse) {
		t.Helper()
		body, contentType := multipartBody(t, nil, files...)
		req := httptest.NewRequest(http.MethodPost, "/api/upload", body)
		req.Header.Set("Content-Type", contentType)
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		var resp chibiResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	shot := testFile{field: "file[]", name: "shot.png", content: "png"}

	code, resp := post("key", nil, shot)
	if code != http.StatusOK || resp.Name != resp.UUID+".png" || resp.URL != "http://example.com/"+resp.Name {
		t.Fatalf("upload: status %d, %+v", code, resp)
	}
	dl := httptest.NewRecorder()
	s.routes().ServeHTTP(dl, httptest.NewRequest(http.MethodGet, "/"+resp.Name, nil))
	if dl.Code != http.StatusOK || dl.Body.String() != "png" {
		t.Errorf("download: status %d, body %q", dl.Code, dl.Body.String())
	}

	failures := []struct {
		name    string
		apiKey  string
		headers map[string]string
		files   []testFile
		status  int
	}{
		{"wrong key", "nope", nil, []testFile{shot}, http.StatusUnauthorized},
		{"no key", "", nil, []testFile{shot}, http.StatusUnauthorized},
		{"chunked", "key", map[string]string{"chibi-uuid": "u", "chibi-chunk-number": "0", "chibi-chunks-total": "2"}, []testFile{shot}, http.StatusBadRequest},
		{"two files", "key", nil, []testFile{shot, shot}, http.StatusBadRequest},
		{"oversized", "key", nil, []testFile{{field: "file[]", name: "big.bin", content: strings.Repeat("x", 2<<10)}}, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range failures {
		code, resp := post(tc.apiKey, tc.headers, tc.files...)
		if code != tc.status || resp.StatusCode != tc.status || resp.Error != http.StatusText(tc.status) || resp.Message == "" {
			t.Errorf("%s: status %d, %+v", tc.name, code, resp)
		}
	}
	if files, _ := s.loadFiles(); len(files) != 1 {
		t.Errorf("rejected uploads stored records: %+v", files)
	}

	// Without API_KEY configured, x-api-key never authenticates.
	s.cfg.APIKey = ""
	if code, _ := post("", map[string]string{"x-api-key": ""}, shot); code != http.StatusUnauthorized {
		t.Errorf("empty configured key: status %d", code)
	}
}
