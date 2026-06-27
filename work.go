package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

func (s *server) handleWorkJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createWorkJob(w, r)
	case http.MethodGet:
		s.listJobs(w, r, "work")
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
		if err := jsonDecode(r.Body, &body); err != nil {
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
	if err := s.writeAuditEvent(newAuditWorkEvent(r, j)); err != nil {
		http.Error(w, "could not write audit log", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()

	go s.notifyWorkSubmitted(j)
	go s.runJob(j)
	writeJSON(w, http.StatusAccepted, createJobResponse{ID: id})
}

func (s *server) runJob(j *job) {
	exitCode := -1
	defer func() {
		go s.refreshUsageLimits()
		s.notifyWorkFinished(j, exitCode)
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

func (s *server) notifyWorkSubmitted(j *job) {
	s.sendNotification("New Codex prompt", workSubmittedMessage(j), 15000, j)
}

func (s *server) notifyWorkFinished(j *job, exitCode int) {
	s.sendNotification("Codex job finished", workFinishedMessage(j, exitCode), 10000, j)
}

func workSubmittedMessage(j *job) string {
	return fmt.Sprintf("User: %s\n\nPrompt:\n%s", j.Email, j.Prompt)
}

func workFinishedMessage(j *job, exitCode int) string {
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

func newAuditWorkEvent(r *http.Request, j *job) auditEvent {
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
