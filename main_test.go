package main

import (
	"encoding/json"
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
