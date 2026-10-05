// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

//go:embed doc.go
var docSource string

var dryRun = flag.Bool("dry-run", false, "skip mutations (rebase and abandon), but still print actions")

func main() {
	fetch := flag.Bool("fetch", false, "fetch latest changes from remote")
	var ontoFlag, remoteFlag string
	flag.StringVar(&ontoFlag, "onto", "master", "destination branch or `bookmark` (optionally <branch>@<remote>)")
	flag.StringVar(&remoteFlag, "remote", "", "git `remote` to fetch from and sync against (defaults to git.fetch, then origin)")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage: jj-gerrit-sync [flags]\n\n")
		_, doc, _ := strings.Cut(docSource, "/*")
		doc, _, _ = strings.Cut(doc, "*/")
		fmt.Fprintln(out, strings.TrimSpace(doc))
		fmt.Fprintln(out, "\nFlags:")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *fetch && *dryRun {
		fmt.Fprintln(os.Stderr, "Error: -fetch and -dry-run cannot be used together.")
		os.Exit(1)
	}

	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(1)
	}

	branch, remote, target, err := resolveTarget(ontoFlag, remoteFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if *fetch {
		fmt.Printf("Fetching latest changes from remote tracking branch (%s)...\n", target)
		if _, err := runJJ("git", "fetch", "--remote", remote, "--branch", branch); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	if err := gerritSync(os.Stdout, target); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// jjChangeID is a Jujutsu change ID.
type jjChangeID string

// gerritChangeID is a Gerrit Change-Id trailer value.
type gerritChangeID string

// gitCommitHash is a full Git commit object hash.
type gitCommitHash string

type commitInfo struct {
	CommitID         gitCommitHash
	ChangeID         jjChangeID
	Summary          string
	GerritCID        gerritChangeID
	UpstreamCommitID gitCommitHash
}

func gerritSync(out io.Writer, target string) error {
	// Extract upstream commits and their Gerrit Change-Ids (@..target)
	_, upstreamByCID, err := commitsByGerritCID(fmt.Sprintf("@..%s", target))
	if err != nil {
		return fmt.Errorf("inspecting upstream commits: %w", err)
	}

	// Collect commits in the local stack ((target..@)::)
	//
	// Inspect commits in the branch being rebased, including any descendants
	// on top of @, matching what `jj rebase -b @ -d target` moves.
	stackCommits, _, err := commitsByGerritCID(fmt.Sprintf("(%s..@)::", target))
	if err != nil {
		return fmt.Errorf("reading local stack: %w", err)
	}

	if len(stackCommits) == 0 {
		fmt.Fprintf(out, "Stack is empty relative to %s. Nothing to rebase.\n", target)
		return nil
	}

	// Split the local stack into landed and unlanded commits
	var landed []commitInfo
	var unlanded []commitInfo
	for _, commit := range stackCommits {
		if commit.GerritCID != "" {
			if up, found := upstreamByCID[commit.GerritCID]; found {
				commit.UpstreamCommitID = up.CommitID
				landed = append(landed, commit)
				continue
			}
		}
		unlanded = append(unlanded, commit)
	}
	if len(landed) == 0 {
		fmt.Fprintf(out, "None of the %d commit(s) in the current stack have landed upstream.\n", len(stackCommits))
		return nil
	}
	fmt.Fprintf(out, "Found %d landed commit(s) in local stack:\n", len(landed))
	for _, c := range landed {
		fmt.Fprintf(out, "    -f %s -t %s (%s): %s\n", shortID(c.ChangeID, 12), shortID(c.UpstreamCommitID, 12), shortID(c.GerritCID, 10), c.Summary)
	}
	fmt.Fprintln(out)

	// Pre-flight diff verification with `jj interdiff`
	fmt.Fprintln(out, "Validating patch equivalence using `jj interdiff`...")
	if err := verifyPatchEquivalence(out, landed); err != nil {
		return fmt.Errorf("validating patch equivalence: %w", err)
	}

	// Rebase the entire branch onto target (`-d target`).
	//
	// Often, changes that landed upstream will simply be empty after this
	// rebase, but not always! (If this always happened, there would be way less
	// need for this tool.) jj rebase only looks at the final tree state of the
	// target, so if a *later* upstream change touched the same diff hunks, this
	// will result in a conflict. And in a large stack, this is pretty likely to
	// happen, because later changes on the local stack that have landed
	// upstream are likely to touch the same diff hunks.
	fmt.Fprintf(out, "Rebasing entire stack onto %s...\n", target)
	if !*dryRun {
		if _, err := runJJ("rebase", "-d", target); err != nil {
			return fmt.Errorf("rebasing stack: %w", err)
		}
	}

	// Abandon landed commits individually
	//
	// This will eliminate the harmless empty commits left by the rebase, as
	// well as the conflicts caused by interleaving issues described above. We
	// *should* wind up with a conflict-free stack at the end.
	fmt.Fprintln(out, "Abandoning landed changes individually...")
	for _, c := range landed {
		fmt.Fprintf(out, "    Abandoning %s (%s)...\n", shortID(c.ChangeID, 12), c.Summary)
		if !*dryRun {
			if _, err := runJJ("abandon", string(c.ChangeID)); err != nil {
				return fmt.Errorf("abandoning %s: %w", c.ChangeID, err)
			}
		}
	}

	if *dryRun {
		fmt.Fprintf(out, "\nDry run complete! Remaining unlanded changes (%d) would be rebased cleanly.\n", len(unlanded))
	} else {
		fmt.Fprintf(out, "\nSync complete! Remaining unlanded changes (%d) rebased cleanly.\n", len(unlanded))
	}
	return nil
}

// runJJ executes a jujutsu command with --no-pager --ignore-working-copy and
// returns stdout as a string.
func runJJ(args ...string) (string, error) {
	cmdArgs := []string{"--no-pager"}

	// Use --ignore-working-copy if possible to save some time by not scanning
	// the repo for changes.
	needsWorkingCopy := false
	switch args[0] {
	case "rebase", "abandon":
		needsWorkingCopy = true
	}
	if !needsWorkingCopy {
		cmdArgs = append(cmdArgs, "--ignore-working-copy")
	}

	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("jj", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("command 'jj %s' failed: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

func shortID[T ~string](id T, n int) string {
	if len(id) <= n {
		return string(id)
	}
	return string(id[:n])
}

// resolveTarget determines the destination branch, remote, and fully-qualified
// tracking target (<branch>@<remote>) from the -onto and -remote flags.
// If -onto is omitted, the branch defaults to "master".
//
// There are two cases for target resolution:
//
//	Case 1: -onto specifies `<branch>@<remote>`:
//	  - The remote is extracted directly from -onto.
//	  - If -remote is also explicitly passed, it must match this remote.
//	Case 2: -onto specifies only `<branch>`:
//	  - The remote is taken from -remote if provided.
//	  - Otherwise, the remote falls back to jj's git.fetch config, or "origin".
func resolveTarget(ontoFlag, remoteFlag string) (branch, remote, target string, err error) {
	// Default the branch to "master" if no destination is provided.
	if ontoFlag == "" {
		ontoFlag = "master"
	}

	// Disallow ambiguous targets with multiple '@' delimiters.
	if strings.Count(ontoFlag, "@") > 1 {
		return "", "", "", fmt.Errorf("invalid target %q", ontoFlag)
	}

	// Case 1: Target includes an explicit remote (<branch>@<remote>).
	if b, r, ok := strings.Cut(ontoFlag, "@"); ok {
		if b == "" || r == "" {
			return "", "", "", fmt.Errorf("invalid target %q", ontoFlag)
		}
		// When both -onto and -remote define a remote, they must agree.
		if remoteFlag != "" && remoteFlag != r {
			return "", "", "", fmt.Errorf("-onto remote %q does not match -remote %q", r, remoteFlag)
		}
		branch, remote = b, r
	} else {
		// Case 2: Target is just a branch name (<branch>).
		branch = ontoFlag

		// Resolve the remote: prefer explicit -remote flag; otherwise discover
		// the default remote via jj's git.fetch configuration or remote list.
		if remoteFlag != "" {
			remote = remoteFlag
		} else {
			remote, err = defaultRemote()
			if err != nil {
				return "", "", "", fmt.Errorf("determining default remote: %w", err)
			}
		}
	}

	// Construct the canonical Jujutsu tracking bookmark revset (e.g. "master@origin").
	target = fmt.Sprintf("%s@%s", branch, remote)
	return branch, remote, target, nil
}

// defaultRemote determines the default Git remote to use, matching Jujutsu's
// documented remote resolution logic for `jj git fetch` (see `jj help git fetch`
// and `jj help -k config` under "Default remotes for jj git fetch and jj git push"):
//
//  1. Check `git.fetch` config:
//     Jujutsu allows overriding the default fetch remote(s) via the `git.fetch` setting,
//     which can be a single remote string (e.g. "upstream") or a list of remotes
//     (e.g. ["origin", "upstream"]). If set, we use the first remote from this list.
//
//  2. Inspect available remotes via `jj git remote list`:
//     If `git.fetch` is not configured, Jujutsu rules state:
//     "if a single remote exists it is used for `jj git fetch` and `jj git push`;
//     however if multiple remotes exist, the default remote is assumed to be named 'origin'".
//     Therefore:
//     - If there is exactly one remote, return that remote (even if not named "origin").
//     - If multiple remotes exist and "origin" is among them, return "origin".
//     - If multiple remotes exist without "origin", return the first remote.
//
//  3. Default fallback:
//     If no remotes are found or queries fail, fall back to "origin".
func defaultRemote() (string, error) {
	out, err := runJJ("config", "get", "git.fetch")
	if err == nil {
		out = strings.TrimSpace(out)
		if strings.HasPrefix(out, "[") && strings.HasSuffix(out, "]") {
			var list []string
			if jsonErr := json.Unmarshal([]byte(out), &list); jsonErr == nil && len(list) > 0 {
				if r := strings.TrimSpace(list[0]); r != "" {
					return r, nil
				}
			}
			trimmed := strings.Trim(out, "[]")
			for _, item := range strings.Split(trimmed, ",") {
				item = strings.Trim(strings.TrimSpace(item), `"'`)
				if item != "" {
					return item, nil
				}
			}
		} else {
			rem := strings.Trim(out, `"'`)
			if rem != "" {
				return rem, nil
			}
		}
	}

	remoteOut, err := runJJ("git", "remote", "list")
	if err == nil {
		var remotes []string
		for _, line := range strings.Split(remoteOut, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				remotes = append(remotes, fields[0])
			}
		}
		if len(remotes) == 1 {
			return remotes[0], nil
		}
		for _, r := range remotes {
			if r == "origin" {
				return "origin", nil
			}
		}
		if len(remotes) > 0 {
			return remotes[0], nil
		}
	}

	return "origin", nil
}

// commitsByGerritCID queries a revset in chronological order (--reversed) using jj log
// and extracts commit metadata including Gerrit Change-Ids. If a commit lacks an explicit
// Change-Id trailer, it falls back to format_gerrit_change_id_trailer(self), matching jj's
// transient change ID generation.
// It returns an ordered slice of commits and a lookup map keyed by Gerrit Change-Id,
// or an error if multiple commits share the same Gerrit Change-Id.
func commitsByGerritCID(revset string) ([]commitInfo, map[gerritChangeID]commitInfo, error) {
	template := `
		commit_id ++ "\t" ++
		change_id ++ "\t" ++
		if(
			trailers.contains_key("Change-Id"),
			trailers.filter(|t| t.key() == "Change-Id").last().value().trim(),
			stringify(format_gerrit_change_id_trailer(self)).remove_prefix("Change-Id: ").trim(),
		) ++ "\t" ++
		description.first_line() ++ "\n"
	`
	out, err := runJJ("log", "-r", revset, "--reversed", "--no-graph", "-T", template)
	if err != nil {
		return nil, nil, fmt.Errorf("reading commits for %s: %w", revset, err)
	}

	var commits []commitInfo
	byCID := make(map[gerritChangeID]commitInfo)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 3 {
			continue
		}
		commit := commitInfo{
			CommitID:  gitCommitHash(parts[0]),
			ChangeID:  jjChangeID(parts[1]),
			GerritCID: gerritChangeID(parts[2]),
		}
		if len(parts) > 3 {
			commit.Summary = parts[3]
		}
		commits = append(commits, commit)
		if commit.GerritCID != "" {
			if prev, exists := byCID[commit.GerritCID]; exists {
				if prev.ChangeID == commit.ChangeID {
					return nil, nil, fmt.Errorf("duplicate Gerrit Change-Id %s (divergent change %s: %s and %s)", commit.GerritCID, shortID(commit.ChangeID, 12), shortID(prev.CommitID, 12), shortID(commit.CommitID, 12))
				}
				return nil, nil, fmt.Errorf("duplicate Gerrit Change-Id %s (%s and %s)", commit.GerritCID, shortID(prev.ChangeID, 12), shortID(commit.ChangeID, 12))
			}
			byCID[commit.GerritCID] = commit
		}
	}
	return commits, byCID, nil
}
