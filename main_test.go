package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
