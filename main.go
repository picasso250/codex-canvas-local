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
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

const (
	defaultAddr       = "127.0.0.1:8765"
	defaultNotifyPort = "25378"
	runTimeout        = 60 * time.Minute
	maxPrompt         = 12000
	maxUploadBody     = 128 << 20
	notifyTimeout     = 1500 * time.Millisecond
)

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".jfif": true, ".webp": true, ".gif": true,
}

type server struct {
	root      string
	auditPath string
	store     *jobStore
	jobs      map[string]*job
	mu        sync.RWMutex
	auditMu   sync.Mutex
	usageMu   sync.Mutex
	usage     usageLimitsResponse
	usageRun  bool
	sem       chan struct{}
	picSem    chan struct{}
	notifyURL string
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
	Event            string      `json:"event"`
	JobID            string      `json:"jobId"`
	CreatedAt        time.Time   `json:"createdAt"`
	FinishedAt       *time.Time  `json:"finishedAt,omitempty"`
	Email            string      `json:"email,omitempty"`
	IP               string      `json:"ip,omitempty"`
	RemoteAddr       string      `json:"remoteAddr,omitempty"`
	UserAgent        string      `json:"userAgent,omitempty"`
	CFRay            string      `json:"cfRay,omitempty"`
	Prompt           string      `json:"prompt"`
	CodexArgs        []string    `json:"codexArgs"`
	CodexPrompt      string      `json:"codexPrompt"`
	WorkDir          string      `json:"workDir"`
	Status           string      `json:"status,omitempty"`
	Error            string      `json:"error,omitempty"`
	DaemonCode       string      `json:"daemonCode,omitempty"`
	DaemonMessage    string      `json:"daemonMessage,omitempty"`
	DaemonRequestID  string      `json:"daemonRequestId,omitempty"`
	DaemonCurrentURL string      `json:"daemonCurrentUrl,omitempty"`
	Images           []imageInfo `json:"images,omitempty"`
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
	store, err := openJobStore(filepath.Join(root, "data", "jobs.sqlite"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.close()
	if err := store.markInterruptedJobs("service restarted before job completed"); err != nil {
		log.Fatal(err)
	}

	app := &server{
		root:      root,
		auditPath: filepath.Join(root, "data", "audit.jsonl"),
		store:     store,
		jobs:      make(map[string]*job),
		sem:       make(chan struct{}, 1),
		picSem:    make(chan struct{}, 1),
		notifyURL: cfg.notifyURL,
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
	mux.HandleFunc("/api/jobs", app.handleActiveJobs)
	mux.HandleFunc("/api/usage-limits", app.handleUsageLimits)
	mux.HandleFunc("/api/pic/jobs", app.handlePicJobs)
	mux.HandleFunc("/api/pic/jobs/", app.handlePicJob)
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
	addr      string
	notifyURL string
}

func parseConfig() config {
	addr := flag.String("addr", defaultAddr, "listen address, for example 127.0.0.1:8765")
	port := flag.String("port", "", "listen port on 127.0.0.1; overrides --addr when set")
	flag.Parse()

	notifyURL, err := winNotifyURL(os.Getenv("WINNOTIFYAPI_PORT"))
	if err != nil {
		log.Fatal(err)
	}

	if strings.TrimSpace(*port) != "" {
		return config{addr: "127.0.0.1:" + strings.TrimSpace(*port), notifyURL: notifyURL}
	}
	return config{addr: strings.TrimSpace(*addr), notifyURL: notifyURL}
}

func winNotifyURL(port string) (string, error) {
	port = strings.TrimSpace(port)
	if port == "" {
		port = defaultNotifyPort
	}
	if len(port) > 5 {
		return "", fmt.Errorf("invalid WINNOTIFYAPI_PORT %q: use a TCP port from 1024 to 65535", port)
	}
	for _, ch := range port {
		if ch < '0' || ch > '9' {
			return "", fmt.Errorf("invalid WINNOTIFYAPI_PORT %q: use a TCP port from 1024 to 65535", port)
		}
	}
	var portNumber int
	if _, err := fmt.Sscanf(port, "%d", &portNumber); err != nil || portNumber < 1024 || portNumber > 65535 {
		return "", fmt.Errorf("invalid WINNOTIFYAPI_PORT %q: use a TCP port from 1024 to 65535", port)
	}
	return "http://127.0.0.1:" + port + "/notify", nil
}

func mustStaticVersion() string {
	h := sha256.New()
	for _, name := range []string{
		"static/index.html",
		"static/work.html",
		"static/pic.html",
		"static/audit.html",
		"static/app.js",
		"static/work.js",
		"static/pic.js",
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

		if r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/work" || r.URL.Path == "/work/" || r.URL.Path == "/pic" || r.URL.Path == "/pic/" {
			name := "index.html"
			if r.URL.Path == "/work" || r.URL.Path == "/work/" || isWorkHost(r.Host) {
				name = "work.html"
			} else if r.URL.Path == "/pic" || r.URL.Path == "/pic/" || isPicHost(r.Host) {
				name = "pic.html"
			}
			b, err := fs.ReadFile(root, name)
			if err != nil {
				http.Error(w, "index not found", http.StatusInternalServerError)
				return
			}
			html := strings.ReplaceAll(string(b), `href="/styles.css"`, `href="/styles.css?v=`+version+`"`)
			html = strings.ReplaceAll(html, `src="/app.js"`, `src="/app.js?v=`+version+`"`)
			html = strings.ReplaceAll(html, `src="/work.js"`, `src="/work.js?v=`+version+`"`)
			html = strings.ReplaceAll(html, `src="/pic.js"`, `src="/pic.js?v=`+version+`"`)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, html)
			return
		}

		files.ServeHTTP(w, r)
	})
}

func isWorkHost(host string) bool {
	host = normalizedHost(host)
	return host == "codex.io99.xyz"
}

func isPicHost(host string) bool {
	host = normalizedHost(host)
	return host == "pic.io99.xyz"
}

func normalizedHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request, mode string) {
	userKey := s.userKey(r)
	if s.store != nil {
		jobs, err := s.store.listJobs(userKey, mode)
		if err != nil {
			http.Error(w, "could not read jobs", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, jobs)
		return
	}

	s.mu.RLock()
	jobs := make([]jobView, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.UserKey != userKey || j.Mode != mode {
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

func (s *server) handleActiveJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLocalUser(r) {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	if s.store != nil {
		jobs, err := s.store.activeJobs()
		if err != nil {
			http.Error(w, "could not read jobs", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, jobs)
		return
	}

	s.mu.RLock()
	jobs := make([]jobView, 0, len(s.jobs))
	for _, j := range s.jobs {
		status := j.snapshot()
		if status.Status != "queued" && status.Status != "running" {
			continue
		}
		jobs = append(jobs, status)
	}
	s.mu.RUnlock()

	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].CreatedAt.After(jobs[k].CreatedAt)
	})
	writeJSON(w, http.StatusOK, jobs)
}

func jsonDecode(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.notifyURL, &body)
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

func (s *server) storeJob(j *job) error {
	if s.store == nil {
		return nil
	}
	return s.store.upsertJob(j)
}

func (s *server) storeRecoveredJob(status jobView, userKey, email string) error {
	if s.store == nil {
		return nil
	}
	j := &job{
		ID:         status.ID,
		Mode:       status.Mode,
		UserKey:    userKey,
		Email:      email,
		Prompt:     status.Prompt,
		WorkDir:    status.WorkDir,
		Status:     status.Status,
		CreatedAt:  status.CreatedAt,
		StartedAt:  status.StartedAt,
		FinishedAt: status.FinishedAt,
		Log:        status.Log,
		Error:      status.Error,
		Images:     status.Images,
	}
	if err := s.store.upsertJob(j); err != nil {
		return err
	}
	return s.store.replaceImages(j.ID, status.Images)
}

func (s *server) updateStoredJob(j *job) {
	if s.store == nil {
		return
	}
	if err := s.store.updateJob(j.snapshot()); err != nil {
		log.Printf("could not update job %s: %v", j.ID, err)
	}
}

func (s *server) setJobStatus(j *job, status string, started time.Time, finished *time.Time) {
	j.setStatus(status, started, finished)
	s.updateStoredJob(j)
}

func (s *server) failJob(j *job, err error) {
	j.fail(err)
	s.updateStoredJob(j)
}

func (s *server) finishJob(j *job, images []imageInfo) {
	j.mu.Lock()
	j.Images = images
	j.Status = "succeeded"
	now := time.Now()
	j.FinishedAt = &now
	j.mu.Unlock()
	if s.store != nil {
		if err := s.store.updateJob(j.snapshot()); err != nil {
			log.Printf("could not finish job %s: %v", j.ID, err)
		}
		if err := s.store.replaceImages(j.ID, images); err != nil {
			log.Printf("could not store images for job %s: %v", j.ID, err)
		}
	}
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
	j.Log += fmt.Sprintf(format, args...)
	status := jobView{
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
	j.mu.Unlock()
	if s.store != nil {
		if err := s.store.updateJob(status); err != nil {
			log.Printf("could not append job log %s: %v", j.ID, err)
		}
	}
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
