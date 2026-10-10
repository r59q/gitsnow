package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
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
