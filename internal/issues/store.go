package issues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	mu      sync.Mutex
	syncMu  sync.Mutex
	repo    repository
	db      *sql.DB
	changes chan struct{}
}

func Open(ctx context.Context, path string) (*Store, error) {
	repo, err := openRepository(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(repo.indexPath()), 0700); err != nil {
		return nil, fmt.Errorf("create local index directory: %w", err)
	}
	db, err := sql.Open("sqlite", repo.indexPath())
	if err != nil {
		return nil, fmt.Errorf("open issue index: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{repo: repo, db: db, changes: make(chan struct{}, 1)}
	if err := repo.bootstrapMetadata(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Root() string { return s.repo.root }

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;`); err != nil {
		return fmt.Errorf("configure issue index: %w", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("unsupported issue index version %d", version)
	}
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS issues (
			id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL,
			status TEXT NOT NULL, created_at TEXT NOT NULL, record TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS issues_status_created ON issues(status, created_at DESC, id);
		CREATE INDEX IF NOT EXISTS issues_created ON issues(created_at DESC, id);
		CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		PRAGMA user_version=1;`)
	if err != nil {
		return fmt.Errorf("initialize issue index: %w", err)
	}
	_, err = s.refresh(ctx)
	return err
}

// The indexed ref is kept in SQLite so separate CLI and daemon processes agree.
func (s *Store) refresh(ctx context.Context) (string, error) {
	head, err := s.repo.head(ctx)
	if err != nil {
		return "", err
	}
	var cached string
	err = s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='head'").Scan(&cached)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read indexed revision: %w", err)
	}
	if err == nil && head == cached {
		return head, nil
	}
	records, err := s.repo.records(ctx, head)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	currentHead, err := s.repo.head(ctx)
	if err != nil {
		return "", err
	}
	if currentHead != head {
		return "", ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM issues"); err != nil {
		return "", err
	}
	for _, issue := range records {
		if err := indexIssue(ctx, tx, issue); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO metadata(key,value) VALUES('head',?)", head); err != nil {
		return "", err
	}
	return head, tx.Commit()
}

func indexIssue(ctx context.Context, tx *sql.Tx, issue Issue) error {
	record, err := json.Marshal(issue)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO issues(id,title,body,status,created_at,record)
		VALUES(?,?,?,?,?,?)`, issue.ID, issue.Title, issue.Body, issue.Status,
		issue.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), string(record))
	return err
}

func (s *Store) List(ctx context.Context, status, query string) ([]Issue, error) {
	if status != "" && status != "open" && status != "closed" {
		return nil, &ValidationError{"status must be open or closed"}
	}
	if len(query) > 200 {
		return nil, &ValidationError{"search must not exceed 200 bytes"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.refresh(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM issues
		WHERE (?='' OR status=?) AND (?='' OR instr(lower(title || char(10) || body),lower(?))>0)
		ORDER BY created_at DESC, id`, status, status, query, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Issue{}
	for rows.Next() {
		var record string
		if err := rows.Scan(&record); err != nil {
			return nil, err
		}
		var issue Issue
		if err := json.Unmarshal([]byte(record), &issue); err != nil {
			return nil, fmt.Errorf("decode indexed issue: %w", err)
		}
		result = append(result, issue)
	}
	return result, rows.Err()
}

func (s *Store) get(ctx context.Context, id string) (Issue, error) {
	var record string
	err := s.db.QueryRowContext(ctx, "SELECT record FROM issues WHERE id=?", id).Scan(&record)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNotFound
	}
	if err != nil {
		return Issue{}, err
	}
	var issue Issue
	if err := json.Unmarshal([]byte(record), &issue); err != nil {
		return Issue{}, fmt.Errorf("decode indexed issue: %w", err)
	}
	return issue, nil
}

func (s *Store) Get(ctx context.Context, id string) (Issue, error) {
	if err := ValidateID(id); err != nil {
		return Issue{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.refresh(ctx); err != nil {
		return Issue{}, err
	}
	return s.get(ctx, id)
}

func (s *Store) Create(ctx context.Context, input Input) (Issue, error) {
	if input.Status == "" {
		input.Status = "open"
	}
	if err := validateInput(input); err != nil {
		return Issue{}, err
	}
	if input.ID == "" {
		var err error
		input.ID, err = NewID()
		if err != nil {
			return Issue{}, err
		}
	}
	if err := ValidateID(input.ID); err != nil {
		return Issue{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := s.refresh(ctx)
	if err != nil {
		return Issue{}, err
	}
	existing, err := s.get(ctx, input.ID)
	if err == nil {
		if existing.Title == strings.TrimSpace(input.Title) && existing.Body == input.Body && existing.Status == input.Status {
			return existing, nil
		}
		return Issue{}, ErrConflict
	}
	if !errors.Is(err, ErrNotFound) {
		return Issue{}, err
	}
	now := time.Now().UTC()
	issue := Issue{ID: input.ID, Title: strings.TrimSpace(input.Title), Body: input.Body, Status: input.Status, CreatedAt: now, UpdatedAt: now}
	return s.save(ctx, head, issue, false)
}

func (s *Store) Update(ctx context.Context, id string, input Input) (Issue, error) {
	if err := ValidateID(id); err != nil {
		return Issue{}, err
	}
	if err := validateInput(input); err != nil {
		return Issue{}, err
	}
	if input.ID != "" && input.ID != id {
		return Issue{}, &ValidationError{"request ID does not match issue ID"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := s.refresh(ctx)
	if err != nil {
		return Issue{}, err
	}
	issue, err := s.get(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	if input.Revision == "" || input.Revision != issue.Revision {
		return Issue{}, ErrConflict
	}
	title := strings.TrimSpace(input.Title)
	if title == issue.Title && input.Body == issue.Body && input.Status == issue.Status {
		return issue, nil
	}
	issue.Title, issue.Body, issue.Status = title, input.Body, input.Status
	issue.UpdatedAt = time.Now().UTC()
	return s.save(ctx, head, issue, false)
}

func (s *Store) Delete(ctx context.Context, id, revision string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := s.refresh(ctx)
	if err != nil {
		return err
	}
	issue, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if revision == "" || revision != issue.Revision {
		return ErrConflict
	}
	_, err = s.save(ctx, head, issue, true)
	return err
}

func (s *Store) save(ctx context.Context, oldHead string, issue Issue, remove bool) (Issue, error) {
	head, revision, err := s.repo.commitIssue(ctx, oldHead, issue, remove)
	if err != nil {
		return Issue{}, err
	}
	s.RequestSync()
	issue.Revision = revision
	if err := s.updateIndex(ctx, oldHead, head, issue, remove); err != nil {
		return Issue{}, fmt.Errorf("metadata was saved to Git, but updating the local index failed; reload to reconcile: %w", err)
	}
	return issue, nil
}

func (s *Store) updateIndex(ctx context.Context, oldHead, head string, issue Issue, remove bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cached string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='head'").Scan(&cached); err != nil {
		return err
	}
	// Another process may have rebuilt the index while the Git commit was made.
	if cached != oldHead {
		return nil
	}
	if remove {
		_, err = tx.ExecContext(ctx, "DELETE FROM issues WHERE id=?", issue.ID)
	} else {
		err = indexIssue(ctx, tx, issue)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE metadata SET value=? WHERE key='head'", head); err != nil {
		return err
	}
	return tx.Commit()
}
