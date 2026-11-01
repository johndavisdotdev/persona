---
name: git
description: Use for any git work in a persona spawn (a microVM whose /workspace is on a branch named persona/<spawn>), such as committing, pushing, opening or updating a pull request, getting the latest main or another branch, rebasing or merging. In a spawn, git push, fetch and pull against origin fail because the VM has no credentials; persona-agent does them through the host instead.
---

# Git in a persona spawn

You are running in a persona spawn: a microVM with no GitHub or SSH credentials. Your repository in `/workspace`
is a private copy. Anything that crosses to the host or GitHub goes through `persona-agent`, which asks the
persona host to do it with the user's own credentials.

## How the workspace is set up

- `/workspace` is a clone holding **one branch of yours**, `persona/<spawn>` (check with
  `git branch --show-current`). It started from the host repository's `HEAD` when you were spawned. If an earlier
  spawn of the same name left that branch behind, it continues from that branch instead.
- The **pull request base** (the branch the host had checked out, usually `main`) is there too, read-only, as
  `host/<base>`.
- **No other branches** of the host are here until you fetch them (`git branch -a` shows only what's local).
- `origin` points at the real remote, but **`git push`, `git fetch`, `git pull` and `gh` fail** for lack of
  credentials. Don't try to work around this. Use the commands below.
- Only your own branch can ever be pushed. When the spawn stops, the host saves your branch and your uncommitted
  files to its worktree for this branch.

## Commands

| Command | What it does |
|---|---|
| `persona-agent fetch [BRANCH...]` | Copies host branches here as `host/BRANCH`, refreshed each time. With no argument it fetches the PR base. `BRANCH` is a host branch (`main`) or one of the host's remote-tracking branches (`origin/main`, as the host last fetched it). |
| `persona-agent push` | Pushes your committed branch to GitHub (`origin`). Only new commits on top are accepted. |
| `persona-agent push --force` | The same after you rewrote history (rebase, amend, squash). The host pushes with `--force-with-lease`. |
| `persona-agent pr --title "..." --body "..." [--draft] [--force]` | Pushes, then opens a pull request from your branch into the PR base. If one is already open, it pushes and prints that PR's URL. |

`push` and `pr` print JSON with the branch, the commit, the remote and (for `pr`) the PR URL. Each line of `fetch`
output is `host/BRANCH <commit> (host branch BRANCH)`.

## Workflows

**Bring your branch up to date with main (rebase):**

```sh
persona-agent fetch main          # or with no argument: the PR base
git rebase host/main              # resolve conflicts, rerun the tests
persona-agent push --force        # the rebase rewrote history
```

**The same with a merge** (no `--force` needed):

```sh
persona-agent fetch main
git merge host/main
persona-agent push
```

**Look at another branch**:

```sh
persona-agent fetch feature/login
git log --oneline host/feature/login
git diff HEAD...host/feature/login
```

`git show host/feature/login:path/to/file` reads a file from it without switching branches.

**Open a pull request**: commit, then:

```sh
persona-agent pr --title "Add retry to the importer" --body "Why, what changed, how it was tested."
```

**Update a pull request after review**: commit the fixes, then run `persona-agent push`. That updates the open PR.

## Rules

- **Commit first.** Only committed work is pushed. Uncommitted files reach the host only when the spawn stops.
- **Do your work on `persona/<spawn>`.** You may check out other branches to look around, but `push` and `pr` always
  send `persona/<spawn>`, whatever is checked out. Never rename or delete that branch.
- **Treat `host/*` as read-only.** `fetch` overwrites those refs every time, so never commit on them. To build on
  one, merge or rebase it into your branch.
- **Use `--force` only after rewriting history.** If a push fails with "has diverged ... push with --force", you
  rewrote history. Decide whether you meant to, then push with `--force`.
- **"Host" means the user's checkout.** `host/main` is the host repository's *local* `main` as it is now, which can
  lag behind GitHub. For GitHub's version as the host last fetched it, use `persona-agent fetch origin/main` (it
  arrives as `host/origin/main`). You can't make the host fetch from GitHub.
- **Report host-side errors.** If `push`, `pr` or `fetch` fails because of the host (no `origin` remote,
  authentication, `gh` not logged in, an unknown branch), tell the user what failed. You can't fix the host's
  credentials from inside the spawn.
