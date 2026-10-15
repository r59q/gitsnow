package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const MetadataRef = "refs/heads/gitsnow/meta"

type repository struct {
	root   string
	gitDir string
}

type treeEntry struct {
	mode string
	kind string
	hash string
	name string
}

type issueHeader struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func openRepository(ctx context.Context, path string) (repository, error) {
	repo := repository{root: path}
	root, err := repo.git(ctx, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, fmt.Errorf("open Git working repository: %w", err)
	}
	repo.root = strings.TrimSpace(string(root))
	common, err := repo.git(ctx, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return repository{}, err
	}
	repo.gitDir = strings.TrimSpace(string(common))
	return repo, nil
}

func (r repository) git(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return r.runGit(ctx, input, false, args...)
}

func (r repository) networkGit(ctx context.Context, args ...string) ([]byte, error) {
	return r.runGit(ctx, nil, true, args...)
}

func (r repository) runGit(ctx context.Context, input []byte, network bool, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.root}, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=gitsnow", "GIT_AUTHOR_EMAIL=gitsnow@localhost",
		"GIT_COMMITTER_NAME=gitsnow", "GIT_COMMITTER_EMAIL=gitsnow@localhost",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	if network {
		ssh := os.Getenv("GIT_SSH_COMMAND")
		if ssh == "" {
			ssh = "ssh -oBatchMode=yes -oConnectTimeout=10"
		}
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+ssh)
	}
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func (r repository) head(ctx context.Context) (string, error) {
	return r.refHead(ctx, MetadataRef)
}

func (r repository) refHead(ctx context.Context, ref string) (string, error) {
	output, err := r.git(ctx, nil, "show-ref", "--verify", "--hash", ref)
	if err != nil {
		// show-ref --exists distinguishes an absent ref from a broken repository.
		_, existsErr := r.git(ctx, nil, "show-ref", "--exists", ref)
		var exit *exec.ExitError
		if errors.As(existsErr, &exit) && exit.ExitCode() == 2 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (r repository) records(ctx context.Context, head string) ([]Issue, error) {
	_, entries, err := r.issueEntries(ctx, head)
	if err != nil {
		return nil, err
	}
	records := []Issue{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.name, ".md") {
			return nil, fmt.Errorf("unexpected file in metadata issues directory: %s", entry.name)
		}
		data, err := r.git(ctx, nil, "cat-file", "blob", entry.hash)
		if err != nil {
			return nil, err
		}
		issue, err := decodeIssue(data, entry)
		if err != nil {
			return nil, err
		}
		records = append(records, issue)
	}
	return records, nil
}

func (r repository) tree(ctx context.Context, ref string) ([]treeEntry, error) {
	if ref == "" {
		return nil, nil
	}
	output, err := r.git(ctx, nil, "ls-tree", "-z", ref)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		metadata, name, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid Git tree entry")
		}
		entries = append(entries, treeEntry{fields[0], fields[1], fields[2], name})
	}
	return entries, nil
}

func (r repository) issueEntries(ctx context.Context, head string) ([]treeEntry, []treeEntry, error) {
	root, err := r.tree(ctx, head)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range root {
		if entry.name == "issues" {
			if entry.kind != "tree" {
				return nil, nil, fmt.Errorf("metadata issues path is not a directory")
			}
			entries, err := r.tree(ctx, entry.hash)
			return root, entries, err
		}
	}
	return root, nil, nil
}

func encodeIssue(issue Issue) ([]byte, error) {
	header, err := json.MarshalIndent(issueHeader{
		ID: issue.ID, Title: issue.Title, Status: issue.Status,
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []byte("---\n" + string(header) + "\n---\n" + issue.Body), nil
}

func decodeIssue(data []byte, entry treeEntry) (Issue, error) {
	var issue Issue
	if entry.mode != "100644" || entry.kind != "blob" {
		return issue, fmt.Errorf("issue %s must be a regular file", entry.name)
	}
	header, body, ok := strings.Cut(strings.TrimPrefix(string(data), "---\n"), "\n---\n")
	if !bytes.HasPrefix(data, []byte("---\n")) || !ok {
		return issue, fmt.Errorf("issue %s has invalid Markdown frontmatter", entry.name)
	}
	if err := json.Unmarshal([]byte(header), &issue); err != nil {
		return issue, fmt.Errorf("decode issue %s: %w", entry.name, err)
	}
	issue.Body = body
	issue.Revision = entry.hash
	if err := ValidateID(issue.ID); err != nil {
		return issue, err
	}
	if entry.name != issue.ID+".md" {
		return issue, fmt.Errorf("issue filename does not match its ID: %s", entry.name)
	}
	if err := validateInput(Input{Title: issue.Title, Body: body, Status: issue.Status}); err != nil {
		return issue, fmt.Errorf("invalid issue %s: %w", entry.name, err)
	}
	if issue.CreatedAt.IsZero() || issue.UpdatedAt.Before(issue.CreatedAt) {
		return issue, fmt.Errorf("invalid timestamps in issue %s", entry.name)
	}
	return issue, nil
}

func (r repository) writeTree(ctx context.Context, entries []treeEntry) (string, error) {
	var input bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&input, "%s %s %s\t%s%c", entry.mode, entry.kind, entry.hash, entry.name, 0)
	}
	output, err := r.git(ctx, input.Bytes(), "mktree", "-z")
	return strings.TrimSpace(string(output)), err
}

func replaceEntry(entries []treeEntry, name string, replacement *treeEntry) []treeEntry {
	var result []treeEntry
	for _, entry := range entries {
		if entry.name != name {
			result = append(result, entry)
		}
	}
	if replacement != nil {
		result = append(result, *replacement)
	}
	return result
}

func (r repository) ensureMetadataDetached(ctx context.Context) error {
	output, err := r.git(ctx, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range bytes.Split(output, []byte{0}) {
		if string(field) == "branch "+MetadataRef {
			return &ValidationError{"gitsnow/meta is checked out in a worktree; finish reconciliation and remove that worktree before using managed issue writes or synchronization"}
		}
	}
	return nil
}

func (r repository) commitIssue(ctx context.Context, oldHead string, issue Issue, remove bool) (string, string, error) {
	if err := r.ensureMetadataDetached(ctx); err != nil {
		return "", "", err
	}
	root, entries, err := r.issueEntries(ctx, oldHead)
	if err != nil {
		return "", "", err
	}
	var replacement *treeEntry
	if !remove {
		data, err := encodeIssue(issue)
		if err != nil {
			return "", "", err
		}
		output, err := r.git(ctx, data, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", "", err
		}
		replacement = &treeEntry{"100644", "blob", strings.TrimSpace(string(output)), issue.ID + ".md"}
	}
	entries = replaceEntry(entries, issue.ID+".md", replacement)
	tree, err := r.writeTree(ctx, entries)
	if err != nil {
		return "", "", err
	}
	root = replaceEntry(root, "issues", &treeEntry{"040000", "tree", tree, "issues"})
	tree, err = r.writeTree(ctx, root)
	if err != nil {
		return "", "", err
	}
	args := []string{"commit-tree", tree}
	if oldHead != "" {
		args = append(args, "-p", oldHead)
	}
	action := "Save"
	if remove {
		action = "Delete"
	}
	output, err := r.git(ctx, []byte(action+" issue "+issue.ID+"\n"), args...)
	if err != nil {
		return "", "", err
	}
	head := strings.TrimSpace(string(output))
	if _, err := r.git(ctx, nil, "update-ref", MetadataRef, head, oldHead); err != nil {
		current, readErr := r.head(ctx)
		if readErr == nil && current != oldHead {
			return "", "", ErrConflict
		}
		return "", "", err
	}
	revision := ""
	if replacement != nil {
		revision = replacement.hash
	}
	return head, revision, nil
}

func (r repository) indexPath() string {
	return filepath.Join(r.gitDir, "gitsnow", "index.sqlite")
}
