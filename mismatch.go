// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strings"
)

// verifyPatchEquivalence validates that landed local commits have identical
// code changes and commit message descriptions to their upstream counterparts,
// while tolerating expected divergence in commit message trailers (e.g.
// Change-Id, Reviewed-on, Reviewed-by added by Gerrit on submit).
//
// TODO: Perhaps a more "jj way" to do this would be to go ahead and split the
// commits. Ideally we would find a way to use the upstream change's change ID
// for the diff reside so it gets marked as divergent. A downside of this would
// be that a simple "jj undo" wouldn't work because of how many steps are
// involved.
func verifyPatchEquivalence(out io.Writer, landed []commitInfo) error {
	var mismatches []mismatch
	// Invoke jj in parallel. Each invocation takes ~0.15 seconds, but only 1
	// CPU, so they parallelize very well and this can significantly speed up
	// processing a large stack.
	for c, res := range parApply(context.Background(), verifyOnePatch, slices.Values(landed), runtime.NumCPU()) {
		if res.err != nil {
			return res.err
		}
		if !res.mismatch {
			continue
		}

		// There's a meaningful mismatch. Let jj interdiff report the whole thing.
		m := mismatch{c, res.descOnly}
		m.print(out)
		mismatches = append(mismatches, m)
	}
	fmt.Fprintln(out)

	if len(mismatches) > 0 {
		return &mismatchError{mismatches}
	}
	return nil
}

type mismatchError struct {
	mismatches []mismatch
}

func (*mismatchError) Error() string {
	return "one or more landed changes differ from upstream"
}

type repairCommand struct {
	cmd    []string
	setVar string
}

// repairCommands returns the sequence of commands to split all mismatched changes.
func (e *mismatchError) repairCommands() []repairCommand {
	var all []repairCommand
	for i := range e.mismatches {
		all = append(all, e.mismatches[i].repairCommands()...)
	}
	return all
}

type mismatch struct {
	c        commitInfo
	descOnly bool // Code matches, only description doesn't match
}

func (m *mismatch) print(out io.Writer) {
	changeID := shortID(m.c.ChangeID, 12)
	commitID := shortID(m.c.CommitID, 12)
	upstreamCommitID := shortID(m.c.UpstreamCommitID, 12)

	fmt.Fprintf(out, "Mismatch between local and upstream (Gerrit %s):\n", shortID(m.c.GerritCID, 10))
	fmt.Fprintf(out, "    Local change:    %s %s\n", changeID, commitID)
	fmt.Fprintf(out, "    Upstream commit: %s\n", upstreamCommitID)
	cmd := exec.Command("jj", "--no-pager", "--ignore-working-copy", "interdiff", "--from", string(m.c.CommitID), "--to", string(m.c.UpstreamCommitID))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "command failed: %s\n", err)
	}
	fmt.Fprintln(out)
	if m.descOnly {
		fmt.Fprintln(out, "Hint: To discard local description changes, run:")
	} else {
		fmt.Fprintln(out, "Hint: To split this change into an upstream-matching commit and a residual commit, run:")
	}
	cmds := m.repairCommands()
	for i, cmd := range cmds {
		sep := " && \\"
		if i == len(cmds)-1 {
			sep = ""
		}
		fmt.Fprintf(out, "  %s%s\n", formatCommand(cmd), sep)
	}
}

func formatCommand(c repairCommand) string {
	var parts []string
	for _, arg := range c.cmd {
		// Just enough shell quoting for the commands produced by splitCommands.
		if arg == "" || strings.Contains(arg, " ") {
			parts = append(parts, `"`+arg+`"`)
		} else {
			parts = append(parts, arg)
		}
	}
	s := strings.Join(parts, " ")
	if c.setVar != "" {
		s = c.setVar + "=$(" + s + ")"
	}
	return s
}

// repairCommands returns a sequence of jj commands repair this mismatch. If
// it's just a description difference, it will simply return a command to make
// the description match in order to let the sync continue. Otherwise, it will
// return a command sequence to split this mismatched change into a change
// matching the upstream commit and a change containing the residual diff.
//
// The split sequence is not trivial. Let changeID be the change we want to
// split and upstreamCommitID be the git commit hash of the upstream commit. The
// command sequence performs the following steps:
//
//  1. Prepare:
//     a. Create an empty commit RESIDUE on top of changeID that will ultimately
//     hold the diff residue.
//     b. Cherry-pick upstreamCommitID onto changeID-, producing a sibling to
//     changeID.
//     c. Capture the duplicate's change ID as DUPREV by getting the change ID
//     that was just created from the oplog.
//  2. Split the change
//     a. Use jj restore to make changeID's tree match DUPREV. This includes
//     --restore-descendants, so it leaves the diff residue in changeID+.
//  3. Fix up commit messages
//     a. Empty out changeID's description to prepare for squash.
//     b. Squash DUPREV into changeID, which will have no effect on the tree,
//     but will take DUPREV's commit message because changeID's commit message
//     is now empty. This also has the effect of abandoning the temporary DUPREV
//     change.
//     c. Open an interactive editor to describe RESIDUE.
//
// The main advantage of this sequence is that the change corresponding to the
// upstream change retains the existing change ID (and the same derived Gerrit
// change IDq) and the residue winds up in a new change.
//
// Note: An alternative single-pipeline approach using `jj split --tool` is:
//
//	jj interdiff --from <commitID> --to <upstreamCommitID> --git | \
//	jj split -r <changeID> \
//	  --config 'merge-tools.apply.program="git"' \
//	  --config 'merge-tools.apply.edit-args=["apply", "--unsafe-paths", "--exclude=*JJ-COMMIT-DESCRIPTION*", "--directory", "$right"]' \
//	  --tool apply
//
// This feeds the interdiff directly to `git apply` operating on $right, turning
// <changeID> into the upstream tree (keeping its original change ID) while the
// residual diff becomes a child with a fresh change ID in a single command. We
// don't use this because it's a little too clever: it seems really weird to
// shell out to git-apply, and jj handles one-off merge tools really awkwardly.
//
// Yet another option would be to add support to jj-gerrit-sync itself to either
// do the split or act as a split tool. That could simplify things, but also
// makes it more "magic."
func (m *mismatch) repairCommands() []repairCommand {
	changeID := shortID(m.c.ChangeID, 12)
	upstreamCommitID := shortID(m.c.UpstreamCommitID, 12)

	if m.descOnly {
		return []repairCommand{
			{
				cmd:    []string{"jj", "log", "-r", upstreamCommitID, "--no-graph", "-T", "description"},
				setVar: "DESC",
			},
			{cmd: []string{"jj", "describe", "-r", changeID, "-m", "$DESC"}},
		}
	}

	return []repairCommand{
		{cmd: []string{"jj", "new", "-A", changeID}},
		{cmd: []string{"jj", "duplicate", upstreamCommitID, "--onto", changeID + "-"}},
		{
			cmd:    []string{"jj", "log", "-r", ":: ~ at_operation(@-, ::)", "--no-graph", "-T", "change_id"},
			setVar: "DUPREV",
		},
		{cmd: []string{"jj", "restore", "--from", "$DUPREV", "--into", changeID, "--restore-descendants"}},
		{cmd: []string{"jj", "describe", "-r", changeID, "-m", ""}},
		{cmd: []string{"jj", "squash", "--from", "$DUPREV", "--into", changeID}},
		{cmd: []string{"jj", "describe"}},
	}
}

type verifyResult struct {
	mismatch bool
	descOnly bool // Code matches, only description doesn't match

	err error
}

func verifyOnePatch(_ context.Context, c commitInfo) verifyResult {
	// Verify that code/file changes are identical.
	// We skip running full interdiff initially because commit message trailers
	// added by Gerrit almost always cause it to produce output in the common case.
	// Instead, --summary only reports file changes.
	codeSummary, err := runJJ("interdiff", "--from", string(c.CommitID), "--to", string(c.UpstreamCommitID), "--summary")
	if err != nil {
		return verifyResult{err: fmt.Errorf("running interdiff summary for %s: %w", c.ChangeID, err)}
	}
	if strings.TrimSpace(codeSummary) != "" {
		return verifyResult{mismatch: true}
	}

	// The code changes are identical. Fetch both commit descriptions in a
	// single jj log invocation to check for body divergence.
	descOut, err := runJJ("log", "-r", fmt.Sprintf("%s | %s", c.CommitID, c.UpstreamCommitID), "--no-graph", "-T", `json(self) ++ "\n"`)
	if err != nil {
		return verifyResult{err: fmt.Errorf("reading descriptions for %s: %w", c.ChangeID, err)}
	}

	descs := make(map[gitCommitHash]string)
	dec := json.NewDecoder(strings.NewReader(descOut))
	for {
		var commit struct {
			CommitID    gitCommitHash `json:"commit_id"`
			Description string        `json:"description"`
		}
		if err := dec.Decode(&commit); err != nil {
			if err == io.EOF {
				break
			}
			return verifyResult{err: fmt.Errorf("decoding commit description JSON: %w", err)}
		}
		descs[commit.CommitID] = commit.Description
	}

	localDesc := descs[c.CommitID]
	upstreamDesc := descs[c.UpstreamCommitID]

	if stripTrailers(localDesc) != stripTrailers(upstreamDesc) {
		return verifyResult{mismatch: true, descOnly: true}
	}
	return verifyResult{mismatch: false}
}

// stripTrailers removes the trailing Git trailer block from a commit description.
func stripTrailers(desc string) string {
	lines := strings.Split(strings.TrimRight(desc, "\r\n"), "\n")
	if len(lines) == 0 {
		return ""
	}

	trailerStart := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		if isTrailerLine(line) || ((strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && i < len(lines)-1) {
			trailerStart = i
		} else {
			break
		}
	}

	if trailerStart < len(lines) {
		if trailerStart > 0 && strings.TrimSpace(lines[trailerStart-1]) == "" {
			end := trailerStart - 1
			for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
				end--
			}
			return strings.TrimSpace(strings.Join(lines[:end], "\n"))
		} else if trailerStart == 0 {
			return ""
		}
	}

	return strings.TrimSpace(desc)
}

func isTrailerLine(line string) bool {
	colon := strings.IndexByte(line, ':')
	if colon <= 0 {
		return false
	}
	key := line[:colon]
	for _, ch := range key {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}
