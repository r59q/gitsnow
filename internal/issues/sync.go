package issues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const SyncTimeout = 30 * time.Second

type SyncStatus struct {
	State          string     `json:"state"`
	Remote         string     `json:"remote"`
	Message        string     `json:"message"`
	SyncedRevision string     `json:"syncedRevision,omitempty"`
	LastSuccess    *time.Time `json:"lastSuccess,omitempty"`
	AttemptStarted *time.Time `json:"attemptStarted,omitempty"`
	Conflicts      []string   `json:"conflicts,omitempty"`
}

type SyncConflict struct {
	Paths []string
}

func (e *SyncConflict) Error() string {
	return "metadata synchronization conflicts: " + strings.Join(e.Paths, ", ")
}

func gitExit(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func (r repository) syncRemote(ctx context.Context) (string, error) {
	output, err := r.git(ctx, nil, "config", "--local", "--get", "gitsnow.syncRemote")
	if err != nil && !gitExit(err, 1) {
		return "", err
	}
	configured := err == nil
	remote := strings.TrimSpace(string(output))
	if remote == "off" {
		return "", nil
	}
	if !configured {
		remote = "origin"
	}
	remotes, err := r.git(ctx, nil, "remote")
	if err != nil {
		return "", err
	}
	for _, name := range strings.Fields(string(remotes)) {
		if name == remote {
			return remote, nil
		}
	}
	if configured {
		return "", fmt.Errorf("configured synchronization remote %q does not exist; select a valid remote or off", remote)
	}
	return "", nil
}

// Configuration is local to the clone, so choosing not to share affects no teammates.
func ConfigureSync(ctx context.Context, path, remote string) error {
	repo, err := openRepository(ctx, path)
	if err != nil {
		return err
	}
	if remote != "off" {
		remotes, err := repo.git(ctx, nil, "remote")
		if err != nil {
			return err
		}
		found := false
		for _, name := range strings.Fields(string(remotes)) {
			found = found || name == remote
		}
		if !found {
			return &ValidationError{"synchronization remote must be an existing remote name or off"}
		}
	}
	_, err = repo.git(ctx, nil, "config", "--local", "gitsnow.syncRemote", remote)
	return err
}

func (r repository) bootstrapMetadata(ctx context.Context) error {
	head, err := r.head(ctx)
	if err != nil || head != "" {
		return err
	}
	remote, err := r.syncRemote(ctx)
	if err != nil || remote == "" {
		return err
	}
	fetched, err := r.refHead(ctx, "refs/remotes/"+remote+"/gitsnow/meta")
	if err != nil || fetched == "" {
		return err
	}
	if _, err := r.records(ctx, fetched); err != nil {
		return fmt.Errorf("fetched metadata is invalid: %w", err)
	}
	if err := r.ensureMetadataDetached(ctx); err != nil {
		return err
	}
	if _, err := r.git(ctx, nil, "update-ref", MetadataRef, fetched, ""); err != nil {
		current, readErr := r.head(ctx)
		if readErr == nil && current != "" {
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) storedSyncStatus(ctx context.Context) (SyncStatus, error) {
	var record string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='sync'").Scan(&record)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncStatus{}, nil
	}
	if err != nil {
		return SyncStatus{}, err
	}
	var status SyncStatus
	if err := json.Unmarshal([]byte(record), &status); err != nil {
		return SyncStatus{}, fmt.Errorf("decode synchronization status: %w", err)
	}
	return status, nil
}

func (s *Store) SyncStatus(ctx context.Context) (SyncStatus, error) {
	remote, err := s.repo.syncRemote(ctx)
	if err != nil {
		return SyncStatus{}, err
	}
	if remote == "" {
		return SyncStatus{State: "local_only", Message: "Local only: automatic sharing is disabled or origin is not configured"}, nil
	}
	status, err := s.storedSyncStatus(ctx)
	if err != nil {
		return SyncStatus{}, err
	}
	if status.Remote != remote {
		status = SyncStatus{Remote: remote}
	}
	head, err := s.repo.head(ctx)
	if err != nil {
		return SyncStatus{}, err
	}
	if status.State == "" || (status.State == "synced" && head != status.SyncedRevision) ||
		(status.State == "syncing" && status.AttemptStarted != nil && time.Since(*status.AttemptStarted) > SyncTimeout+5*time.Second) {
		status.State = "pending"
		status.Message = "Saved locally; waiting to share with " + remote
	}
	return status, nil
}

func (s *Store) writeSyncStatus(ctx context.Context, status SyncStatus) error {
	record, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO metadata(key,value) VALUES('sync',?)", string(record))
	return err
}

func (s *Store) RequestSync() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

// Network work is never done under the issue-store lock or in an HTTP mutation.
func (s *Store) RunSync(ctx context.Context, interval time.Duration, report func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.RequestSync()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.changes:
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
	}
}

func (s *Store) Sync(ctx context.Context) (status SyncStatus, err error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, SyncTimeout)
	defer cancel()
	status, err = s.SyncStatus(ctx)
	if err != nil || status.State == "local_only" {
		return status, err
	}
	now := time.Now().UTC()
	status.State, status.Message = "syncing", "Sharing metadata with "+status.Remote
	status.AttemptStarted, status.Conflicts = &now, nil
	if err := s.writeSyncStatus(ctx, status); err != nil {
		return status, err
	}
	head, syncErr := s.synchronize(ctx, status.Remote)
	status.AttemptStarted = nil
	if syncErr == nil {
		now = time.Now().UTC()
		status.State, status.Message = "synced", "Shared with "+status.Remote
		status.LastSuccess, status.SyncedRevision = &now, head
		current, readErr := s.repo.head(ctx)
		if readErr != nil {
			syncErr = readErr
		} else if current != head {
			status.State, status.Message = "pending", "More local changes are waiting to share with "+status.Remote
		}
	}
	if syncErr != nil {
		status.State = "error"
		status.Message = "Not shared: synchronization with " + status.Remote + " failed. Local changes are safe; the daemon will retry automatically. Check daemon logs or run gitsnow sync."
		var conflict *SyncConflict
		if errors.As(syncErr, &conflict) {
			status.State = "conflict"
			status.Conflicts = conflict.Paths
			status.Message = "Not shared: conflicting metadata edits require explicit reconciliation. Both versions are preserved."
		}
	}
	// A timeout must still leave a durable, visible failure for the next startup.
	persist, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelPersist()
	return status, errors.Join(syncErr, s.writeSyncStatus(persist, status))
}

func (r repository) validateSyncDestination(ctx context.Context, remote string) error {
	fetch, err := r.git(ctx, nil, "remote", "get-url", "--all", remote)
	if err != nil {
		return err
	}
	push, err := r.git(ctx, nil, "remote", "get-url", "--push", "--all", remote)
	if err != nil {
		return err
	}
	fetchURLs, pushURLs := strings.Split(strings.TrimSpace(string(fetch)), "\n"), strings.Split(strings.TrimSpace(string(push)), "\n")
	if len(fetchURLs) != 1 || len(pushURLs) != 1 || fetchURLs[0] != pushURLs[0] {
		return fmt.Errorf("metadata sync requires one identical fetch/push URL; configure a dedicated remote and select it with gitsnow sync --remote NAME")
	}
	return nil
}

func (r repository) fetchMetadata(ctx context.Context, remote string) (head string, err error) {
	output, err := r.networkGit(ctx, "ls-remote", "--exit-code", "--heads", "--", remote, MetadataRef)
	if gitExit(err, 2) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(strings.Fields(string(output))) != 2 {
		return "", fmt.Errorf("remote returned an invalid metadata ref")
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	ref := "refs/gitsnow/fetch/" + id
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, cleanupErr := r.git(cleanup, nil, "update-ref", "-d", ref)
		err = errors.Join(err, cleanupErr)
	}()
	_, err = r.networkGit(ctx, "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--",
		remote, MetadataRef+":"+ref)
	if err != nil {
		return "", err
	}
	head, err = r.refHead(ctx, ref)
	if err == nil && head == "" {
		err = fmt.Errorf("fetched metadata ref is missing")
	}
	return head, err
}

func (s *Store) synchronize(ctx context.Context, remote string) (string, error) {
	if err := s.repo.ensureMetadataDetached(ctx); err != nil {
		return "", err
	}
	if err := s.repo.validateSyncDestination(ctx, remote); err != nil {
		return "", err
	}
	incoming, err := s.repo.fetchMetadata(ctx, remote)
	if err != nil {
		return "", err
	}
	if current, err := s.repo.syncRemote(ctx); err != nil || current != remote {
		return "", errors.Join(fmt.Errorf("synchronization configuration changed; will retry with the new setting"), err)
	}
	head, err := s.integrate(ctx, incoming)
	if err != nil {
		return "", err
	}
	if head != "" && head != incoming {
		if current, err := s.repo.syncRemote(ctx); err != nil || current != remote {
			return "", errors.Join(fmt.Errorf("synchronization configuration changed before publication"), err)
		}
		if _, err := s.repo.networkGit(ctx, "-c", "remote."+remote+".mirror=false",
			"push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no",
			"--", remote, head+":"+MetadataRef); err != nil {
			return "", fmt.Errorf("publish metadata (will fetch and retry automatically): %w", err)
		}
	}
	return head, nil
}

func (s *Store) integrate(ctx context.Context, incoming string) (string, error) {
	for range 3 {
		s.mu.Lock()
		local, err := s.refresh(ctx)
		s.mu.Unlock()
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return "", err
		}
		if incoming != local {
			if _, err := s.repo.records(ctx, incoming); err != nil {
				return "", fmt.Errorf("remote metadata is invalid: %w", err)
			}
		}
		merged, err := s.repo.mergeMetadata(ctx, local, incoming)
		if err != nil {
			var conflict *SyncConflict
			if errors.As(err, &conflict) {
				_, retainErr := s.repo.git(ctx, nil, "update-ref", "refs/gitsnow/conflicts/"+incoming, incoming)
				return "", errors.Join(err, retainErr)
			}
			return "", err
		}
		if merged == local {
			return local, nil
		}
		if _, err := s.repo.records(ctx, merged); err != nil {
			return "", fmt.Errorf("merged metadata is invalid: %w", err)
		}
		s.mu.Lock()
		updateErr := s.repo.ensureMetadataDetached(ctx)
		if updateErr == nil {
			_, updateErr = s.repo.git(ctx, nil, "update-ref", MetadataRef, merged, local)
		}
		if updateErr == nil {
			_, err = s.refresh(ctx)
		}
		s.mu.Unlock()
		if updateErr == nil {
			return merged, err
		}
		current, readErr := s.repo.head(ctx)
		if readErr != nil || current == local {
			return "", errors.Join(updateErr, readErr)
		}
	}
	return "", fmt.Errorf("metadata kept changing during synchronization; will retry")
}

func (r repository) mergeMetadata(ctx context.Context, local, incoming string) (string, error) {
	if local == "" || local == incoming {
		return incoming, nil
	}
	if incoming == "" {
		return local, nil
	}
	output, err := r.git(ctx, nil, "merge-base", "--all", local, incoming)
	if err != nil && !gitExit(err, 1) {
		return "", err
	}
	bases := strings.Fields(string(output))
	if len(bases) > 1 {
		return "", &SyncConflict{Paths: []string{"multiple merge bases; reconcile metadata history explicitly"}}
	}
	base := ""
	if len(bases) == 1 {
		base = bases[0]
		if base == local {
			return incoming, nil
		}
		if base == incoming {
			return local, nil
		}
	}
	tree, conflicts, err := r.mergeTrees(ctx, base, local, incoming, "")
	if err != nil {
		return "", err
	}
	if len(conflicts) != 0 {
		return "", &SyncConflict{Paths: conflicts}
	}
	output, err = r.git(ctx, []byte("Synchronize shared metadata\n"), "commit-tree", tree, "-p", local, "-p", incoming)
	return strings.TrimSpace(string(output)), err
}

func (r repository) mergeTrees(ctx context.Context, base, local, incoming, prefix string) (string, []string, error) {
	trees := make([]map[string]treeEntry, 3)
	names := map[string]bool{}
	for i, ref := range []string{base, local, incoming} {
		entries, err := r.tree(ctx, ref)
		if err != nil {
			return "", nil, err
		}
		trees[i] = map[string]treeEntry{}
		for _, entry := range entries {
			trees[i][entry.name] = entry
			names[entry.name] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var entries []treeEntry
	var conflicts []string
	for _, name := range ordered {
		ancestor, left, right := trees[0][name], trees[1][name], trees[2][name]
		var chosen treeEntry
		switch {
		case left == right:
			chosen = left
		case left == ancestor:
			chosen = right
		case right == ancestor:
			chosen = left
		case left.kind == "tree" && right.kind == "tree" && (ancestor.kind == "" || ancestor.kind == "tree"):
			hash, paths, err := r.mergeTrees(ctx, ancestor.hash, left.hash, right.hash, prefix+name+"/")
			if err != nil {
				return "", nil, err
			}
			conflicts = append(conflicts, paths...)
			chosen = treeEntry{"040000", "tree", hash, name}
		default:
			conflicts = append(conflicts, prefix+name)
		}
		if chosen.kind != "" {
			entries = append(entries, chosen)
		}
	}
	if len(conflicts) != 0 {
		return "", conflicts, nil
	}
	hash, err := r.writeTree(ctx, entries)
	return hash, nil, err
}
