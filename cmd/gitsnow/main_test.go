package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r59q/gitsnow/internal/issues"
)

func TestCLIWorkflow(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize Git: %v: %s", err, output)
	}
	invoke := func(args ...string) (string, error) {
		t.Helper()
		var output, stderr bytes.Buffer
		err := run(context.Background(), args, &output, &stderr)
		return output.String(), err
	}
	output, err := invoke("issue", "create", "--repo", root, "--title", "CLI issue", "--body", "First body")
	if err != nil {
		t.Fatal(err)
	}
	var issue issues.Issue
	if err := json.Unmarshal([]byte(output), &issue); err != nil {
		t.Fatal(err)
	}
	output, err = invoke("issue", "update", "--repo", root, "--status", "closed", "--body", "", issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	var updated issues.Issue
	if err := json.Unmarshal([]byte(output), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Title != issue.Title || updated.Body != "" || updated.Status != "closed" {
		t.Fatalf("partial update did not preserve fields: %+v", updated)
	}
	if _, err := invoke("issue", "delete", "--repo", root, issue.ID); err == nil {
		t.Fatal("delete accepted missing revision")
	}
	if _, err := invoke("issue", "delete", "--repo", root, "--revision", issue.Revision, issue.ID); err == nil {
		t.Fatal("delete accepted stale revision")
	}
	if _, err := invoke("issue", "delete", "--repo", root, "--revision", updated.Revision, issue.ID); err != nil {
		t.Fatal(err)
	}
	output, err = invoke("issue", "list", "--repo", root)
	if err != nil || strings.TrimSpace(output) != "[]" {
		t.Fatalf("unexpected list: %s, %v", output, err)
	}
}

func TestCLIRejectsRemoteServing(t *testing.T) {
	var output bytes.Buffer
	for _, addr := range []string{"0.0.0.0:7331", "[::]:7331", "attacker.example:7331", ":7331"} {
		err := run(context.Background(), []string{"serve", "--listen", addr}, &output, &output)
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("accepted remote listener %s: %v", addr, err)
		}
	}
}

func TestCLIAutomaticSharingAndOptOut(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	left, right := filepath.Join(root, "left"), filepath.Join(root, "right")
	git := func(args ...string) string {
		t.Helper()
		output, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--bare", "-q", remote)
	for _, repo := range []string{left, right} {
		git("init", "-q", repo)
		git("-C", repo, "remote", "add", "origin", remote)
	}
	invoke := func(args ...string) (string, string, error) {
		t.Helper()
		var output, stderr bytes.Buffer
		err := run(context.Background(), args, &output, &stderr)
		return output.String(), stderr.String(), err
	}
	output, stderr, err := invoke("issue", "create", "--repo", left, "--title", "Automatically shared")
	if err != nil || !strings.Contains(stderr, "Shared with origin") {
		t.Fatalf("create did not share: %s %s %v", output, stderr, err)
	}
	output, _, err = invoke("issue", "list", "--repo", right)
	var list []issues.Issue
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(output), &list); err != nil || len(list) != 1 || list[0].Title != "Automatically shared" {
		t.Fatalf("list did not fetch shared issues: %s %v", output, err)
	}
	if _, _, err := invoke("sync", "--repo", left, "--remote", "off"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke("issue", "create", "--repo", left, "--title", "Local-only issue"); err != nil {
		t.Fatal(err)
	}
	output, _, err = invoke("issue", "list", "--repo", right)
	if err != nil || strings.Contains(output, "Local-only") {
		t.Fatalf("local-only issue was shared: %s %v", output, err)
	}
	if _, _, err := invoke("sync", "--repo", left, "--remote", "origin"); err != nil {
		t.Fatal(err)
	}
	output, _, err = invoke("issue", "list", "--repo", right)
	if err != nil || !strings.Contains(output, "Local-only") {
		t.Fatalf("re-enabling sync did not publish pending data: %s %v", output, err)
	}
	if refs := git("--git-dir="+remote, "for-each-ref", "--format=%(refname)"); refs != issues.MetadataRef {
		t.Fatalf("CLI pushed unexpected refs: %s", refs)
	}
}

func TestCLIReportsSavedButUnsharedMutation(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, output)
	}
	if output, err := exec.Command("git", "-C", root, "remote", "add", "origin", filepath.Join(root, "missing.git")).CombinedOutput(); err != nil {
		t.Fatalf("remote: %v %s", err, output)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"issue", "create", "--repo", root, "--title", "Safe offline"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "saved locally, but not shared") {
		t.Fatalf("remote failure hidden: %v", err)
	}
	var issue issues.Issue
	if err := json.Unmarshal(stdout.Bytes(), &issue); err != nil || issue.ID == "" {
		t.Fatalf("failed to return persisted issue for reconciliation: %s %v", stdout.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "gitsnow", "index.sqlite")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err = run(context.Background(), []string{"issue", "list", "--repo", root}, &stdout, &stderr)
	if err != nil || !strings.Contains(stdout.String(), issue.ID) || !strings.Contains(stderr.String(), "showing local records") {
		t.Fatalf("offline reads did not report cached data: %s %s %v", stdout.String(), stderr.String(), err)
	}
}
