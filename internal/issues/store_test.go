package issues

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t testing.TB, path string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func testStore(t testing.TB) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	store, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, root
}

func TestCRUDPreservesWorkingTree(t *testing.T) {
	store, root := testStore(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "application.txt"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "application.txt")
	if err := os.WriteFile(filepath.Join(root, "application.txt"), []byte("unstaged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := gitTest(t, root, "status", "--porcelain=v1")
	branch := gitTest(t, root, "symbolic-ref", "HEAD")
	issue, err := store.Create(ctx, Input{Title: "  First issue  ", Body: "# Acceptance\n\nWorks from a phone.\n---\n"})
	if err != nil {
		t.Fatal(err)
	}
	if issue.Title != "First issue" || issue.Status != "open" || issue.Revision == "" {
		t.Fatalf("unexpected issue: %+v", issue)
	}
	data := gitTest(t, root, "show", MetadataRef+":issues/"+issue.ID+".md")
	if !strings.Contains(data, "# Acceptance") || strings.Contains(data, `"revision"`) {
		t.Fatalf("invalid persisted Markdown: %s", data)
	}
	updated, err := store.Update(ctx, issue.ID, Input{Title: issue.Title, Body: issue.Body, Status: "closed", Revision: issue.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision == issue.Revision {
		t.Fatal("revision did not change")
	}
	if _, err := store.Update(ctx, issue.ID, Input{Title: "Stale", Status: "open", Revision: issue.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected stale-write conflict, got %v", err)
	}
	if err := store.Delete(ctx, issue.ID, issue.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected stale-delete conflict, got %v", err)
	}
	if err := store.Delete(ctx, issue.ID, updated.Revision); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(ctx, "", "")
	if err != nil || len(list) != 0 || list == nil {
		t.Fatalf("expected empty array, got %v, %v", list, err)
	}
	if _, err := store.Get(ctx, issue.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing issue, got %v", err)
	}
	if after := gitTest(t, root, "status", "--porcelain=v1"); before != after {
		t.Fatalf("working tree changed: %q -> %q", before, after)
	}
	if after := gitTest(t, root, "symbolic-ref", "HEAD"); branch != after {
		t.Fatalf("checked-out branch changed: %q -> %q", branch, after)
	}
	if commits := gitTest(t, root, "rev-list", "--count", MetadataRef); commits != "3" {
		t.Fatalf("expected three metadata commits, got %s", commits)
	}
}

func TestReopenReindexAndSearch(t *testing.T) {
	store, root := testStore(t)
	ctx := context.Background()
	issue, err := store.Create(ctx, Input{Title: "Mobile review", Body: "Unique acceptance criteria"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	list, err := other.List(ctx, "open", "ACCEPTANCE")
	if err != nil || len(list) != 1 || list[0].ID != issue.ID {
		t.Fatalf("unexpected search result: %v, %v", list, err)
	}
	if _, err := other.Create(ctx, Input{Title: "Other"}); err != nil {
		t.Fatal(err)
	}
	list, err = store.List(ctx, "", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("index did not refresh across processes: %v, %v", list, err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM metadata"); err != nil {
		t.Fatal(err)
	}
	list, err = store.List(ctx, "", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("failed to rebuild index: %v, %v", list, err)
	}
}

func TestCreateIdempotencyAndValidation(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	input := Input{ID: strings.Repeat("a", 32), Title: "Retryable"}
	first, err := store.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, input)
	if err != nil || first.Revision != second.Revision {
		t.Fatalf("create retry was not idempotent: %+v, %v", second, err)
	}
	input.Title = "Different"
	if _, err := store.Create(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflicting ID, got %v", err)
	}
	for _, invalid := range []Input{{Title: " "}, {Title: "x", Status: "unknown"}, {ID: "../escape", Title: "x"}, {Title: "x", Body: strings.Repeat("a", 1024*1024+1)}} {
		if _, err := store.Create(ctx, invalid); err == nil {
			t.Fatalf("accepted invalid input: %+v", invalid.Title)
		}
	}
}

func TestConcurrentStoresDoNotLoseIssues(t *testing.T) {
	store, root := testStore(t)
	ctx := context.Background()
	other, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	results := make(chan error, 2)
	for _, current := range []*Store{store, other} {
		go func(s *Store) {
			_, err := s.Create(ctx, Input{Title: "Concurrent"})
			results <- err
		}(current)
	}
	successes := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	list, err := store.List(ctx, "", "")
	if err != nil || len(list) != successes || successes == 0 {
		t.Fatalf("lost a successful write: %d successes, %v, %v", successes, list, err)
	}
}

func TestMetadataRefCompareAndSwap(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	first, err := store.Create(ctx, Input{Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	oldHead, err := store.repo.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, Input{Title: "Second"}); err != nil {
		t.Fatal(err)
	}
	first.Title = "Stale write"
	if _, _, err := store.repo.commitIssue(ctx, oldHead, first, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ref conflict, got %v", err)
	}
}

func TestLinkedWorktreesShareMetadata(t *testing.T) {
	store, root := testStore(t)
	ctx := context.Background()
	gitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@localhost", "commit", "--allow-empty", "-qm", "Application")
	applicationHead := gitTest(t, root, "rev-parse", "HEAD")
	linked := filepath.Join(t.TempDir(), "linked")
	gitTest(t, root, "worktree", "add", "-q", "-b", "linked", linked)
	other, err := Open(ctx, linked)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if store.repo.indexPath() != other.repo.indexPath() {
		t.Fatal("linked worktrees did not share the read model")
	}
	issue, err := other.Create(ctx, Input{Title: "Shared issue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, issue.ID); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, root, "rev-parse", "HEAD"); got != applicationHead {
		t.Fatal("application HEAD changed")
	}
	if got := gitTest(t, linked, "status", "--porcelain"); got != "" {
		t.Fatalf("linked worktree changed: %s", got)
	}
}

func TestFreshCloneAdoptsFetchedMetadata(t *testing.T) {
	store, root := testStore(t)
	ctx := context.Background()
	gitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@localhost", "commit", "--allow-empty", "-qm", "Application")
	issue, err := store.Create(ctx, Input{Title: "Shared with another clone"})
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitTest(t, root, "clone", "-q", root, clone)
	other, err := Open(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	list, err := other.List(ctx, "", "")
	if err != nil || len(list) != 1 || list[0].ID != issue.ID {
		t.Fatalf("fresh clone could not rebuild shared metadata: %v, %v", list, err)
	}
}

func TestSHA256Repository(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q", "--object-format=sha256")
	store, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	issue, err := store.Create(context.Background(), Input{Title: "SHA-256"})
	if err != nil || len(issue.Revision) != 64 {
		t.Fatalf("SHA-256 issue creation failed: %+v, %v", issue, err)
	}
}

func TestMalformedMetadataIsNotSilentlySkipped(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	issue, err := store.Create(ctx, Input{Title: "Valid"})
	if err != nil {
		t.Fatal(err)
	}
	issue.Status = "invalid"
	head, err := store.repo.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.repo.commitIssue(ctx, head, issue, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, "", ""); err == nil {
		t.Fatal("accepted malformed Git metadata")
	}
}

func BenchmarkCachedList1000(b *testing.B) {
	store, root := testStore(b)
	ctx := context.Background()
	now := time.Now().UTC()
	entries := make([]treeEntry, 0, 1000)
	for i := range 1000 {
		issue := Issue{ID: fmt.Sprintf("%032x", i), Title: fmt.Sprintf("Issue %d", i), Status: "open", CreatedAt: now, UpdatedAt: now}
		data, err := encodeIssue(issue)
		if err != nil {
			b.Fatal(err)
		}
		output, err := store.repo.git(ctx, data, "hash-object", "-w", "--stdin")
		if err != nil {
			b.Fatal(err)
		}
		entries = append(entries, treeEntry{"100644", "blob", strings.TrimSpace(string(output)), issue.ID + ".md"})
	}
	tree, err := store.repo.writeTree(ctx, entries)
	if err != nil {
		b.Fatal(err)
	}
	tree, err = store.repo.writeTree(ctx, []treeEntry{{"040000", "tree", tree, "issues"}})
	if err != nil {
		b.Fatal(err)
	}
	head, err := store.repo.git(ctx, []byte("Benchmark fixture\n"), "commit-tree", tree)
	if err != nil {
		b.Fatal(err)
	}
	gitTest(b, root, "update-ref", MetadataRef, strings.TrimSpace(string(head)))
	if _, err := store.List(ctx, "", ""); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		list, err := store.List(ctx, "", "")
		if err != nil || len(list) != 1000 {
			b.Fatalf("list: %d records, %v", len(list), err)
		}
	}
}
