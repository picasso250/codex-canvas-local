package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

const (
	defaultAddr   = "127.0.0.1:8765"
	notifyURL     = "http://127.0.0.1:8787/notify"
	runTimeout    = 20 * time.Minute
	maxPrompt     = 12000
	maxUploadBody = 128 << 20
	notifyTimeout = 1500 * time.Millisecond
)

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".jfif": true, ".webp": true, ".gif": true,
}

type server struct {
	root      string
	auditPath string
	jobs      map[string]*job
	mu        sync.RWMutex
	auditMu   sync.Mutex
	usageMu   sync.Mutex
	usage     usageLimitsResponse
	usageRun  bool
	sem       chan struct{}
}

type job struct {
	ID         string      `json:"id"`
	Mode       string      `json:"mode"`
	UserKey    string      `json:"-"`
	Email      string      `json:"-"`
	Prompt     string      `json:"prompt"`
	WorkDir    string      `json:"workDir"`
	Status     string      `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	Log        string      `json:"log"`
	Error      string      `json:"error,omitempty"`
	Images     []imageInfo `json:"images"`

	mu sync.Mutex
}

type jobView struct {
	ID         string      `json:"id"`
	Mode       string      `json:"mode"`
	Prompt     string      `json:"prompt"`
	WorkDir    string      `json:"workDir"`
	Status     string      `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	Log        string      `json:"log"`
	Error      string      `json:"error,omitempty"`
	Images     []imageInfo `json:"images"`
}

type imageInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type createJobResponse struct {
	ID string `json:"id"`
}

type fileEntry struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	IsDir       bool      `json:"isDir"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modTime"`
	IsImage     bool      `json:"isImage"`
	PreviewURL  string    `json:"previewUrl,omitempty"`
	DownloadURL string    `json:"downloadUrl,omitempty"`
}

type auditEvent struct {
	Event       string    `json:"event"`
	JobID       string    `json:"jobId"`
	CreatedAt   time.Time `json:"createdAt"`
	Email       string    `json:"email,omitempty"`
	IP          string    `json:"ip,omitempty"`
	RemoteAddr  string    `json:"remoteAddr,omitempty"`
	UserAgent   string    `json:"userAgent,omitempty"`
	CFRay       string    `json:"cfRay,omitempty"`
	Prompt      string    `json:"prompt"`
	CodexArgs   []string  `json:"codexArgs"`
	CodexPrompt string    `json:"codexPrompt"`
	WorkDir     string    `json:"workDir"`
}

type auditLine struct {
	Line  int         `json:"line"`
	Event *auditEvent `json:"event,omitempty"`
	Error string      `json:"error,omitempty"`
	Raw   string      `json:"raw,omitempty"`
}

type auditResponse struct {
	Emails []string    `json:"emails"`
	Lines  []auditLine `json:"lines"`
}

type usageLimitsResponse struct {
	OK        bool            `json:"ok"`
	UpdatedAt *time.Time      `json:"updatedAt,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func main() {
	cfg := parseConfig()

	root, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "runs"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp", "users"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0700); err != nil {
		log.Fatal(err)
	}

	app := &server{
		root:      root,
		auditPath: filepath.Join(root, "data", "audit.jsonl"),
		jobs:      make(map[string]*job),
		sem:       make(chan struct{}, 1),
	}

	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	staticVersion := mustStaticVersion()

	mux := http.NewServeMux()
	mux.HandleFunc("/audit", app.handleAuditPage(staticRoot, staticVersion))
	mux.HandleFunc("/audit/", app.handleAuditPage(staticRoot, staticVersion))
	mux.HandleFunc("/audit.html", app.handleAuditPage(staticRoot, staticVersion))
	mux.Handle("/", staticHandler(staticRoot, staticVersion))
	mux.Handle("/runs/", http.StripPrefix("/runs/", http.FileServer(http.Dir(filepath.Join(root, "runs")))))
	mux.HandleFunc("/api/audit", app.handleAudit)
	mux.HandleFunc("/api/usage-limits", app.handleUsageLimits)
	mux.HandleFunc("/api/work/jobs", app.handleWorkJobs)
	mux.HandleFunc("/api/work/jobs/", app.handleWorkJob)
	mux.HandleFunc("/api/work/files", app.handleWorkFiles)
	mux.HandleFunc("/api/work/files/upload", app.handleWorkFileUpload)
	mux.HandleFunc("/api/work/files/download", app.handleWorkFileDownload)
	mux.HandleFunc("/api/work/files/preview", app.handleWorkFilePreview)

	log.Printf("codex canvas local listening on http://%s", cfg.addr)
	log.Fatal(http.ListenAndServe(cfg.addr, mux))
}

type config struct {
	addr string
}

func parseConfig() config {
	addr := flag.String("addr", defaultAddr, "listen address, for example 127.0.0.1:8765")
	port := flag.String("port", "", "listen port on 127.0.0.1; overrides --addr when set")
	flag.Parse()

	if strings.TrimSpace(*port) != "" {
		return config{addr: "127.0.0.1:" + strings.TrimSpace(*port)}
	}
	return config{addr: strings.TrimSpace(*addr)}
}

func mustStaticVersion() string {
	h := sha256.New()
	for _, name := range []string{
		"static/index.html",
		"static/work.html",
		"static/audit.html",
		"static/app.js",
		"static/work.js",
		"static/audit.js",
		"static/styles.css",
		"static/base.css",
		"static/layout.css",
		"static/forms.css",
		"static/panels.css",
		"static/files.css",
		"static/logs.css",
		"static/responsive.css",
	} {
		b, err := staticFiles.ReadFile(name)
		if err != nil {
			log.Fatal(err)
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (s *server) handleAuditPage(root fs.FS, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/audit" && r.URL.Path != "/audit/" && r.URL.Path != "/audit.html" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isLocalUser(r) {
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}

		b, err := fs.ReadFile(root, "audit.html")
		if err != nil {
			http.Error(w, "audit page not found", http.StatusInternalServerError)
			return
		}
		html := strings.ReplaceAll(string(b), `href="/styles.css"`, `href="/styles.css?v=`+version+`"`)
		html = strings.ReplaceAll(html, `src="/audit.js"`, `src="/audit.js?v=`+version+`"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}
}

func staticHandler(root fs.FS, version string) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		if r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/work" || r.URL.Path == "/work/" {
			name := "index.html"
			if r.URL.Path == "/work" || r.URL.Path == "/work/" || isWorkHost(r.Host) {
				name = "work.html"
			}
			b, err := fs.ReadFile(root, name)
			if err != nil {
				http.Error(w, "index not found", http.StatusInternalServerError)
				return
			}
			html := strings.ReplaceAll(string(b), `href="/styles.css"`, `href="/styles.css?v=`+version+`"`)
			html = strings.ReplaceAll(html, `src="/app.js"`, `src="/app.js?v=`+version+`"`)
			html = strings.ReplaceAll(html, `src="/work.js"`, `src="/work.js?v=`+version+`"`)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, html)
			return
		}

		files.ServeHTTP(w, r)
	})
}

func isWorkHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "codex.io99.xyz"
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request) {
	userKey := s.userKey(r)
	s.mu.RLock()
	jobs := make([]jobView, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.UserKey != userKey {
			continue
		}
		jobs = append(jobs, j.snapshot())
	}
	s.mu.RUnlock()

	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].CreatedAt.After(jobs[k].CreatedAt)
	})
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) handleWorkJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createWorkJob(w, r)
	case http.MethodGet:
		s.listJobs(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleWorkJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/work/jobs/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	j := s.jobs[id]
	s.mu.RUnlock()
	if j == nil || j.UserKey != s.userKey(r) {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLocalUser(r) {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}

	resp, err := s.readAudit()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, auditResponse{})
			return
		}
		http.Error(w, "could not read audit log", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleUsageLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := s.refreshUsageLimits()
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) refreshUsageLimits() usageLimitsResponse {
	s.usageMu.Lock()
	if s.usageRun {
		resp := s.usage
		if !resp.OK && resp.Error == "" {
			resp.Error = "usage limit refresh already running"
		}
		s.usageMu.Unlock()
		return resp
	}
	s.usageRun = true
	s.usageMu.Unlock()

	resp := s.readUsageLimits()

	s.usageMu.Lock()
	s.usage = resp
	s.usageRun = false
	s.usageMu.Unlock()
	return resp
}

func (s *server) readUsageLimits() usageLimitsResponse {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	script := filepath.Join(s.root, "scripts", "codex_usage_limits.py")
	cmd := exec.CommandContext(ctx, "python", script, "--json")
	cmd.Dir = s.root
	out, err := cmd.CombinedOutput()
	now := time.Now()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return usageLimitsResponse{OK: false, UpdatedAt: &now, Error: "codex usage limit script timed out"}
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return usageLimitsResponse{OK: false, UpdatedAt: &now, Error: msg}
	}

	var raw json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return usageLimitsResponse{OK: false, UpdatedAt: &now, Error: "invalid usage limit json: " + err.Error()}
	}
	return usageLimitsResponse{OK: true, UpdatedAt: &now, Data: raw}
}

func (s *server) readAudit() (auditResponse, error) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	f, err := os.Open(s.auditPath)
	if err != nil {
		return auditResponse{}, err
	}
	defer f.Close()

	var lines []auditLine
	emailSet := map[string]bool{}
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}

		var event auditEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			lines = append(lines, auditLine{Line: lineNo, Error: err.Error(), Raw: raw})
			continue
		}
		email := strings.TrimSpace(event.Email)
		if email == "" {
			email = "local"
		}
		emailSet[email] = true
		lines = append(lines, auditLine{Line: lineNo, Event: &event})
	}
	if err := scanner.Err(); err != nil {
		return auditResponse{}, err
	}

	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	emails := make([]string, 0, len(emailSet))
	for email := range emailSet {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return auditResponse{Emails: emails, Lines: lines}, nil
}

func (s *server) createWorkJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	contentType := r.Header.Get("Content-Type")
	var prompt string
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		prompt = strings.TrimSpace(r.FormValue("prompt"))
	} else {
		var body struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		prompt = strings.TrimSpace(body.Prompt)
	}
	if prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if len(prompt) > maxPrompt {
		http.Error(w, fmt.Sprintf("prompt is too long; max %d bytes", maxPrompt), http.StatusBadRequest)
		return
	}

	id, err := newID()
	if err != nil {
		http.Error(w, "could not create job id", http.StatusInternalServerError)
		return
	}

	j := &job{
		ID:        id,
		Mode:      "work",
		UserKey:   s.userKey(r),
		Email:     displayEmail(accessEmail(r)),
		Prompt:    prompt,
		WorkDir:   s.userWorkDir(r),
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	if err := os.MkdirAll(j.WorkDir, 0755); err != nil {
		http.Error(w, "could not create user directory", http.StatusInternalServerError)
		return
	}
	if err := s.writeAuditEvent(newAuditEvent(r, j)); err != nil {
		http.Error(w, "could not write audit log", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()

	go s.notifyJobSubmitted(j)
	go s.runJob(j)
	writeJSON(w, http.StatusAccepted, createJobResponse{ID: id})
}

func (s *server) runJob(j *job) {
	exitCode := -1
	defer func() {
		go s.refreshUsageLimits()
		s.notifyJobFinished(j, exitCode)
	}()

	s.appendLog(j, "Waiting for the local Codex runner...\n")
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	start := time.Now()
	j.setStatus("running", start, nil)

	if err := os.MkdirAll(j.WorkDir, 0755); err != nil {
		j.fail(fmt.Errorf("create session workdir: %w", err))
		return
	}

	before := knownImages(s.root, j.WorkDir)
	arg := buildCodexPrompt(j)
	args := buildCodexArgs()
	s.appendLog(j, "Session workdir: %s\n", j.WorkDir)
	s.appendLog(j, "Running: codex %s\n", quoteArgs(args))
	s.appendLog(j, "Prompt via stdin:\n%s\n\n", arg)

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Dir = j.WorkDir
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	cmd.Stdin = strings.NewReader(arg)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		j.fail(fmt.Errorf("stdout pipe: %w", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		j.fail(fmt.Errorf("stderr pipe: %w", err))
		return
	}
	if err := cmd.Start(); err != nil {
		j.fail(fmt.Errorf("start codex: %w", err))
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipe(&wg, stdout, func(line string) { s.appendLog(j, "%s\n", line) })
	go streamPipe(&wg, stderr, func(line string) { s.appendLog(j, "%s\n", line) })

	waitErr := cmd.Wait()
	wg.Wait()
	exitCode = cmd.ProcessState.ExitCode()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		j.fail(fmt.Errorf("codex timed out after %s", runTimeout))
		return
	}
	if waitErr != nil {
		j.fail(fmt.Errorf("codex exited with error: %w", waitErr))
		return
	}

	images := s.collectImages(j, before, start)
	j.mu.Lock()
	j.Images = images
	if len(images) == 0 {
		j.Log += "\nCodex finished, but no new image file was detected. Check the log for the generated path.\n"
	}
	now := time.Now()
	j.FinishedAt = &now
	j.Status = "succeeded"
	j.mu.Unlock()
}

func (s *server) notifyJobSubmitted(j *job) {
	s.sendNotification("New Codex prompt", submittedNotificationMessage(j), 15000, j)
}

func (s *server) notifyJobFinished(j *job, exitCode int) {
	s.sendNotification("Codex job finished", finishedNotificationMessage(j, exitCode), 10000, j)
}

func submittedNotificationMessage(j *job) string {
	return fmt.Sprintf("User: %s\n\nPrompt:\n%s", j.Email, j.Prompt)
}

func finishedNotificationMessage(j *job, exitCode int) string {
	status := j.snapshot()
	message := fmt.Sprintf("User: %s\nJob: %s\nExit code: %d", j.Email, j.ID, exitCode)
	if exitCode != 0 {
		failure := strings.TrimSpace(status.Error)
		if failure == "" {
			failure = "Job failed."
		}
		message += "\n\nFAILED: " + failure
	}
	return message
}

func (s *server) sendNotification(title, message string, durationMs int, j *job) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(struct {
		Title      string `json:"title"`
		Message    string `json:"message"`
		DurationMs int    `json:"durationMs"`
	}{
		Title:      title,
		Message:    message,
		DurationMs: durationMs,
	}); err != nil {
		s.appendLog(j, "Notification encode failed: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, notifyURL, &body)
	if err != nil {
		s.appendLog(j, "Notification request failed: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.appendLog(j, "Notification delivery failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.appendLog(j, "Notification delivery returned HTTP %d\n", resp.StatusCode)
	}
}

func streamPipe(wg *sync.WaitGroup, r io.Reader, appendLine func(string)) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		appendLine(scanner.Text())
	}
}

func (s *server) handleWorkFiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listWorkFiles(w, r)
	case http.MethodDelete:
		s.deleteWorkFile(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) listWorkFiles(w http.ResponseWriter, r *http.Request) {
	root := s.userWorkDir(r)
	target, rel, err := safeUserPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		http.Error(w, "could not create user directory", http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !info.IsDir() {
		http.Error(w, "path is not a directory", http.StatusBadRequest)
		return
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		http.Error(w, "could not read directory", http.StatusInternalServerError)
		return
	}
	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		childRel := filepath.ToSlash(filepath.Join(rel, entry.Name()))
		if rel == "." || rel == "" {
			childRel = filepath.ToSlash(entry.Name())
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		item := fileEntry{
			Name:    entry.Name(),
			Path:    childRel,
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			IsImage: imageExts[ext],
		}
		if !item.IsDir {
			item.DownloadURL = "/api/work/files/download?path=" + urlQueryEscape(childRel)
			if item.IsImage {
				item.PreviewURL = "/api/work/files/preview?path=" + urlQueryEscape(childRel)
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].IsDir != out[k].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[k].Name)
	})
	writeJSON(w, http.StatusOK, struct {
		Path    string      `json:"path"`
		Entries []fileEntry `json:"entries"`
	}{Path: rel, Entries: out})
}

func (s *server) handleWorkFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	root := s.userWorkDir(r)
	target, _, err := safeUserPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		http.Error(w, "could not create upload directory", http.StatusInternalServerError)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "files are required", http.StatusBadRequest)
		return
	}
	seen := map[string]int{}
	saved := make([]fileEntry, 0, len(files))
	for _, header := range files {
		name := sanitizeFileName(header.Filename)
		if name == "" {
			http.Error(w, "invalid file name", http.StatusBadRequest)
			return
		}
		name = uniqueName(name, seen)
		dest, _, err := safeUserPath(root, filepath.Join(r.URL.Query().Get("path"), name))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		src, err := header.Open()
		if err != nil {
			http.Error(w, "could not open upload", http.StatusBadRequest)
			return
		}
		err = writeUploadedFile(src, dest)
		if closeErr := src.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			http.Error(w, "could not save upload", http.StatusInternalServerError)
			return
		}
		info, err := os.Stat(dest)
		if err != nil {
			http.Error(w, "could not stat upload", http.StatusInternalServerError)
			return
		}
		rel, err := filepath.Rel(root, dest)
		if err != nil {
			http.Error(w, "could not resolve upload", http.StatusInternalServerError)
			return
		}
		rel = filepath.ToSlash(rel)
		ext := strings.ToLower(filepath.Ext(name))
		item := fileEntry{
			Name:    name,
			Path:    rel,
			IsDir:   false,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			IsImage: imageExts[ext],
		}
		item.DownloadURL = "/api/work/files/download?path=" + urlQueryEscape(rel)
		if item.IsImage {
			item.PreviewURL = "/api/work/files/preview?path=" + urlQueryEscape(rel)
		}
		saved = append(saved, item)
	}
	writeJSON(w, http.StatusCreated, struct {
		Status string      `json:"status"`
		Files  []fileEntry `json:"files"`
	}{Status: "ok", Files: saved})
}

func (s *server) handleWorkFileDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root := s.userWorkDir(r)
	target, _, err := safeUserPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(target)))
	http.ServeFile(w, r, target)
}

func (s *server) handleWorkFilePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root := s.userWorkDir(r)
	target, _, err := safeUserPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() || !imageExts[strings.ToLower(filepath.Ext(target))] {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, target)
}

func (s *server) deleteWorkFile(w http.ResponseWriter, r *http.Request) {
	root := s.userWorkDir(r)
	target, rel, err := safeUserPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if rel == "." || rel == "" {
		http.Error(w, "refusing to delete user root", http.StatusBadRequest)
		return
	}
	if err := os.RemoveAll(target); err != nil {
		http.Error(w, "could not delete path", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func buildCodexArgs() []string {
	return []string{
		"exec",
		"--model", "gpt-5.5",
		"-c", `model_reasoning_effort="low"`,
		"--disable", "memories",
		"--skip-git-repo-check",
		"--sandbox", "workspace-write",
		"--color", "never",
	}
}

func buildCodexPrompt(j *job) string {
	return j.Prompt
}

func (s *server) collectImages(j *job, before map[string]time.Time, started time.Time) []imageInfo {
	discovered := knownImages(s.root, j.WorkDir)
	logPaths := imagePathsFromText(j.snapshot().Log)
	for _, p := range logPaths {
		if abs, err := filepath.Abs(p); err == nil {
			discovered[abs] = time.Time{}
		}
	}

	workOutputDir := filepath.Join(j.WorkDir, "outputs", j.ID)
	runsRelDir := filepath.ToSlash(filepath.Join("users", publicUserKey(j.UserKey), "outputs", j.ID))
	runsDir := filepath.Join(s.root, "runs", filepath.FromSlash(runsRelDir))
	if err := os.MkdirAll(workOutputDir, 0755); err != nil {
		s.appendLog(j, "\nCould not create image output directory: %v\n", err)
		return nil
	}
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		s.appendLog(j, "\nCould not create public image output directory: %v\n", err)
		return nil
	}

	var out []imageInfo
	seenNames := map[string]int{}
	var candidates []imageCandidate
	for p := range discovered {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		if beforeModTime, existed := before[p]; existed && !info.ModTime().After(beforeModTime) {
			continue
		}
		if info.ModTime().Before(started.Add(-5 * time.Second)) {
			continue
		}
		if !imageExts[strings.ToLower(filepath.Ext(p))] {
			continue
		}
		hash, err := fileSHA256(p)
		if err != nil {
			s.appendLog(j, "\nCould not hash generated image %s: %v\n", p, err)
			continue
		}
		candidates = append(candidates, imageCandidate{
			Path:    p,
			Hash:    hash,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}

	sort.Slice(candidates, func(i, k int) bool {
		return betterImageCandidate(candidates[i], candidates[k])
	})

	seenHashes := map[string]struct{}{}
	for _, candidate := range candidates {
		if _, ok := seenHashes[candidate.Hash]; ok {
			continue
		}
		seenHashes[candidate.Hash] = struct{}{}

		name := sanitizeFileName(filepath.Base(candidate.Path))
		if name == "" {
			name = "image" + strings.ToLower(filepath.Ext(candidate.Path))
		}
		name = uniqueName(name, seenNames)
		workDest := filepath.Join(workOutputDir, name)
		if err := copyFile(candidate.Path, workDest); err != nil {
			s.appendLog(j, "\nCould not copy generated image %s: %v\n", candidate.Path, err)
			continue
		}
		runsDest := filepath.Join(runsDir, name)
		if err := copyFile(workDest, runsDest); err != nil {
			s.appendLog(j, "\nCould not mirror generated image %s: %v\n", workDest, err)
			continue
		}
		if copied, err := os.Stat(runsDest); err == nil {
			out = append(out, imageInfo{
				Name: name,
				URL:  "/runs/" + runsRelDir + "/" + url.PathEscape(name),
				Size: copied.Size(),
			})
		}
	}

	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

type imageCandidate struct {
	Path    string
	Hash    string
	Size    int64
	ModTime time.Time
}

func betterImageCandidate(a, b imageCandidate) bool {
	aName := strings.ToLower(filepath.Base(a.Path))
	bName := strings.ToLower(filepath.Base(b.Path))
	aDefault := strings.HasPrefix(aName, "ig_")
	bDefault := strings.HasPrefix(bName, "ig_")
	if aDefault != bDefault {
		return !aDefault
	}
	if !a.ModTime.Equal(b.ModTime) {
		return a.ModTime.After(b.ModTime)
	}
	return aName < bName
}

func knownImages(root string, extraDirs ...string) map[string]time.Time {
	dirs := imageSearchDirs(root, extraDirs...)
	found := make(map[string]time.Time)
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !imageExts[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			abs, err := filepath.Abs(path)
			if err == nil {
				if info, statErr := d.Info(); statErr == nil {
					found[abs] = info.ModTime()
				}
			}
			return nil
		})
	}
	return found
}

func imageSearchDirs(root string, extraDirs ...string) []string {
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			dirs = append(dirs, p)
		}
	}

	add(filepath.Join(root, "output", "imagegen"))
	add(filepath.Join(root, "tmp", "imagegen"))
	add(filepath.Join(root, "runs"))
	for _, dir := range extraDirs {
		add(dir)
	}

	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		add(filepath.Join(codexHome, "generated_images"))
		add(filepath.Join(codexHome, "output", "imagegen"))
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		add(filepath.Join(userProfile, ".codex", "generated_images"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".codex", "generated_images"))
	}

	return dirs
}

func imagePathsFromText(text string) []string {
	re := regexp.MustCompile(`(?i)([A-Za-z]:\\[^\r\n"'<>|]+?\.(?:png|jpe?g|jfif|webp|gif)|/[^\s"'<>|]+?\.(?:png|jpe?g|jfif|webp|gif))`)
	matches := re.FindAllString(text, -1)
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		paths = append(paths, strings.TrimSpace(m))
	}
	return paths
}

func (j *job) snapshot() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()

	return jobView{
		ID:         j.ID,
		Mode:       j.Mode,
		Prompt:     j.Prompt,
		WorkDir:    j.WorkDir,
		Status:     j.Status,
		CreatedAt:  j.CreatedAt,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
		Log:        j.Log,
		Error:      j.Error,
		Images:     append([]imageInfo(nil), j.Images...),
	}
}

func (j *job) setStatus(status string, started time.Time, finished *time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = status
	j.StartedAt = &started
	j.FinishedAt = finished
}

func (j *job) fail(err error) {
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = "failed"
	j.Error = err.Error()
	j.FinishedAt = &now
	j.Log += "\nError: " + err.Error() + "\n"
}

func (s *server) appendLog(j *job, format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Log += fmt.Sprintf(format, args...)
}

func (s *server) userWorkDir(r *http.Request) string {
	return filepath.Join(s.root, "tmp", "users", s.userKey(r))
}

func (s *server) userKey(r *http.Request) string {
	return userWorkDirKey(accessEmail(r))
}

func userWorkDirKey(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "local"
	}
	sum := sha256.Sum256([]byte(email))
	prefix := sanitizeFileName(strings.Split(email, "@")[0])
	if prefix == "" {
		prefix = "user"
	}
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	return prefix + "-" + hex.EncodeToString(sum[:])[:16]
}

func publicUserKey(userKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(userKey)))
	return hex.EncodeToString(sum[:])[:16]
}

func newAuditEvent(r *http.Request, j *job) auditEvent {
	email := accessEmail(r)

	return auditEvent{
		Event:       "job_created",
		JobID:       j.ID,
		CreatedAt:   j.CreatedAt,
		Email:       email,
		IP:          clientIP(r),
		RemoteAddr:  r.RemoteAddr,
		UserAgent:   strings.TrimSpace(r.UserAgent()),
		CFRay:       strings.TrimSpace(r.Header.Get("Cf-Ray")),
		Prompt:      j.Prompt,
		CodexArgs:   buildCodexArgs(),
		CodexPrompt: buildCodexPrompt(j),
		WorkDir:     j.WorkDir,
	}
}

func accessEmail(r *http.Request) string {
	return firstHeader(r,
		"Cf-Access-Authenticated-User-Email",
		"CF-Access-Authenticated-User-Email",
		"X-Forwarded-Email",
	)
}

func displayEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "local"
	}
	return email
}

func isLocalUser(r *http.Request) bool {
	return accessEmail(r) == ""
}

func (s *server) writeAuditEvent(event auditEvent) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.auditPath), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	return enc.Encode(event)
}

func firstHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func clientIP(r *http.Request) string {
	if value := firstHeader(r, "Cf-Connecting-Ip", "True-Client-Ip", "X-Real-Ip"); value != "" {
		return value
	}
	if forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeUploadedFile(src io.Reader, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	return out.Close()
}

func safeUserPath(root, raw string) (string, string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "."
	}
	raw = filepath.FromSlash(strings.TrimSpace(raw))
	if filepath.IsAbs(raw) {
		return "", "", fmt.Errorf("absolute paths are not allowed")
	}
	clean := filepath.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path traversal is not allowed")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return "", "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path escapes user directory")
	}
	return targetAbs, filepath.ToSlash(rel), nil
}

func urlQueryEscape(value string) string {
	return url.QueryEscape(filepath.ToSlash(value))
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.ReplaceAll(name, "*", "_")
	name = strings.ReplaceAll(name, "?", "_")
	name = strings.ReplaceAll(name, "\"", "_")
	name = strings.ReplaceAll(name, "<", "_")
	name = strings.ReplaceAll(name, ">", "_")
	name = strings.ReplaceAll(name, "|", "_")
	return name
}

func uniqueName(name string, seen map[string]int) string {
	seen[name]++
	if seen[name] == 1 {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s-%d%s", base, seen[name], ext)
}

func quoteArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\r\n\"") {
			parts = append(parts, fmt.Sprintf("%q", arg))
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}
