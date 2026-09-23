package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonPicPromptAddsGenerationPrefix(t *testing.T) {
	if got := daemonPicPrompt("draw a cabin"); got != "生成图片 draw a cabin" {
		t.Fatalf("daemonPicPrompt() = %q", got)
	}
}

func TestNewAuditEventUsesAccessHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/pic/jobs", nil)
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
	}

	event := newAuditPicEvent(req, j, []string{"a.png"})
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

func TestListJobsFiltersByUser(t *testing.T) {
	s := &server{jobs: map[string]*job{}}
	userReq := httptest.NewRequest("GET", "/api/work/jobs", nil)
	userReq.Header.Set("Cf-Access-Authenticated-User-Email", "user@example.com")
	otherReq := httptest.NewRequest("GET", "/api/work/jobs", nil)
	otherReq.Header.Set("Cf-Access-Authenticated-User-Email", "other@example.com")

	s.jobs["user-job"] = &job{ID: "user-job", Mode: "work", UserKey: s.userKey(userReq), Prompt: "mine", Status: "succeeded", CreatedAt: time.Now()}
	s.jobs["other-job"] = &job{ID: "other-job", Mode: "work", UserKey: s.userKey(otherReq), Prompt: "theirs", Status: "succeeded", CreatedAt: time.Now()}

	rr := httptest.NewRecorder()
	s.listJobs(rr, userReq, "work")
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

func TestHandleActiveJobsReturnsAllUsersAndModes(t *testing.T) {
	s := &server{jobs: map[string]*job{}}
	now := time.Now()
	s.jobs["work-running"] = &job{ID: "work-running", Mode: "work", UserKey: "user-a", Prompt: "work", Status: "running", CreatedAt: now}
	s.jobs["pic-queued"] = &job{ID: "pic-queued", Mode: "pic", UserKey: "user-b", Prompt: "pic", Status: "queued", CreatedAt: now.Add(time.Second)}
	s.jobs["done"] = &job{ID: "done", Mode: "pic", UserKey: "user-c", Prompt: "done", Status: "succeeded", CreatedAt: now}

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	rr := httptest.NewRecorder()
	s.handleActiveJobs(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	var got []jobView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("jobs = %#v", got)
	}
	ids := map[string]bool{}
	for _, job := range got {
		ids[job.ID] = true
	}
	if !ids["work-running"] || !ids["pic-queued"] || ids["done"] {
		t.Fatalf("jobs = %#v", got)
	}
}

func TestJobStorePersistsJobsAndImages(t *testing.T) {
	store, err := openJobStore(filepath.Join(t.TempDir(), "jobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	now := time.Now().UTC()
	j := &job{
		ID:        "pic-job",
		Mode:      "pic",
		UserKey:   "user-a",
		Email:     "user@example.com",
		Prompt:    "draw",
		WorkDir:   "tmp/users/user-a",
		Status:    "running",
		CreatedAt: now,
	}
	if err := store.upsertJob(j); err != nil {
		t.Fatal(err)
	}

	j.Status = "succeeded"
	finished := now.Add(time.Minute)
	j.FinishedAt = &finished
	if err := store.updateJob(j.snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := store.replaceImages(j.ID, []imageInfo{{Name: "generated.png", URL: "/runs/generated.png", Size: 123}}); err != nil {
		t.Fatal(err)
	}

	jobs, err := store.listJobs("user-a", "pic")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != j.ID || jobs[0].Status != "succeeded" || len(jobs[0].Images) != 1 {
		t.Fatalf("jobs = %#v", jobs)
	}

	active, err := store.activeJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active jobs = %#v", active)
	}
}

func TestSubmittedNotificationIncludesEmailAndFullPrompt(t *testing.T) {
	prompt := "line one\nline two\n完整提示词"
	j := &job{Email: "user@example.com", Prompt: prompt}

	got := picSubmittedMessage(j)
	if !strings.Contains(got, "User: user@example.com") {
		t.Fatalf("notification missing email: %q", got)
	}
	if !strings.Contains(got, prompt) {
		t.Fatalf("notification missing full prompt: %q", got)
	}
}

func TestFinishedNotificationIncludesFailureText(t *testing.T) {
	j := &job{ID: "job123", Email: "user@example.com", Error: "chatgpt daemon exited with error: exit status 7"}

	got := picFinishedMessage(j)
	for _, want := range []string{"User: user@example.com", "Job: job123", "FAILED:", j.Error} {
		if !strings.Contains(got, want) {
			t.Fatalf("notification missing %q: %q", want, got)
		}
	}
}

func TestWinNotifyURL(t *testing.T) {
	got, err := winNotifyURL("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:25378/notify" {
		t.Fatalf("default URL = %q", got)
	}

	got, err = winNotifyURL("25379")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:25379/notify" {
		t.Fatalf("custom URL = %q", got)
	}

	for _, port := range []string{"80", "70000", "abc"} {
		if _, err := winNotifyURL(port); err == nil {
			t.Fatalf("expected invalid port %q to fail", port)
		}
	}
}

func TestSafeUserPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if _, _, err := safeUserPath(root, `..\other`); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, _, err := safeUserPath(root, filepath.Join(root, "file.txt")); err == nil {
		t.Fatal("expected absolute path error")
	}
}

func TestStaticHandlerServesPicPageAtRoot(t *testing.T) {
	root := os.DirFS("static")
	handler := staticHandler(root, "test")

	for _, target := range []string{"http://pic.io99.xyz/", "http://127.0.0.1:8765/", "http://127.0.0.1:8765/pic/"} {
		req := httptest.NewRequest("GET", target, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d", target, rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, `src="/pic.js?v=test"`) {
			t.Fatalf("%s expected pic page with versioned script, got %q", target, body)
		}
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
