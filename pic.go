package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	daemonPort    = 53166
	daemonHost    = "127.0.0.1"
	daemonProbeTO = 1 * time.Second
)

var daemonScriptPath = filepath.Join("scripts", "chatgpt_agent.py")

type picAskRequest struct {
	Prompt        string   `json:"prompt"`
	Timeout       float64  `json:"timeout"`
	StableSeconds float64  `json:"stable_seconds"`
	Images        []string `json:"images"`
	Workdir       string   `json:"workdir"`
}

type picAskResponse struct {
	OK         bool     `json:"ok"`
	Response   string   `json:"response"`
	Images     []string `json:"images"`
	CurrentURL string   `json:"current_url"`
	RequestID  string   `json:"request_id"`
	Code       string   `json:"code"`
	Message    string   `json:"message"`
}

func (s *server) handlePicJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createPicJob(w, r)
	case http.MethodGet:
		s.listJobs(w, r, "pic")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handlePicJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/pic/jobs/")
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

func (s *server) createPicJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	contentType := r.Header.Get("Content-Type")

	var prompt string
	var imagePaths []string

	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		prompt = strings.TrimSpace(r.FormValue("prompt"))

		// Save uploaded reference images to user dir
		userDir := s.userWorkDir(r)
		if err := os.MkdirAll(userDir, 0755); err != nil {
			http.Error(w, "could not create user directory", http.StatusInternalServerError)
			return
		}
		files := r.MultipartForm.File["images"]
		seen := map[string]int{}
		for _, header := range files {
			name := sanitizeFileName(header.Filename)
			if name == "" {
				continue
			}
			name = uniqueName(name, seen)
			dest := filepath.Join(userDir, name)
			src, err := header.Open()
			if err != nil {
				http.Error(w, "could not open upload", http.StatusBadRequest)
				return
			}
			err = writeUploadedFile(src, dest)
			src.Close()
			if err != nil {
				http.Error(w, "could not save upload", http.StatusInternalServerError)
				return
			}
			imagePaths = append(imagePaths, dest)
		}
	} else {
		var body struct {
			Prompt string   `json:"prompt"`
			Images []string `json:"images"`
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
		Mode:      "pic",
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
	if err := s.writeAuditEvent(newAuditPicEvent(r, j, imagePaths)); err != nil {
		http.Error(w, "could not write audit log", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()

	go s.notifyPicSubmitted(j)
	go s.runPicJob(j, imagePaths)
	writeJSON(w, http.StatusAccepted, createJobResponse{ID: id})
}

func (s *server) runPicJob(j *job, imagePaths []string) {
	defer func() {
		if err := s.writeAuditEvent(newAuditPicFinishedEvent(j)); err != nil {
			s.appendLog(j, "Audit finish write failed: %v\n", err)
		}
		go s.refreshUsageLimits()
		s.notifyPicFinished(j)
	}()

	s.appendLog(j, "Starting image generation via ChatGPT daemon...\n")
	s.picSem <- struct{}{}
	defer func() { <-s.picSem }()

	start := time.Now()
	j.setStatus("running", start, nil)

	if err := s.ensureDaemon(); err != nil {
		j.fail(fmt.Errorf("daemon ensure: %w", err))
		return
	}

	picWorkDir := filepath.Join(j.WorkDir, "pic-outputs", j.ID)
	if err := os.MkdirAll(picWorkDir, 0755); err != nil {
		j.fail(fmt.Errorf("create pic workdir: %w", err))
		return
	}

	req := picAskRequest{
		Prompt:        j.Prompt,
		Timeout:       180.0,
		StableSeconds: 5.0,
		Images:        imagePaths,
		Workdir:       picWorkDir,
	}
	reqBody, _ := json.Marshal(req)

	s.appendLog(j, "Sending request to daemon on :%d...\n", daemonPort)

	resp, err := daemonPost("/ask", reqBody)
	if err != nil {
		j.fail(fmt.Errorf("daemon request: %w", err))
		return
	}

	if !resp.OK {
		if err := s.writeAuditEvent(newAuditPicDaemonErrorEvent(j, resp)); err != nil {
			s.appendLog(j, "Audit daemon error write failed: %v\n", err)
		}
		j.fail(fmt.Errorf("%s: %s", resp.Code, resp.Message))
		return
	}

	if resp.Response != "" {
		s.appendLog(j, "%s\n", resp.Response)
	}

	// Collect generated images and copy to runs/
	var images []imageInfo
	runsRelDir := filepath.ToSlash(filepath.Join("users", publicUserKey(j.UserKey), "pic-outputs", j.ID))
	runsDir := filepath.Join(s.root, "runs", filepath.FromSlash(runsRelDir))
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		s.appendLog(j, "\nCould not create public image output directory: %v\n", err)
	}

	seenNames := map[string]int{}
	for _, imgPath := range resp.Images {
		name := sanitizeFileName(filepath.Base(imgPath))
		if name == "" {
			name = "generated.png"
		}
		name = uniqueName(name, seenNames)
		runsDest := filepath.Join(runsDir, name)
		if err := copyFile(imgPath, runsDest); err != nil {
			s.appendLog(j, "\nCould not mirror generated image %s: %v\n", imgPath, err)
			continue
		}
		if copied, err := os.Stat(runsDest); err == nil {
			images = append(images, imageInfo{
				Name: name,
				URL:  "/runs/" + runsRelDir + "/" + url.PathEscape(name),
				Size: copied.Size(),
			})
		}
	}

	j.mu.Lock()
	j.Images = images
	j.Status = "succeeded"
	now := time.Now()
	j.FinishedAt = &now
	j.mu.Unlock()
}

func (s *server) ensureDaemon() error {
	alive := daemonProbe()
	if alive {
		return nil
	}
	return daemonStart()
}

func daemonProbe() bool {
	resp, err := daemonGet("/status")
	if err != nil {
		return false
	}
	return resp.OK && resp.Code == ""
}

func daemonStart() error {
	cmd := exec.Command("python", daemonScriptPath, "serve",
		"--host", daemonHost,
		"--port", fmt.Sprintf("%d", daemonPort),
		"--mode", "always_new",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait for daemon to become ready
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if daemonProbe() {
			return nil
		}
	}
	return fmt.Errorf("daemon start timeout")
}

func daemonGet(path string) (*picAskResponse, error) {
	client := &http.Client{Timeout: daemonProbeTO}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d%s", daemonHost, daemonPort, path))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result picAskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func daemonPost(path string, body []byte) (*picAskResponse, error) {
	client := &http.Client{Timeout: 0} // no timeout, may take minutes
	resp, err := client.Post(
		fmt.Sprintf("http://%s:%d%s", daemonHost, daemonPort, path),
		"application/json; charset=utf-8",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result picAskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *server) notifyPicSubmitted(j *job) {
	s.sendNotification("New image generation", picSubmittedMessage(j), 15000, j)
}

func (s *server) notifyPicFinished(j *job) {
	s.sendNotification("Image generation finished", picFinishedMessage(j), 10000, j)
}

func picSubmittedMessage(j *job) string {
	return fmt.Sprintf("User: %s\n\nPrompt:\n%s", j.Email, j.Prompt)
}

func picFinishedMessage(j *job) string {
	status := j.snapshot()
	message := fmt.Sprintf("User: %s\nJob: %s", j.Email, j.ID)
	if status.Error != "" {
		message += "\n\nFAILED: " + status.Error
	} else {
		message += "\n\nSUCCESS: " + fmt.Sprintf("%d image(s)", len(status.Images))
	}
	return message
}

func newAuditPicEvent(r *http.Request, j *job, imagePaths []string) auditEvent {
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
		CodexArgs:   append([]string{"chatgpt_agent.py", "serve", "--mode", "always_new"}, imagePaths...),
		CodexPrompt: j.Prompt,
		WorkDir:     j.WorkDir,
	}
}

func newAuditPicDaemonErrorEvent(j *job, resp *picAskResponse) auditEvent {
	return auditEvent{
		Event:            "pic_daemon_error",
		JobID:            j.ID,
		CreatedAt:        time.Now(),
		Email:            j.Email,
		Prompt:           j.Prompt,
		CodexArgs:        []string{"chatgpt_agent.py", "serve", "--mode", "always_new"},
		CodexPrompt:      j.Prompt,
		WorkDir:          j.WorkDir,
		Status:           "failed",
		DaemonCode:       resp.Code,
		DaemonMessage:    resp.Message,
		DaemonRequestID:  resp.RequestID,
		DaemonCurrentURL: resp.CurrentURL,
	}
}

func newAuditPicFinishedEvent(j *job) auditEvent {
	status := j.snapshot()
	return auditEvent{
		Event:       "pic_job_finished",
		JobID:       status.ID,
		CreatedAt:   status.CreatedAt,
		FinishedAt:  status.FinishedAt,
		Email:       j.Email,
		Prompt:      status.Prompt,
		CodexArgs:   []string{"chatgpt_agent.py", "serve", "--mode", "always_new"},
		CodexPrompt: status.Prompt,
		WorkDir:     status.WorkDir,
		Status:      status.Status,
		Error:       status.Error,
		Images:      status.Images,
	}
}
