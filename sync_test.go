// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type testEnv struct {
	t         *testing.T
	root      string
	remoteGit string // bare Git repo acting as the central remote ("origin")
	gerritDir string // non-bare Git clone used to simulate Gerrit landing changes
	localDir  string // local Jujutsu repo under test
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	// remote.git is the bare central repository (the "origin" remote).
	remote := filepath.Join(root, "remote.git")
	runCmd(t, root, "git", "init", "--bare", "-b", "master", remote)

	// gerrit is a working Git clone of remote.git used to simulate Gerrit's
	// server-side actions: cherry-picking local commits onto master, amending
	// them with Gerrit trailers, and pushing them to remote.git.
	gerrit := filepath.Join(root, "gerrit")
	runCmd(t, root, "git", "clone", remote, gerrit)
	runCmd(t, gerrit, "git", "config", "user.name", "Test User")
	runCmd(t, gerrit, "git", "config", "user.email", "test@example.com")

	writeFile(t, filepath.Join(gerrit, "file.txt"), "line 1\nline 2\nline 3\n")
	runCmd(t, gerrit, "git", "add", "file.txt")
	runCmd(t, gerrit, "git", "commit", "-m", "initial commit")
	runCmd(t, gerrit, "git", "push", "origin", "master")

	// local is the developer's Jujutsu repository cloned from remote.git.
	local := filepath.Join(root, "local")
	runCmd(t, root, "jj", "--no-pager", "git", "clone", remote, local)
	runCmd(t, local, "jj", "--no-pager", "config", "set", "--repo", "user.name", "Test User")
	runCmd(t, local, "jj", "--no-pager", "config", "set", "--repo", "user.email", "test@example.com")
	runCmd(t, local, "jj", "--no-pager", "config", "set", "--repo", "ui.editor", "\"true\"")

	t.Chdir(local)

	return &testEnv{
		t:         t,
		root:      root,
		remoteGit: remote,
		gerritDir: gerrit,
		localDir:  local,
	}
}

func (e *testEnv) writeFiles(files map[string]string) {
	e.t.Helper()
	for relPath, content := range files {
		fullPath := filepath.Join(e.localDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			e.t.Fatalf("MkdirAll: %v", err)
		}
		writeFile(e.t, fullPath, content)
	}
}

func (e *testEnv) createLocalChange(summary string, files map[string]string) (gitCommitHash, jjChangeID) {
	e.t.Helper()
	e.writeFiles(files)

	runCmd(e.t, e.localDir, "jj", "--no-pager", "commit", "-m", summary)
	out := strings.TrimSpace(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", "@-", "--no-graph", "-T", `commit_id ++ "\t" ++ change_id`))
	commitID, changeID, ok := strings.Cut(out, "\t")
	if !ok {
		e.t.Fatalf("unexpected jj log output: %q", out)
	}
	return gitCommitHash(commitID), jjChangeID(changeID)
}

func (e *testEnv) editChange(changeID jjChangeID, summary string, files map[string]string) gitCommitHash {
	e.t.Helper()
	if len(files) > 0 {
		e.writeFiles(files)
		runCmd(e.t, e.localDir, "jj", "--no-pager", "squash", "--into", string(changeID))
	}
	if summary != "" {
		runCmd(e.t, e.localDir, "jj", "--no-pager", "describe", "-r", string(changeID), "-m", summary)
	}
	return gitCommitHash(strings.TrimSpace(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", string(changeID), "--no-graph", "-T", "commit_id")))
}

func (e *testEnv) submitCommit(gitCommitID gitCommitHash, trailers string) {
	e.t.Helper()
	runCmd(e.t, e.gerritDir, "git", "fetch", filepath.Join(e.localDir, ".git"), string(gitCommitID))
	runCmd(e.t, e.gerritDir, "git", "cherry-pick", string(gitCommitID))
	cid := e.getGerritChangeID(gitCommitID)
	msg := runCmd(e.t, e.gerritDir, "git", "log", "-1", "--format=%B")
	newMsg := fmt.Sprintf("%s\n\nChange-Id: %s\n%s\n", strings.TrimRight(msg, "\r\n"), cid, trailers)
	runCmd(e.t, e.gerritDir, "git", "commit", "--amend", "-m", newMsg)
	runCmd(e.t, e.gerritDir, "git", "push", "origin", "master")
}

func (e *testEnv) getGerritChangeID(commitID gitCommitHash) gerritChangeID {
	e.t.Helper()
	template := `
		if(
			trailers.contains_key("Change-Id"),
			trailers.filter(|t| t.key() == "Change-Id").last().value().trim(),
			stringify(format_gerrit_change_id_trailer(self)).remove_prefix("Change-Id: ").trim(),
		)
	`
	out := strings.TrimSpace(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", string(commitID), "--no-graph", "-T", template))
	return gerritChangeID(out)
}

func (e *testEnv) getChangeID(rev string) jjChangeID {
	e.t.Helper()
	return jjChangeID(strings.TrimSpace(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", rev, "--no-graph", "-T", "change_id")))
}

func (e *testEnv) getCommitID(rev string) gitCommitHash {
	e.t.Helper()
	return gitCommitHash(strings.TrimSpace(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", rev, "--no-graph", "-T", "commit_id")))
}

func (e *testEnv) fetchLocal() {
	e.t.Helper()
	runCmd(e.t, e.localDir, "jj", "--no-pager", "git", "fetch")
}

func (e *testEnv) getConflicts() []gitCommitHash {
	e.t.Helper()
	var commits []gitCommitHash
	for _, id := range lines(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", "conflicts()", "--no-graph", "-T", `commit_id ++ "\n"`)) {
		commits = append(commits, gitCommitHash(id))
	}
	return commits
}

func (e *testEnv) getStackAbove(target string) []string {
	e.t.Helper()
	return lines(runCmd(e.t, e.localDir, "jj", "--no-pager", "log", "-r", fmt.Sprintf("(%s..@):: ~ (empty() & description(exact:\"\") & @)", target), "--reversed", "--no-graph", "-T", `description.first_line() ++ "\n"`))
}

func runCmd(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s in %s failed: %v\nstderr:\n%s\nstdout:\n%s", name, strings.Join(args, " "), dir, err, stderr.String(), out)
	}
	return string(out)
}

func lines(str string) []string {
	if str == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(str, "\n"), "\n")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// TestDiffHunkConflict tests the case where later patches touch the same diff
// hunks as earlier patches. In this situation, plain `jj rebase -d target`
// produces merge conflicts because it only compares against target's final tree
// state. jj-gerrit-sync, by rebasing and abandoning landed commits
// individually, resolves cleanly with zero conflicts.
func TestDiffHunkConflict(t *testing.T) {
	env := newTestEnv(t)

	// Local stack:
	// C1 modifies line 2
	c1, _ := env.createLocalChange("commit 1", map[string]string{
		"file.txt": "line 1\nline 2 modified by C1\nline 3\n",
	})
	// C2 modifies line 2 again
	c2, _ := env.createLocalChange("commit 2", map[string]string{
		"file.txt": "line 1\nline 2 modified by C2\nline 3\n",
	})
	// C3 is unlanded
	_, c3ChangeID := env.createLocalChange("commit 3 unlanded", map[string]string{
		"unlanded.txt": "unlanded content\n",
	})

	// Upstream lands C1 and C2 via Gerrit cherry-pick
	env.submitCommit(c1, "Reviewed-on: https://gerrit/1\nReviewed-by: Reviewer 1 <r1@example.com>")
	env.submitCommit(c2, "Reviewed-on: https://gerrit/2\nReviewed-by: Reviewer 2 <r2@example.com>")

	// Fetch upstream into local
	env.fetchLocal()

	// 1. Verify that plain `jj rebase -d master@origin --skip-emptied` DOES create conflicts
	rebaseCmd := exec.Command("jj", "--no-pager", "rebase", "-d", "master@origin", "--skip-emptied")
	rebaseCmd.Dir = env.localDir
	rebaseCmd.Stdout = t.Output()
	rebaseCmd.Stderr = t.Output()
	_ = rebaseCmd.Run()
	conflicts := env.getConflicts()
	if len(conflicts) == 0 {
		t.Fatalf("expected plain jj rebase to produce conflicts, but found none")
	}

	// Undo the rebase to restore original local stack
	runCmd(t, env.localDir, "jj", "--no-pager", "undo")

	// 2. Run gerritSync and verify it succeeds without conflicts
	if err := gerritSync(t.Output(), "master@origin"); err != nil {
		t.Fatalf("gerritSync failed: %v", err)
	}

	// Verify no conflicts exist
	if conflicts := env.getConflicts(); len(conflicts) > 0 {
		t.Fatalf("expected zero conflicts after gerritSync, got %v", conflicts)
	}

	// Verify only unlanded commit 3 remains in the stack above master@origin
	stack := env.getStackAbove("master@origin")
	if len(stack) != 1 || stack[0] != "commit 3 unlanded" {
		t.Fatalf("expected stack above master@origin to be [commit 3 unlanded], got %v", stack)
	}

	// Verify the unlanded commit preserved its jj change ID
	if got := env.getChangeID("@-"); got != c3ChangeID {
		t.Fatalf("expected unlanded commit change ID %s to be preserved, got %s", c3ChangeID, got)
	}

	// Verify file content in working copy
	fileContent, err := os.ReadFile(filepath.Join(env.localDir, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile(file.txt): %v", err)
	}
	expectedFile := "line 1\nline 2 modified by C2\nline 3\n"
	if string(fileContent) != expectedFile {
		t.Fatalf("unexpected file.txt content:\ngot:  %q\nwant: %q", string(fileContent), expectedFile)
	}

	unlandedContent, err := os.ReadFile(filepath.Join(env.localDir, "unlanded.txt"))
	if err != nil {
		t.Fatalf("ReadFile(unlanded.txt): %v", err)
	}
	if string(unlandedContent) != "unlanded content\n" {
		t.Fatalf("unexpected unlanded.txt content: %q", string(unlandedContent))
	}
}



// TestNonSequentialCherryPicks tests syncing when Gerrit lands commits out of
// their local stack order (e.g. C1 and C3 land, while C2 and C4 are unlanded).
func TestNonSequentialCherryPicks(t *testing.T) {
	env := newTestEnv(t)

	// Local stack: C1 -> C2 -> C3 -> C4
	c1, _ := env.createLocalChange("commit 1", map[string]string{
		"f1.txt": "c1 content\n",
	})
	env.createLocalChange("commit 2 unlanded", map[string]string{
		"f2.txt": "c2 content\n",
	})
	c3, _ := env.createLocalChange("commit 3", map[string]string{
		"f3.txt": "c3 content\n",
	})
	env.createLocalChange("commit 4 unlanded", map[string]string{
		"f4.txt": "c4 content\n",
	})

	// Upstream Gerrit cherry-picks C1 and C3, leaving C2 and C4 in review
	env.submitCommit(c1, "Reviewed-on: https://gerrit/1")
	env.submitCommit(c3, "Reviewed-on: https://gerrit/3")

	env.fetchLocal()

	if err := gerritSync(t.Output(), "master@origin"); err != nil {
		t.Fatalf("gerritSync failed: %v", err)
	}

	// Verify no conflicts exist
	if conflicts := env.getConflicts(); len(conflicts) > 0 {
		t.Fatalf("expected zero conflicts, got: %v", conflicts)
	}

	// Remaining stack should contain C2 and C4
	stack := env.getStackAbove("master@origin")
	if len(stack) != 2 {
		t.Fatalf("expected 2 unlanded commits, got %v", stack)
	}
	if stack[0] != "commit 2 unlanded" || stack[1] != "commit 4 unlanded" {
		t.Fatalf("expected [commit 2 unlanded, commit 4 unlanded], got %v", stack)
	}

	// All files should exist with correct contents
	for _, f := range []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt"} {
		path := filepath.Join(env.localDir, f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("file %s missing: %v", f, err)
		}
	}
}

// TestNoLandedCommits tests that gerritSync is a no-op when no commits in the
// local stack have landed upstream.
func TestNoLandedCommits(t *testing.T) {
	env := newTestEnv(t)

	env.createLocalChange("unlanded 1", map[string]string{
		"unlanded.txt": "data\n",
	})

	var buf bytes.Buffer
	out := io.MultiWriter(t.Output(), &buf)

	if err := gerritSync(out, "master@origin"); err != nil {
		t.Fatalf("expected gerritSync to succeed without error, got: %v", err)
	}

	wantMsg := "None of the 2 commit(s) in the current stack have landed upstream."
	if !strings.Contains(buf.String(), wantMsg) {
		t.Errorf("expected output to contain %q, got:\n%s", wantMsg, buf.String())
	}

	stack := env.getStackAbove("master@origin")
	if len(stack) != 1 || stack[0] != "unlanded 1" {
		t.Fatalf("expected stack to be preserved, got %v", stack)
	}
}

// TestDryRun tests that -dry-run verifies landed commits but performs no
// mutations.
func TestDryRun(t *testing.T) {
	env := newTestEnv(t)

	c1, _ := env.createLocalChange("commit 1", map[string]string{
		"f1.txt": "c1\n",
	})
	env.createLocalChange("commit 2 unlanded", map[string]string{
		"f2.txt": "c2\n",
	})

	env.submitCommit(c1, "Reviewed-on: https://gerrit/1")
	env.fetchLocal()

	*dryRun = true
	defer func() { *dryRun = false }()

	var buf bytes.Buffer
	out := io.MultiWriter(t.Output(), &buf)

	if err := gerritSync(out, "master@origin"); err != nil {
		t.Fatalf("gerritSync dry-run failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Found 1 landed commit(s) in local stack:") {
		t.Errorf("expected output to report 1 landed commit, got:\n%s", output)
	}
	if !strings.Contains(output, "Abandoning ") {
		t.Errorf("expected output to report abandoning commit, got:\n%s", output)
	}
	if !strings.Contains(output, "Dry run complete! Remaining unlanded changes (2) would be rebased cleanly.") {
		t.Errorf("expected output to report dry run complete, got:\n%s", output)
	}

	// Under dry-run, commit 1 should NOT be abandoned
	stack := env.getStackAbove("master@origin")
	if len(stack) != 2 {
		t.Fatalf("expected stack to remain untouched in dry-run, got %v", stack)
	}
}

// TestEmptyStack tests that gerritSync reports an empty stack and exits cleanly
// when there are no commits in the revset relative to target.
func TestEmptyStack(t *testing.T) {
	_ = newTestEnv(t)

	var buf bytes.Buffer
	out := io.MultiWriter(t.Output(), &buf)

	if err := gerritSync(out, "@"); err != nil {
		t.Fatalf("expected gerritSync to succeed on empty stack, got: %v", err)
	}

	wantMsg := "Stack is empty relative to @. Nothing to rebase."
	if !strings.Contains(buf.String(), wantMsg) {
		t.Errorf("expected output to contain %q, got:\n%s", wantMsg, buf.String())
	}
}

// TestDuplicateGerritChangeID tests that gerritSync errors when multiple commits
// in the local stack or upstream have the same Gerrit Change-Id.
func TestDuplicateGerritChangeID(t *testing.T) {
	t.Run("LocalStack", func(t *testing.T) {
		env := newTestEnv(t)

		c1, _ := env.createLocalChange("commit 1", map[string]string{
			"f1.txt": "content 1\n",
		})
		cid := env.getGerritChangeID(c1)

		// Create a second commit with the same Change-Id trailer
		env.createLocalChange(fmt.Sprintf("commit 2\n\nChange-Id: %s", cid), map[string]string{
			"f2.txt": "content 2\n",
		})

		var buf bytes.Buffer
		out := io.MultiWriter(t.Output(), &buf)

		err := gerritSync(out, "master@origin")
		if err == nil {
			t.Fatalf("expected gerritSync to fail on duplicate Gerrit Change-Id in local stack, but it succeeded")
		}
		if !strings.Contains(err.Error(), "duplicate Gerrit Change-Id") {
			t.Fatalf("expected error mentioning duplicate Gerrit Change-Id, got: %v", err)
		}
	})

	t.Run("Upstream", func(t *testing.T) {
		env := newTestEnv(t)

		// Create two upstream commits with the same Change-Id trailer
		cid := "I0123456789abcdef0123456789abcdef01234567"
		writeFile(t, filepath.Join(env.gerritDir, "up1.txt"), "up 1\n")
		runCmd(t, env.gerritDir, "git", "add", "up1.txt")
		runCmd(t, env.gerritDir, "git", "commit", "-m", fmt.Sprintf("up 1\n\nChange-Id: %s", cid))

		writeFile(t, filepath.Join(env.gerritDir, "up2.txt"), "up 2\n")
		runCmd(t, env.gerritDir, "git", "add", "up2.txt")
		runCmd(t, env.gerritDir, "git", "commit", "-m", fmt.Sprintf("up 2\n\nChange-Id: %s", cid))
		runCmd(t, env.gerritDir, "git", "push", "origin", "master")

		env.fetchLocal()

		env.createLocalChange("local commit", map[string]string{
			"local.txt": "local\n",
		})

		var buf bytes.Buffer
		out := io.MultiWriter(t.Output(), &buf)

		err := gerritSync(out, "master@origin")
		if err == nil {
			t.Fatalf("expected gerritSync to fail on duplicate Gerrit Change-Id upstream, but it succeeded")
		}
		if !strings.Contains(err.Error(), "duplicate Gerrit Change-Id") {
			t.Fatalf("expected error mentioning duplicate Gerrit Change-Id, got: %v", err)
		}
	})
}

