package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type jobStore struct {
	db *sql.DB
}

func openJobStore(path string) (*jobStore, error) {
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store := &jobStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func ensureParentDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0700)
}

func (s *jobStore) init() error {
	_, err := s.db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	mode TEXT NOT NULL,
	user_key TEXT NOT NULL,
	email TEXT,
	prompt TEXT NOT NULL,
	work_dir TEXT NOT NULL,
	status TEXT NOT NULL,
	log TEXT NOT NULL DEFAULT '',
	error TEXT,
	created_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_jobs_user_mode_created ON jobs(user_key, mode, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE TABLE IF NOT EXISTS job_images (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	name TEXT NOT NULL,
	url TEXT NOT NULL,
	size INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_job_images_job ON job_images(job_id, id);
`)
	return err
}

func (s *jobStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *jobStore) upsertJob(j *job) error {
	if s == nil {
		return nil
	}
	status := j.snapshot()
	_, err := s.db.Exec(`
INSERT INTO jobs (id, mode, user_key, email, prompt, work_dir, status, log, error, created_at, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	mode = excluded.mode,
	user_key = excluded.user_key,
	email = excluded.email,
	prompt = excluded.prompt,
	work_dir = excluded.work_dir,
	status = excluded.status,
	log = excluded.log,
	error = excluded.error,
	created_at = excluded.created_at,
	started_at = excluded.started_at,
	finished_at = excluded.finished_at
`, j.ID, j.Mode, j.UserKey, j.Email, j.Prompt, j.WorkDir, status.Status, status.Log, nullableString(status.Error), formatTime(status.CreatedAt), formatTimePtr(status.StartedAt), formatTimePtr(status.FinishedAt))
	return err
}

func (s *jobStore) updateJob(status jobView) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(`
UPDATE jobs
SET status = ?, log = ?, error = ?, started_at = ?, finished_at = ?
WHERE id = ?
`, status.Status, status.Log, nullableString(status.Error), formatTimePtr(status.StartedAt), formatTimePtr(status.FinishedAt), status.ID)
	return err
}

func (s *jobStore) replaceImages(jobID string, images []imageInfo) error {
	if s == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM job_images WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	now := formatTime(time.Now())
	for _, image := range images {
		if _, err := tx.Exec(`INSERT INTO job_images (job_id, name, url, size, created_at) VALUES (?, ?, ?, ?, ?)`, jobID, image.Name, image.URL, image.Size, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *jobStore) listJobs(userKey, mode string) ([]jobView, error) {
	if s == nil {
		return []jobView{}, nil
	}
	rows, err := s.db.Query(`
SELECT id, mode, prompt, work_dir, status, log, COALESCE(error, ''), created_at, started_at, finished_at
FROM jobs
WHERE user_key = ? AND mode = ?
ORDER BY created_at DESC
`, userKey, mode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanJobRows(rows)
}

func (s *jobStore) activeJobs() ([]jobView, error) {
	if s == nil {
		return []jobView{}, nil
	}
	rows, err := s.db.Query(`
SELECT id, mode, prompt, work_dir, status, log, COALESCE(error, ''), created_at, started_at, finished_at
FROM jobs
WHERE status IN ('queued', 'running')
ORDER BY created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanJobRows(rows)
}

func (s *jobStore) markInterruptedJobs(message string) error {
	if s == nil {
		return nil
	}
	now := formatTime(time.Now())
	_, err := s.db.Exec(`
UPDATE jobs
SET status = 'failed',
	error = ?,
	finished_at = ?,
	log = log || ?
WHERE status IN ('queued', 'running')
`, message, now, "\nError: "+message+"\n")
	return err
}

func (s *jobStore) getJob(userKey, mode, id string) (jobView, bool, error) {
	if s == nil {
		return jobView{}, false, nil
	}
	rows, err := s.db.Query(`
SELECT id, mode, prompt, work_dir, status, log, COALESCE(error, ''), created_at, started_at, finished_at
FROM jobs
WHERE user_key = ? AND mode = ? AND id = ?
`, userKey, mode, id)
	if err != nil {
		return jobView{}, false, err
	}
	defer rows.Close()
	jobs, err := s.scanJobRows(rows)
	if err != nil {
		return jobView{}, false, err
	}
	if len(jobs) == 0 {
		return jobView{}, false, nil
	}
	return jobs[0], true, nil
}

func (s *jobStore) scanJobRows(rows *sql.Rows) ([]jobView, error) {
	var jobs []jobView
	for rows.Next() {
		var job jobView
		var createdAt, startedAt, finishedAt sql.NullString
		if err := rows.Scan(&job.ID, &job.Mode, &job.Prompt, &job.WorkDir, &job.Status, &job.Log, &job.Error, &createdAt, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		parsedCreated, err := parseTime(createdAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse created_at for job %s: %w", job.ID, err)
		}
		job.CreatedAt = parsedCreated
		if startedAt.Valid {
			t, err := parseTime(startedAt.String)
			if err != nil {
				return nil, err
			}
			job.StartedAt = &t
		}
		if finishedAt.Valid {
			t, err := parseTime(finishedAt.String)
			if err != nil {
				return nil, err
			}
			job.FinishedAt = &t
		}
		images, err := s.images(job.ID)
		if err != nil {
			return nil, err
		}
		job.Images = images
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *jobStore) images(jobID string) ([]imageInfo, error) {
	rows, err := s.db.Query(`SELECT name, url, size FROM job_images WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []imageInfo
	for rows.Next() {
		var image imageInfo
		if err := rows.Scan(&image.Name, &image.URL, &image.Size); err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
