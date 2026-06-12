package main

import (
	"bufio"
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
	runTimeout    = 20 * time.Minute
	maxPrompt     = 12000
	maxUploadBody = 128 << 20
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
	sem       chan struct{}
}

type job struct {
	ID              string          `json:"id"`
	Prompt          string          `json:"prompt"`
	WorkDir         string          `json:"workDir"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"createdAt"`
	StartedAt       *time.Time      `json:"startedAt,omitempty"`
	FinishedAt      *time.Time      `json:"finishedAt,omitempty"`
	Log             string          `json:"log"`
	Error           string          `json:"error,omitempty"`
	Images          []imageInfo     `json:"images"`
	ReferenceImages []referenceInfo `json:"referenceImages"`

	mu sync.Mutex
}

type jobView struct {
	ID              string          `json:"id"`
	Prompt          string          `json:"prompt"`
	WorkDir         string          `json:"workDir"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"createdAt"`
	StartedAt       *time.Time      `json:"startedAt,omitempty"`
	FinishedAt      *time.Time      `json:"finishedAt,omitempty"`
	Log             string          `json:"log"`
	Error           string          `json:"error,omitempty"`
	Images          []imageInfo     `json:"images"`
	ReferenceImages []referenceInfo `json:"referenceImages"`
}

type imageInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type referenceInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type createJobResponse struct {
	ID string `json:"id"`
}

type auditEvent struct {
	Event           string          `json:"event"`
	JobID           string          `json:"jobId"`
	CreatedAt       time.Time       `json:"createdAt"`
	Email           string          `json:"email,omitempty"`
	IP              string          `json:"ip,omitempty"`
	RemoteAddr      string          `json:"remoteAddr,omitempty"`
	UserAgent       string          `json:"userAgent,omitempty"`
	CFRay           string          `json:"cfRay,omitempty"`
	Prompt          string          `json:"prompt"`
	CodexArgs       []string        `json:"codexArgs"`
	CodexPrompt     string          `json:"codexPrompt"`
	WorkDir         string          `json:"workDir"`
	ReferenceImages []referenceInfo `json:"referenceImages,omitempty"`
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
	mux.Handle("/", staticHandler(staticRoot, staticVersion))
	mux.Handle("/runs/", http.StripPrefix("/runs/", http.FileServer(http.Dir(filepath.Join(root, "runs")))))
	mux.HandleFunc("/api/jobs", app.handleJobs)
	mux.HandleFunc("/api/jobs/", app.handleJob)

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
	for _, name := range []string{"static/index.html", "static/app.js", "static/styles.css"} {
		b, err := staticFiles.ReadFile(name)
		if err != nil {
			log.Fatal(err)
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func staticHandler(root fs.FS, version string) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			b, err := fs.ReadFile(root, "index.html")
			if err != nil {
				http.Error(w, "index not found", http.StatusInternalServerError)
				return
			}
			html := strings.ReplaceAll(string(b), `href="/styles.css"`, `href="/styles.css?v=`+version+`"`)
			html = strings.ReplaceAll(html, `src="/app.js"`, `src="/app.js?v=`+version+`"`)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, html)
			return
		}

		files.ServeHTTP(w, r)
	})
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createJob(w, r)
	case http.MethodGet:
		s.listJobs(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) createJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}

	prompt := strings.TrimSpace(r.FormValue("prompt"))
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

	workDir := s.userWorkDir(r)
	uploadsDir := filepath.Join(workDir, "uploads", id)
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		http.Error(w, "could not create session directory", http.StatusInternalServerError)
		return
	}

	refs, err := s.saveReferenceImages(r, uploadsDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	j := &job{
		ID:              id,
		Prompt:          prompt,
		WorkDir:         workDir,
		Status:          "queued",
		CreatedAt:       time.Now(),
		ReferenceImages: refs,
	}

	if err := s.writeAuditEvent(newAuditEvent(r, j)); err != nil {
		http.Error(w, "could not write audit log", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()

	go s.runJob(j)
	writeJSON(w, http.StatusAccepted, createJobResponse{ID: id})
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	jobs := make([]jobView, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j.snapshot())
	}
	s.mu.RUnlock()

	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].CreatedAt.After(jobs[k].CreatedAt)
	})
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	j := s.jobs[id]
	s.mu.RUnlock()
	if j == nil {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *server) runJob(j *job) {
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
	if len(j.ReferenceImages) > 0 {
		s.appendLog(j, "Reference images:\n")
		for i, ref := range j.ReferenceImages {
			s.appendLog(j, "  %d. %s\n", i+1, ref.Path)
		}
	}
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

func streamPipe(wg *sync.WaitGroup, r io.Reader, appendLine func(string)) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		appendLine(scanner.Text())
	}
}

func (s *server) saveReferenceImages(r *http.Request, uploadsDir string) ([]referenceInfo, error) {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil, nil
	}

	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		return nil, nil
	}

	refs := make([]referenceInfo, 0, len(files))
	seen := map[string]int{}
	for _, header := range files {
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !imageExts[ext] {
			return nil, fmt.Errorf("unsupported reference image type: %s", header.Filename)
		}

		name := sanitizeFileName(header.Filename)
		if name == "" {
			name = "reference" + ext
		}
		name = uniqueName(name, seen)
		dest := filepath.Join(uploadsDir, name)

		src, err := header.Open()
		if err != nil {
			return nil, fmt.Errorf("open reference image %s: %w", header.Filename, err)
		}
		err = writeUploadedFile(src, dest)
		if closeErr := src.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, fmt.Errorf("save reference image %s: %w", header.Filename, err)
		}

		abs, err := filepath.Abs(dest)
		if err != nil {
			return nil, fmt.Errorf("resolve reference image path: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("stat reference image: %w", err)
		}
		refs = append(refs, referenceInfo{
			Name: name,
			Path: abs,
			Size: info.Size(),
		})
	}
	return refs, nil
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
	if len(j.ReferenceImages) == 0 {
		return "use skill $imagegen : " + j.Prompt
	}

	var b strings.Builder
	b.WriteString("you have ")
	for i, ref := range j.ReferenceImages {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(fmt.Sprintf("%d. %s", i+1, ref.Path))
	}
	b.WriteString(" ; use skill $imagegen : ")
	b.WriteString(j.Prompt)
	return b.String()
}

func (s *server) collectImages(j *job, before map[string]time.Time, started time.Time) []imageInfo {
	discovered := knownImages(s.root, j.WorkDir)
	logPaths := imagePathsFromText(j.snapshot().Log)
	for _, p := range logPaths {
		if abs, err := filepath.Abs(p); err == nil {
			discovered[abs] = time.Time{}
		}
	}

	destDir := filepath.Join(s.root, "runs", j.ID)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		s.appendLog(j, "\nCould not create image output directory: %v\n", err)
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
		dest := filepath.Join(destDir, name)
		if err := copyFile(candidate.Path, dest); err != nil {
			s.appendLog(j, "\nCould not copy generated image %s: %v\n", candidate.Path, err)
			continue
		}
		if copied, err := os.Stat(dest); err == nil {
			out = append(out, imageInfo{
				Name: name,
				URL:  "/runs/" + j.ID + "/" + name,
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
		ID:              j.ID,
		Prompt:          j.Prompt,
		WorkDir:         j.WorkDir,
		Status:          j.Status,
		CreatedAt:       j.CreatedAt,
		StartedAt:       j.StartedAt,
		FinishedAt:      j.FinishedAt,
		Log:             j.Log,
		Error:           j.Error,
		Images:          append([]imageInfo(nil), j.Images...),
		ReferenceImages: append([]referenceInfo(nil), j.ReferenceImages...),
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
	return filepath.Join(s.root, "tmp", "users", userWorkDirKey(accessEmail(r)))
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

func newAuditEvent(r *http.Request, j *job) auditEvent {
	email := accessEmail(r)

	return auditEvent{
		Event:           "job_created",
		JobID:           j.ID,
		CreatedAt:       j.CreatedAt,
		Email:           email,
		IP:              clientIP(r),
		RemoteAddr:      r.RemoteAddr,
		UserAgent:       strings.TrimSpace(r.UserAgent()),
		CFRay:           strings.TrimSpace(r.Header.Get("Cf-Ray")),
		Prompt:          j.Prompt,
		CodexArgs:       buildCodexArgs(),
		CodexPrompt:     buildCodexPrompt(j),
		WorkDir:         j.WorkDir,
		ReferenceImages: append([]referenceInfo(nil), j.ReferenceImages...),
	}
}

func accessEmail(r *http.Request) string {
	return firstHeader(r,
		"Cf-Access-Authenticated-User-Email",
		"CF-Access-Authenticated-User-Email",
		"X-Forwarded-Email",
	)
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
