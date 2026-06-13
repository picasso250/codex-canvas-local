package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewAuditEventUsesAccessHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/jobs", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Cf-Access-Authenticated-User-Email", "user@example.com")
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	req.Header.Set("Cf-Ray", "abc123-SJC")
	req.Header.Set("User-Agent", "audit-test")

	j := &job{
		ID:        "job123",
		Prompt:    "draw a cabin",
		WorkDir:   filepath.Join("tmp", "sessions", "job123"),
		CreatedAt: time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC),
		ReferenceImages: []referenceInfo{{
			Name: "ref.png",
			Path: `C:\tmp\ref.png`,
			Size: 42,
		}},
	}

	event := newAuditEvent(req, j)
	if event.Email != "user@example.com" {
		t.Fatalf("email = %q", event.Email)
	}
	if event.IP != "203.0.113.10" {
		t.Fatalf("ip = %q", event.IP)
	}
	if event.CFRay != "abc123-SJC" {
		t.Fatalf("cf ray = %q", event.CFRay)
	}
	if event.CodexPrompt == "" || event.CodexArgs == nil {
		t.Fatalf("codex execution details were not recorded")
	}
	if len(event.ReferenceImages) != 1 {
		t.Fatalf("reference images = %d", len(event.ReferenceImages))
	}
}

func TestWriteAuditEventAppendsJSONLine(t *testing.T) {
	dir := t.TempDir()
	s := &server{auditPath: filepath.Join(dir, "audit.jsonl")}
	want := auditEvent{
		Event:     "job_created",
		JobID:     "job123",
		CreatedAt: time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC),
		Email:     "user@example.com",
		Prompt:    "draw a cabin",
	}

	if err := s.writeAuditEvent(want); err != nil {
		t.Fatal(err)
	}

	var got auditEvent
	b := mustReadFile(t, s.auditPath)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.JobID != want.JobID || got.Email != want.Email || got.Prompt != want.Prompt {
		t.Fatalf("unexpected audit event: %#v", got)
	}
}

func TestHandleAuditAllowsLocalUser(t *testing.T) {
	dir := t.TempDir()
	s := &server{auditPath: filepath.Join(dir, "audit.jsonl")}
	if err := s.writeAuditEvent(auditEvent{
		Event:     "job_created",
		JobID:     "job123",
		CreatedAt: time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC),
		Prompt:    "draw a cabin",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/audit", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var got auditResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 1 || got.Lines[0].Event == nil || got.Lines[0].Event.JobID != "job123" {
		t.Fatalf("audit response = %#v", got)
	}
	if len(got.Emails) != 1 || got.Emails[0] != "local" {
		t.Fatalf("emails = %#v", got.Emails)
	}
}

func TestHandleAuditReturnsEmailSet(t *testing.T) {
	dir := t.TempDir()
	s := &server{auditPath: filepath.Join(dir, "audit.jsonl")}
	for _, event := range []auditEvent{
		{Event: "job_created", JobID: "job-user", Email: "user@example.com", CreatedAt: time.Now()},
		{Event: "job_created", JobID: "job-other", Email: "other@example.com", CreatedAt: time.Now()},
		{Event: "job_created", JobID: "job-local", CreatedAt: time.Now()},
	} {
		if err := s.writeAuditEvent(event); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/api/audit", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var got auditResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantEmails := []string{"local", "other@example.com", "user@example.com"}
	if strings.Join(got.Emails, ",") != strings.Join(wantEmails, ",") {
		t.Fatalf("emails = %#v", got.Emails)
	}
	if len(got.Lines) != 3 {
		t.Fatalf("lines = %#v", got.Lines)
	}
}

func TestHandleAuditRejectsAccessUser(t *testing.T) {
	s := &server{auditPath: filepath.Join(t.TempDir(), "audit.jsonl")}
	req := httptest.NewRequest("GET", "/api/audit", nil)
	req.Header.Set("Cf-Access-Authenticated-User-Email", "user@example.com")
	rr := httptest.NewRecorder()

	s.handleAudit(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestHandleAuditPageRejectsAccessUser(t *testing.T) {
	s := &server{}
	handler := s.handleAuditPage(os.DirFS("static"), "test")
	req := httptest.NewRequest("GET", "/audit.html", nil)
	req.Header.Set("Cf-Access-Authenticated-User-Email", "user@example.com")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestUserWorkDirKey(t *testing.T) {
	if got := userWorkDirKey(""); got != "local" {
		t.Fatalf("empty user key = %q", got)
	}

	first := userWorkDirKey("User@example.com")
	second := userWorkDirKey(" user@example.com ")
	if first != second {
		t.Fatalf("user key should be stable: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "user-") {
		t.Fatalf("user key should keep readable prefix: %q", first)
	}
}

func TestCollectImagesDeduplicatesByHash(t *testing.T) {
	isolateImageSearchEnv(t)
	root := t.TempDir()
	s := &server{root: root}
	workDir := filepath.Join(root, "tmp", "users", "user")
	generatedDir := filepath.Join(root, "tmp", "imagegen")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(generatedDir, 0755); err != nil {
		t.Fatal(err)
	}

	friendly := filepath.Join(workDir, "edited-detail.png")
	defaultName := filepath.Join(generatedDir, "ig_123.png")
	other := filepath.Join(generatedDir, "ig_456.png")
	writeTestFile(t, friendly, []byte("same image"))
	writeTestFile(t, defaultName, []byte("same image"))
	writeTestFile(t, other, []byte("different image"))

	j := &job{ID: "job123", WorkDir: workDir}
	images := s.collectImages(j, map[string]time.Time{}, time.Now().Add(-time.Minute))
	if len(images) != 2 {
		t.Fatalf("images = %#v", images)
	}
	if images[0].Name != "edited-detail.png" && images[1].Name != "edited-detail.png" {
		t.Fatalf("dedupe should keep friendly name: %#v", images)
	}
}

func TestCollectImagesIncludesUpdatedPersistentFile(t *testing.T) {
	isolateImageSearchEnv(t)
	root := t.TempDir()
	s := &server{root: root}
	workDir := filepath.Join(root, "tmp", "users", "user")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(workDir, "output.png")
	writeTestFile(t, path, []byte("updated image"))
	before := map[string]time.Time{
		path: time.Now().Add(-time.Minute),
	}

	j := &job{ID: "job123", WorkDir: workDir}
	images := s.collectImages(j, before, time.Now().Add(-time.Second))
	if len(images) != 1 || images[0].Name != "output.png" {
		t.Fatalf("images = %#v", images)
	}
}

func TestListJobsFiltersByUser(t *testing.T) {
	s := &server{jobs: map[string]*job{}}
	userReq := httptest.NewRequest("GET", "/api/jobs", nil)
	userReq.Header.Set("Cf-Access-Authenticated-User-Email", "user@example.com")
	otherReq := httptest.NewRequest("GET", "/api/jobs", nil)
	otherReq.Header.Set("Cf-Access-Authenticated-User-Email", "other@example.com")

	s.jobs["user-job"] = &job{ID: "user-job", Mode: "work", UserKey: s.userKey(userReq), Prompt: "mine", Status: "succeeded", CreatedAt: time.Now()}
	s.jobs["other-job"] = &job{ID: "other-job", Mode: "work", UserKey: s.userKey(otherReq), Prompt: "theirs", Status: "succeeded", CreatedAt: time.Now()}

	rr := httptest.NewRecorder()
	s.listJobs(rr, userReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []jobView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "user-job" {
		t.Fatalf("jobs = %#v", got)
	}
}

func TestBuildCodexPromptWorkModePassesThrough(t *testing.T) {
	j := &job{Mode: "work", Prompt: "inspect files"}
	if got := buildCodexPrompt(j); got != "inspect files" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestSafeUserPathRejectsTraversalAndRootDelete(t *testing.T) {
	root := t.TempDir()
	if _, _, err := safeUserPath(root, `..\other`); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, _, err := safeUserPath(root, filepath.Join(root, "file.txt")); err == nil {
		t.Fatal("expected absolute path error")
	}

	s := &server{root: t.TempDir()}
	req := httptest.NewRequest("DELETE", "/api/work/files?path=.", nil)
	rr := httptest.NewRecorder()
	s.deleteWorkFile(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestWorkFileUploadAndList(t *testing.T) {
	root := t.TempDir()
	s := &server{root: root}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/work/files/upload?path=.", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	s.handleWorkFileUpload(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d body=%s", rr.Code, rr.Body.String())
	}

	listReq := httptest.NewRequest("GET", "/api/work/files?path=.", nil)
	listRR := httptest.NewRecorder()
	s.listWorkFiles(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list status = %d", listRR.Code)
	}
	var got struct {
		Entries []fileEntry `json:"entries"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Name != "note.txt" {
		t.Fatalf("entries = %#v", got.Entries)
	}
}

func TestStaticHandlerServesWorkForCodexHostRoot(t *testing.T) {
	root := os.DirFS("static")
	handler := staticHandler(root, "test")
	req := httptest.NewRequest("GET", "http://codex.io99.xyz/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Codex Work") {
		t.Fatalf("expected work page, got %q", rr.Body.String())
	}
}

func TestIsWorkHost(t *testing.T) {
	if !isWorkHost("codex.io99.xyz") {
		t.Fatal("expected codex.io99.xyz to be work host")
	}
	if !isWorkHost("codex.io99.xyz:443") {
		t.Fatal("expected codex.io99.xyz:443 to be work host")
	}
	if isWorkHost("pic.io99.xyz") {
		t.Fatal("pic host should not be work host")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func isolateImageSearchEnv(t *testing.T) {
	t.Helper()
	emptyHome := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("USERPROFILE", emptyHome)
	t.Setenv("HOME", emptyHome)
}
