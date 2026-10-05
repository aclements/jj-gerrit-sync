`jj-gerrit-sync` performs a Gerrit-aware rebase (and optionally fetch), it
resolving changes from the current stack that have already landed upstream.

Why not just `jj rebase -d target --skip-emptied`? When Gerrit lands a change,
it creates a new commit, causing jj to lose track of the relationship between
the local and landed change. While `jj rebase` will often rebase a landed change
away as empty, it only compares against the final tree state of target. If later
commits upstream touched the same files, rebasing directly onto the target
change will result in false conflicts rather than an empty commit.

`jj-gerrit-sync`, on the other hand, matches up local and upstream changes by
Gerrit Change-Id, verifies that local changes actually match upstream, and
performs the Gerrit-aware rebase. If a local change doesn't match upstream, it
will show how it differs and offer an appropriate command to address it.
