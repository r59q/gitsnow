package issues

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sharedStores(t *testing.T) (*Store, *Store, string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitTest(t, filepath.Dir(remote), "init", "--bare", "-q", remote)
	left, leftRoot := testStore(t)
	right, rightRoot := testStore(t)
	gitTest(t, leftRoot, "remote", "add", "origin", remote)
	gitTest(t, rightRoot, "remote", "add", "origin", remote)
	return left, right, remote
}

func syncStore(t *testing.T, store *Store) SyncStatus {
	t.Helper()
	status, err := store.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "synced" {
		t.Fatalf("expected synchronized metadata, got %+v", status)
	}
	return status
}

func bareGit(t *testing.T, remote string, args ...string) string {
	t.Helper()
	return gitTest(t, filepath.Dir(remote), append([]string{"--git-dir=" + remote}, args...)...)
}

func createIssue(t *testing.T, store *Store, title string) Issue {
	t.Helper()
	issue, err := store.Create(context.Background(), Input{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func TestSyncPublishesOnlyMetadataAndImportsChanges(t *testing.T) {
	left, right, remote := sharedStores(t)
	ctx := context.Background()
	gitTest(t, left.Root(), "-c", "user.name=Test", "-c", "user.email=test@localhost", "commit", "--allow-empty", "-qm", "Private application commit")
	gitTest(t, left.Root(), "config", "push.followTags", "true")
	gitTest(t, left.Root(), "config", "remote.origin.mirror", "true")
	gitTest(t, left.Root(), "-c", "user.name=Test", "-c", "user.email=test@localhost", "tag", "-a", "private-tag", "-m", "Private", "HEAD")
	file := filepath.Join(left.Root(), "application.txt")
	if err := os.WriteFile(file, []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, left.Root(), "add", "application.txt")
	if err := os.WriteFile(file, []byte("unstaged"), 0600); err != nil {
		t.Fatal(err)
	}
	before := gitTest(t, left.Root(), "status", "--porcelain")
	applicationHead := gitTest(t, left.Root(), "rev-parse", "HEAD")
	issue := createIssue(t, left, "Visible to coworkers")
	gitTest(t, left.Root(), "-c", "user.name=Test", "-c", "user.email=test@localhost", "tag", "-a", "private-metadata-tag", "-m", "Private", MetadataRef)
	if status, err := left.SyncStatus(ctx); err != nil || status.State != "pending" {
		t.Fatalf("unpublished data claimed shared: %+v %v", status, err)
	}
	syncStore(t, left)
	refs := bareGit(t, remote, "for-each-ref", "--format=%(refname)")
	if refs != MetadataRef {
		t.Fatalf("published other refs: %s", refs)
	}
	syncStore(t, right)
	coworker, err := right.Get(ctx, issue.ID)
	if err != nil || coworker.Revision != issue.Revision {
		t.Fatalf("coworker did not receive the issue: %+v %v", coworker, err)
	}
	updated, err := right.Update(ctx, issue.ID, Input{Title: issue.Title, Body: "Coworker's acceptance criteria", Status: "closed", Revision: issue.Revision})
	if err != nil {
		t.Fatal(err)
	}
	syncStore(t, right)
	syncStore(t, left)
	got, err := left.Get(ctx, issue.ID)
	if err != nil || got != updated {
		t.Fatalf("did not import coworker update: %+v %v", got, err)
	}
	if err := left.Delete(ctx, issue.ID, updated.Revision); err != nil {
		t.Fatal(err)
	}
	syncStore(t, left)
	syncStore(t, right)
	if _, err := right.Get(ctx, issue.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("coworker did not receive deletion: %v", err)
	}
	if gitTest(t, left.Root(), "status", "--porcelain") != before || gitTest(t, left.Root(), "rev-parse", "HEAD") != applicationHead {
		t.Fatal("synchronization changed the application worktree or HEAD")
	}
	if refs := gitTest(t, left.Root(), "for-each-ref", "--format=%(refname)", "refs/gitsnow/fetch/"); refs != "" {
		t.Fatalf("temporary fetch refs were not cleaned: %s", refs)
	}
}

func TestSyncMergesIndependentInitialHistories(t *testing.T) {
	left, right, _ := sharedStores(t)
	first := createIssue(t, left, "Left created offline")
	second := createIssue(t, right, "Right created offline")
	syncStore(t, left)
	syncStore(t, right)
	syncStore(t, left)
	for _, store := range []*Store{left, right} {
		list, err := store.List(context.Background(), "", "")
		if err != nil || len(list) != 2 {
			t.Fatalf("lost an independent issue: %v %v", list, err)
		}
		for _, issue := range []Issue{first, second} {
			if got, err := store.Get(context.Background(), issue.ID); err != nil || got.Revision != issue.Revision {
				t.Fatalf("merge changed a record unnecessarily: %+v %v", got, err)
			}
		}
	}
}

func TestSyncMergesIndependentEditsWithSharedBase(t *testing.T) {
	left, right, _ := sharedStores(t)
	first := createIssue(t, left, "First")
	second := createIssue(t, left, "Second")
	syncStore(t, left)
	syncStore(t, right)
	for i, store := range []*Store{left, right} {
		issue := []Issue{first, second}[i]
		if _, err := store.Update(context.Background(), issue.ID, Input{Title: issue.Title, Body: "Independent edit", Status: "closed", Revision: issue.Revision}); err != nil {
			t.Fatal(err)
		}
	}
	syncStore(t, left)
	syncStore(t, right)
	syncStore(t, left)
	list, err := left.List(context.Background(), "closed", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("lost independent edits: %v %v", list, err)
	}
}

func TestSyncConflictsPreserveBothVersions(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		t.Run(map[bool]string{false: "edit-edit", true: "edit-delete"}[deletion], func(t *testing.T) {
			left, right, remote := sharedStores(t)
			ctx := context.Background()
			issue := createIssue(t, left, "Original")
			syncStore(t, left)
			syncStore(t, right)
			if _, err := left.Update(ctx, issue.ID, Input{Title: "Left edit", Status: "open", Revision: issue.Revision}); err != nil {
				t.Fatal(err)
			}
			if deletion {
				if err := right.Delete(ctx, issue.ID, issue.Revision); err != nil {
					t.Fatal(err)
				}
			} else if _, err := right.Update(ctx, issue.ID, Input{Title: "Right edit", Status: "open", Revision: issue.Revision}); err != nil {
				t.Fatal(err)
			}
			syncStore(t, left)
			localBefore := gitTest(t, right.Root(), "rev-parse", MetadataRef)
			remoteBefore := bareGit(t, remote, "rev-parse", MetadataRef)
			status, err := right.Sync(ctx)
			var conflict *SyncConflict
			if !errors.As(err, &conflict) || status.State != "conflict" || len(status.Conflicts) != 1 || status.Conflicts[0] != "issues/"+issue.ID+".md" {
				t.Fatalf("expected explicit issue conflict, got %+v %v", status, err)
			}
			if gitTest(t, right.Root(), "rev-parse", MetadataRef) != localBefore || bareGit(t, remote, "rev-parse", MetadataRef) != remoteBefore {
				t.Fatal("conflict overwrote a version")
			}
			if retained := gitTest(t, right.Root(), "rev-parse", "refs/gitsnow/conflicts/"+remoteBefore); retained != remoteBefore {
				t.Fatal("incoming conflict snapshot was not retained locally")
			}
			other, err := Open(ctx, right.Root())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if status, err := other.SyncStatus(ctx); err != nil || status.State != "conflict" {
				t.Fatalf("conflict was not durable: %+v %v", status, err)
			}
		})
	}
}

func TestSyncNetworkFailureRetryAndRestart(t *testing.T) {
	left, right, remote := sharedStores(t)
	ctx := context.Background()
	issue := createIssue(t, left, "Keep this offline")
	missing := filepath.Join(t.TempDir(), "missing.git")
	gitTest(t, left.Root(), "remote", "set-url", "origin", missing)
	status, err := left.Sync(ctx)
	if err == nil || status.State != "error" {
		t.Fatalf("network failure was hidden: %+v %v", status, err)
	}
	if _, err := left.Get(ctx, issue.ID); err != nil {
		t.Fatal("network failure lost a local issue:", err)
	}
	gitTest(t, left.Root(), "remote", "set-url", "origin", remote)
	restarted, err := Open(ctx, left.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	syncStore(t, restarted)
	syncStore(t, right)
	if _, err := right.Get(ctx, issue.ID); err != nil {
		t.Fatal("restart did not publish pending local data:", err)
	}
}

func TestSyncPushFailureIsNotClaimedShared(t *testing.T) {
	left, _, remote := sharedStores(t)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	createIssue(t, left, "Rejected by remote")
	status, err := left.Sync(context.Background())
	if err == nil || status.State != "error" || status.LastSuccess != nil {
		t.Fatalf("rejected push claimed success: %+v %v", status, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	syncStore(t, left)
}

func TestSyncConfiguration(t *testing.T) {
	left, _, remote := sharedStores(t)
	ctx := context.Background()
	if err := ConfigureSync(ctx, left.Root(), "off"); err != nil {
		t.Fatal(err)
	}
	createIssue(t, left, "Private")
	status, err := left.Sync(ctx)
	if err != nil || status.State != "local_only" {
		t.Fatalf("opt-out failed: %+v %v", status, err)
	}
	if refs := bareGit(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("opt-out published data: %s", refs)
	}
	gitTest(t, left.Root(), "remote", "rename", "origin", "team")
	if err := ConfigureSync(ctx, left.Root(), "invalid"); err == nil {
		t.Fatal("accepted an unknown remote")
	}
	if err := ConfigureSync(ctx, left.Root(), "team"); err != nil {
		t.Fatal(err)
	}
	if status := syncStore(t, left); status.Remote != "team" {
		t.Fatalf("wrong destination: %+v", status)
	}
}

func TestSyncRejectsSplitPushDestinations(t *testing.T) {
	left, _, _ := sharedStores(t)
	ctx := context.Background()
	gitTest(t, left.Root(), "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "different.git"))
	createIssue(t, left, "Do not send to a different destination")
	if status, err := left.Sync(ctx); err == nil || status.State != "error" || !strings.Contains(err.Error(), "fetch/push URL") {
		t.Fatalf("accepted split destinations: %+v %v", status, err)
	}
}

func TestRunSyncPublishesAfterSavesWithoutBlockingReads(t *testing.T) {
	left, right, _ := sharedStores(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	errors := make(chan error, 10)
	go func() {
		defer close(done)
		left.RunSync(ctx, 20*time.Millisecond, func(err error) { errors <- err })
	}()
	defer func() {
		cancel()
		<-done
	}()
	issue := createIssue(t, left, "No manual push")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("automatic sharing never completed")
		}
		status, err := left.SyncStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "synced" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	syncStore(t, right)
	if _, err := right.Get(context.Background(), issue.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSyncRefRaceDoesNotLoseALocalWrite(t *testing.T) {
	left, right, _ := sharedStores(t)
	ctx := context.Background()
	createIssue(t, left, "First")
	syncStore(t, left)
	syncStore(t, right)
	incomingIssue := createIssue(t, right, "Incoming")
	syncStore(t, right)
	incoming, err := right.repo.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.repo.networkGit(ctx, "fetch", "--no-write-fetch-head", "--", "origin", MetadataRef); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := left.integrate(ctx, incoming)
		result <- err
	}()
	localIssue, createErr := left.Create(ctx, Input{Title: "Created while integrating"})
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if createErr != nil {
		if !errors.Is(createErr, ErrConflict) {
			t.Fatal(createErr)
		}
		localIssue = createIssue(t, left, "Created while integrating")
	}
	for _, id := range []string{incomingIssue.ID, localIssue.ID} {
		if _, err := left.Get(ctx, id); err != nil {
			t.Fatalf("lost a successful local or incoming write: %v", err)
		}
	}
}

func TestSlowPublicationDoesNotBlockLocalCRUD(t *testing.T) {
	left, _, remote := sharedStores(t)
	marker := filepath.Join(t.TempDir(), "publishing")
	hook := fmt.Sprintf("#!/bin/sh\n: > %q\nsleep 1\n", marker)
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0700); err != nil {
		t.Fatal(err)
	}
	createIssue(t, left, "First")
	done := make(chan error, 1)
	go func() {
		_, err := left.Sync(context.Background())
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("push hook never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	second := createIssue(t, left, "Saved while publishing")
	list, err := left.List(context.Background(), "", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("local CRUD failed during push: %v %v", list, err)
	}
	if duration := time.Since(start); duration > 500*time.Millisecond {
		t.Fatalf("network operation blocked local CRUD: %s", duration)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status, err := left.SyncStatus(context.Background())
	if err != nil || status.State != "pending" {
		t.Fatalf("unpublished concurrent write claimed shared: %+v %v", status, err)
	}
	syncStore(t, left)
	if data := bareGit(t, remote, "show", MetadataRef+":issues/"+second.ID+".md"); !strings.Contains(data, second.Title) {
		t.Fatal("follow-up sync lost a write made during publication")
	}
}

func TestSyncRejectsInvalidIncomingRecords(t *testing.T) {
	left, right, _ := sharedStores(t)
	ctx := context.Background()
	issue := createIssue(t, left, "Original")
	syncStore(t, left)
	syncStore(t, right)
	before, err := right.repo.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	issue.Status = "invalid"
	head, err := left.repo.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	invalid, _, err := left.repo.commitIssue(ctx, head, issue, false)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, left.Root(), "push", "-q", "origin", invalid+":"+MetadataRef)
	status, err := right.Sync(ctx)
	if err == nil || status.State != "error" || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid remote metadata was accepted: %+v %v", status, err)
	}
	if current, err := right.repo.head(ctx); err != nil || current != before {
		t.Fatalf("invalid metadata replaced local data: %s %v", current, err)
	}
}

func TestSyncConcurrentPublishersConverge(t *testing.T) {
	left, right, _ := sharedStores(t)
	createIssue(t, left, "Left")
	createIssue(t, right, "Right")
	done := make(chan error, 2)
	for _, store := range []*Store{left, right} {
		go func(s *Store) {
			_, err := s.Sync(context.Background())
			done <- err
		}(store)
	}
	// A losing push is expected to be retried, never forced through.
	for range 2 {
		<-done
	}
	syncStore(t, left)
	syncStore(t, right)
	syncStore(t, left)
	for _, store := range []*Store{left, right} {
		list, err := store.List(context.Background(), "", "")
		if err != nil || len(list) != 2 {
			t.Fatalf("concurrent publication lost issues: %v %v", list, err)
		}
	}
}

func TestSyncRefusesCheckedOutMetadata(t *testing.T) {
	left, _, remote := sharedStores(t)
	createIssue(t, left, "Original")
	worktree := filepath.Join(t.TempDir(), "metadata")
	gitTest(t, left.Root(), "worktree", "add", "-q", worktree, "gitsnow/meta")
	status, err := left.Sync(context.Background())
	if err == nil || status.State != "error" || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("sync mutated a checked-out metadata branch: %+v %v", status, err)
	}
	if _, err := left.Create(context.Background(), Input{Title: "Unsafe mutation"}); err == nil {
		t.Fatal("managed writes changed a checked-out metadata branch")
	}
	if refs := bareGit(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("published data while metadata was checked out")
	}
	gitTest(t, left.Root(), "worktree", "remove", worktree)
	syncStore(t, left)
}

func TestSyncRefusesInvalidLocalMetadata(t *testing.T) {
	left, _, remote := sharedStores(t)
	ctx := context.Background()
	issue := createIssue(t, left, "Original")
	issue.Status = "invalid"
	head, err := left.repo.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := left.repo.commitIssue(ctx, head, issue, false); err != nil {
		t.Fatal(err)
	}
	if status, err := left.Sync(ctx); err == nil || status.State != "error" {
		t.Fatalf("invalid local records were published: %+v %v", status, err)
	}
	if refs := bareGit(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("invalid local metadata reached the remote")
	}
}
