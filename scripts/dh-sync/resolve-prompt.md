You are resolving an upstream sync in the DealHub fork of agent-orchestrator.
You are inside a git worktree on branch $AO_SYNC_BRANCH, cut from dh/main. A merge of
upstream/main ($AO_SYNC_UPSTREAM_SHA) either stopped on conflicts or merged cleanly and
then failed the checks.

Rules:
1. Read DH_FORK.md first. Keep every behaviour it lists as modified by the fork
   (the "Upstream files we modify" table), and adopt the upstream changes around it.
2. Never edit, delete, skip or weaken a test to make checks pass. Fix the code, or
   adapt the fork's change to the new upstream API.
3. Never push, never touch any remote, never switch branches, never run git merge --abort.
4. Resolve every conflict, then run scripts/dh-sync/checks.sh (or the same checks if the
   script is missing) until it is green. If you cannot get green honestly, stop and leave
   the tree as it is: the sync job will notify a human.
5. If a merge is in progress, finish it with a commit whose subject is
   "🔀 merge: sync upstream <short sha>" and a trailer line "Refs: $AO_SYNC_TICKET".
   If the merge was already committed, add a fix commit instead.
6. Write SYNC_SUMMARY.md in the worktree root (do not commit it): which files conflicted,
   how each was resolved and why, and anything a human should double-check.
