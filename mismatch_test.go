// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripTrailers(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		{
			name: "single trailer",
			desc: "commit title\n\nChange-Id: I12345",
			want: "commit title",
		},
		{
			name: "multiple trailers",
			desc: "commit title\n\nDetailed body here.\n\nChange-Id: I12345\nReviewed-on: https://gerrit/1\nReviewed-by: Alice <a@example.com>",
			want: "commit title\n\nDetailed body here.",
		},
		{
			name: "trailers with continuation lines",
			desc: "title\n\nSigned-off-by: Bob\n <bob@example.com>\nChange-Id: I999",
			want: "title",
		},
		{
			name: "no trailers",
			desc: "commit title\n\nBody line 1\nBody line 2",
			want: "commit title\n\nBody line 1\nBody line 2",
		},
		{
			name: "empty description",
			desc: "",
			want: "",
		},
		{
			name: "trailers only",
			desc: "Change-Id: I12345\nReviewed-by: Eve",
			want: "",
		},
		{
			name: "body text containing colons in sentences",
			desc: "commit title\n\nThis is a body paragraph with a colon: here in the sentence.",
			want: "commit title\n\nThis is a body paragraph with a colon: here in the sentence.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripTrailers(tc.desc)
			if got != tc.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestFileContentMismatch tests that gerritSync aborts before mutation when an
// upstream commit with the same Change-Id has modified patch content, and that
// its suggested repair command works.
func TestFileContentMismatch(t *testing.T) {
	env := newTestEnv(t)

	// Create change C1 sent to Gerrit
	c1Old, c1Change := env.createLocalChange("commit 1", map[string]string{
		"file.txt": "line 1\nupstream landed version\nline 3\n",
	})

	// Modify C1 locally without sending the new version to Gerrit
	env.editChange(c1Change, "", map[string]string{
		"file.txt": "line 1\nlocal unsubmitted edit\nline 3\n",
	})

	// Gerrit submits the older version that was sent to it (c1Old)
	env.submitCommit(c1Old, "Reviewed-on: https://gerrit/1")

	env.fetchLocal()

	var buf bytes.Buffer
	out := io.MultiWriter(t.Output(), &buf)

	// gerritSync must abort with a mismatchError
	err := gerritSync(out, "master@origin")
	if err == nil {
		t.Fatalf("expected gerritSync to fail on divergent patch, but it succeeded")
	}
	var mErr *mismatchError
	if !errors.As(err, &mErr) {
		t.Fatalf("expected mismatchError, got: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "Mismatch between local and upstream") {
		t.Errorf("expected output to mention mismatch, got:\n%s", output)
	}
	if !strings.Contains(output, "Modified regular file file.txt") {
		t.Errorf("expected interdiff to report modified file.txt, got:\n%s", output)
	}

	// Verify local stack was not modified before remediation: C1 is still present
	stack := env.getStackAbove("master@origin")
	if len(stack) == 0 || !strings.Contains(stack[0], "commit 1") {
		t.Fatalf("expected commit 1 to remain in local stack, got %v", stack)
	}

	content, err := os.ReadFile(filepath.Join(env.localDir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "local unsubmitted edit") {
		t.Fatalf("expected local file.txt to remain unchanged, got %q", string(content))
	}

	// Validate commands against allow-list and execute them
	runRepairCommands(t, env.localDir, mErr.repairCommands())

	// Simulate the user providing a description in the final `jj describe` step
	runCmd(t, env.localDir, "jj", "--no-pager", "describe", "-m", "commit 1 residual")

	// Sync again now that the change has been split into landed and residual commits
	if err := gerritSync(out, "master@origin"); err != nil {
		t.Fatalf("gerritSync failed after executing split commands: %v", err)
	}

	// Verify the stack: landed commit was abandoned, residual commit remains
	stack = env.getStackAbove("master@origin")
	if len(stack) != 1 || !strings.Contains(stack[0], "commit 1 residual") {
		t.Fatalf("expected residual commit 1 in local stack, got %v", stack)
	}

	content, err = os.ReadFile(filepath.Join(env.localDir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	expectedContent := "line 1\nlocal unsubmitted edit\nline 3\n"
	if string(content) != expectedContent {
		t.Fatalf("unexpected file.txt content:\ngot:  %q\nwant: %q", string(content), expectedContent)
	}
}

// TestDescriptionMismatch tests that gerritSync aborts before mutation when an
// upstream commit with the same Change-Id has modified description content.
func TestDescriptionMismatch(t *testing.T) {
	env := newTestEnv(t)

	// Create change C1 sent to Gerrit
	c1Old, c1Change := env.createLocalChange("commit 1 original description sent to gerrit", map[string]string{
		"file.txt": "line 1\nidentical content\nline 3\n",
	})

	// Edit description locally without sending the new version to Gerrit
	env.editChange(c1Change, "commit 1 locally modified description", nil)

	// Gerrit submits the older version that was sent to it (c1Old)
	env.submitCommit(c1Old, "Reviewed-on: https://gerrit/1")

	env.fetchLocal()

	var buf bytes.Buffer
	out := io.MultiWriter(t.Output(), &buf)

	err := gerritSync(out, "master@origin")
	if err == nil {
		t.Fatalf("expected gerritSync to fail on description mismatch, but it succeeded")
	}
	var mErr *mismatchError
	if !errors.As(err, &mErr) {
		t.Fatalf("expected mismatchError, got: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "Mismatch between local and upstream") {
		t.Errorf("expected output to mention mismatch, got:\n%s", output)
	}
	if !strings.Contains(output, "Modified commit description") {
		t.Errorf("expected interdiff to report modified description, got:\n%s", output)
	}

	// Validate commands against allow-list and execute them
	runRepairCommands(t, env.localDir, mErr.repairCommands())

	// Simulate describing the working copy commit
	runCmd(t, env.localDir, "jj", "--no-pager", "describe", "-m", "commit 1 locally modified description")

	// Sync again now that the change has been split into landed and residual commits
	if err := gerritSync(out, "master@origin"); err != nil {
		t.Fatalf("gerritSync failed after executing split commands: %v", err)
	}

	// Verify the stack: landed commit was abandoned, residual commit remains
	stack := env.getStackAbove("master@origin")
	if len(stack) != 1 || !strings.Contains(stack[0], "commit 1 locally modified description") {
		t.Fatalf("expected residual commit with locally modified description in local stack, got %v", stack)
	}
}

func runRepairCommands(t *testing.T, dir string, cmds []repairCommand) {
	t.Helper()
	if len(cmds) == 0 {
		t.Fatalf("expected repairCommands to be non-empty")
	}
	vars := make(map[string]string)
	for _, c := range cmds {
		if len(c.cmd) < 2 || c.cmd[0] != "jj" {
			t.Fatalf("command does not start with 'jj': %v", c.cmd)
		}
		switch c.cmd[1] {
		case "duplicate", "restore", "describe", "squash", "new", "log", "abandon":
			// allowed
		default:
			t.Fatalf("disallowed command in repairCommands: %v", c.cmd)
		}

		args := make([]string, len(c.cmd))
		for i, arg := range c.cmd {
			args[i] = os.Expand(arg, func(name string) string {
				val, ok := vars[name]
				if !ok {
					t.Fatalf("unknown variable $%s in command: %v", name, c.cmd)
				}
				return val
			})
		}

		out := runCmd(t, dir, args[0], args[1:]...)
		if c.setVar != "" {
			vars[c.setVar] = strings.TrimSpace(out)
		}
	}
}

// TestPrecedingUpstreamCommitsIsolation tests that splitting a mismatched change
// applies only the upstream commit's interdiff, and does not pull in any unrelated
// files introduced by earlier upstream commits.
func TestPrecedingUpstreamCommitsIsolation(t *testing.T) {
	env := newTestEnv(t)

	// Step 1: Upstream master gets a preceding commit that creates other.txt
	writeFile(t, filepath.Join(env.gerritDir, "other.txt"), "other upstream file\n")
	runCmd(t, env.gerritDir, "git", "add", "other.txt")
	runCmd(t, env.gerritDir, "git", "commit", "-m", "other commit upstream")
	runCmd(t, env.gerritDir, "git", "push", "origin", "master")

	// Step 2: Local change C1 created (based before other.txt)
	c1Old, c1Change := env.createLocalChange("commit 1", map[string]string{
		"file.txt": "line 1\nupstream landed version\nline 3\n",
	})
	// Local unsubmitted edit
	env.editChange(c1Change, "", map[string]string{
		"file.txt": "line 1\nlocal unsubmitted edit\nline 3\n",
	})
	// Child C2
	env.createLocalChange("commit 2 child", map[string]string{
		"file2.txt": "child file\n",
	})

	// Upstream Gerrit lands C1 (on top of other.txt!)
	env.submitCommit(c1Old, "Reviewed-on: https://gerrit/1")
	env.fetchLocal()

	// Initial sync must fail with mismatchError
	var buf bytes.Buffer
	out := io.MultiWriter(t.Output(), &buf)
	err := gerritSync(out, "master@origin")
	if err == nil {
		t.Fatalf("expected gerritSync to fail on mismatch, but it succeeded")
	}
	var mErr *mismatchError
	if !errors.As(err, &mErr) {
		t.Fatalf("expected mismatchError, got %v", err)
	}

	// Validate and run the recommended split commands
	runRepairCommands(t, env.localDir, mErr.repairCommands())

	// Simulate describing the residual commit
	runCmd(t, env.localDir, "jj", "--no-pager", "describe", "-m", "commit 1 residual")

	// Sync again now that change has been split
	if err := gerritSync(out, "master@origin"); err != nil {
		t.Fatalf("gerritSync failed after split commands: %v", err)
	}

	// Verify no conflicts
	if conflicts := env.getConflicts(); len(conflicts) > 0 {
		t.Fatalf("expected zero conflicts, got: %v", conflicts)
	}

	// Verify the stack above master@origin contains [commit 1 residual, commit 2 child]
	stack := env.getStackAbove("master@origin")
	if len(stack) != 2 || !strings.Contains(stack[0], "commit 1 residual") || !strings.Contains(stack[1], "commit 2 child") {
		t.Fatalf("expected [commit 1 residual, commit 2 child] in local stack, got %v", stack)
	}

	// Verify that C1's residual commit does NOT contain other.txt in its own diff
	// (i.e. other.txt came from master@origin, not from the residual commit)
	c1ResidualDiff := runCmd(t, env.localDir, "jj", "--no-pager", "diff", "-r", "@-", "--stat")
	if strings.Contains(c1ResidualDiff, "other.txt") {
		t.Errorf("residual commit diff should not touch other.txt:\n%s", c1ResidualDiff)
	}
}
