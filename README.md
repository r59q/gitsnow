# gitsnow

Local-first repository management, designed to connect issues, agent workers,
and code review without depending on a particular Git hosting platform.

**Current milestone:** Git-backed issue CRUD, automatic metadata sharing, a CLI,
a localhost daemon, and a responsive React UI. Agent execution, change requests, projects, comments,
workflows, forge adapters, and authenticated remote hosting are **not implemented
yet**.

## Quick start

Requirements: Go 1.26+, Node.js 24+, npm, and Git 2.46+.

```sh
npm --prefix web ci
make build
./gitsnow serve --repo /path/to/your/repository
```

Open **http://127.0.0.1:7331**. The binary embeds the production UI; Node.js is
only required at build time. The repository can be newly initialized and have
no application commits.

If the repository has an **`origin` remote**, gitsnow automatically shares its
metadata branch using your existing Git credentials. Co-workers can run gitsnow
against their own clones to receive issues without creating or pushing a metadata
branch themselves. Only `gitsnow/meta` is published, never application branches
or tags. Repositories without `origin` stay local-only.

To opt out before starting the daemon:

```sh
./gitsnow sync --repo /path/to/repo --remote off
```

The daemon binds to loopback only. Do not expose it through a public reverse
proxy: authentication and remote-hosting authorization are not implemented.
For access from another trusted computer, use an SSH port forward instead.
Responsive layouts are present, but the complete phone-to-runner workflow and
a hosted mobile service are future milestones.

### CLI

Issue commands operate directly on Git, without a running daemon, and return JSON:

```sh
./gitsnow issue create --repo /path/to/repo \
  --title "Review changes from a phone" \
  --body "Acceptance criteria: readable diffs and explicit merge approval."

./gitsnow issue list --repo /path/to/repo --status open --search phone
./gitsnow issue show --repo /path/to/repo ISSUE_ID
./gitsnow issue update --repo /path/to/repo --status closed ISSUE_ID
./gitsnow issue delete --repo /path/to/repo --revision REVISION ISSUE_ID
```

Reads attempt to fetch shared metadata first; writes save locally and then
attempt to share. A failed write synchronization returns the saved record on
stdout and a nonzero exit with an explicit **saved locally, but not shared**
message. Retain its ID before retrying creation. Reads remain available offline,
with a warning on stderr that local records are being shown.

Place flags before the issue ID. `show` returns the full ID and revision.
Updates preserve unspecified fields; `--body ""` clears the description.
An update uses the revision read at command start unless `--revision` is
specified. Deletion always requires an explicit revision.

`create --id` accepts a 32-character lowercase hexadecimal ID. Retrying creation
with the same ID and content returns the existing issue; different content is a
conflict. This allows clients to reconcile an ambiguous network response without
creating duplicates.

## Storage and synchronization

Issues are authoritative Markdown records on **`gitsnow/meta`**, a separate,
initially parentless Git branch. Updates use Git object plumbing and atomic
compare-and-swap ref updates. They do not check out the metadata branch, touch
the application index, change `HEAD`, or require a configured commit identity.
Metadata commits currently use `gitsnow <gitsnow@localhost>` as their identity;
they are history, not authenticated author attribution.

```text
gitsnow/meta branch
  issues/<id>.md

Git common directory (local, untracked)
  gitsnow/index.sqlite
```

Each Markdown file has JSON frontmatter between `---` delimiters, followed by
the description. JSON frontmatter contains the ID, title, status, and creation
and update timestamps; the body follows the header.
The API revision is the blob's Git object ID and is not stored inside the file.
Files can be edited in a separate metadata worktree; IDs, filenames, statuses,
and timestamps must remain valid. Invalid metadata is reported rather than
silently skipped.

SQLite is a rebuildable read model, not a second source of truth. Reads check
the metadata ref and rebuild the index when external changes are detected.
Local writes update the read model incrementally. Separate CLI and daemon
processes share the indexed ref, and conflicting writes return errors rather
than overwrite one another.

### Automatic sharing

The daemon synchronizes at startup, immediately after local mutations, and every
15 seconds to import co-workers' changes and retry unpublished data. HTTP issue
writes do **not** wait for the network. A slow, offline, unauthenticated, or
read-only remote does not prevent saving to local Git. The metadata branch itself
is the durable queue: pending commits are rediscovered on restart.

The UI distinguishes **local only**, **sharing pending**, **sharing**, **shared**,
**sharing failed**, and **conflicting edits**. "Saved locally" never means a
co-worker can already see the issue. Use **Retry sharing** to request another
attempt immediately. The UI refreshes pending status quickly and periodically
refreshes issue records; incoming remote changes are eventually visible, not
instantaneous notifications.

Synchronization fetches only the metadata branch, validates incoming records,
fast-forwards where possible, and creates a two-parent metadata merge for
independent file changes. Separate initial histories are supported. A remote
push is an ordinary fast-forward push of an explicit commit snapshot; a racing
push is rejected and retried after fetching again. gitsnow never force-pushes
over a co-worker's history. Application `HEAD`, staged files, working trees, and
`FETCH_HEAD` are left alone.

Teammates can browse `gitsnow/meta` and `issues/<id>.md` in the forge's branch/file
browser without installing gitsnow. These records do **not** appear in the forge's
native Issues tab; that requires a future forge issue backend.

### Configuration and manual retry

The default synchronization remote is `origin` when it exists. Configuration
is private to the clone, stored as `gitsnow.syncRemote` in local Git config.
Changing it does not impose a setting on teammates.

```sh
# Select an existing remote and share pending records now.
./gitsnow sync --repo /path/to/repo --remote team

# Disable automatic sharing and network access.
./gitsnow sync --repo /path/to/repo --remote off

# Re-enable automatic sharing through origin.
./gitsnow sync --repo /path/to/repo --remote origin

# Retry immediately and print sharing status as JSON.
./gitsnow sync --repo /path/to/repo

# Customize daemon polling.
./gitsnow serve --repo /path/to/repo --sync-interval 30s
```

The minimum poll interval is 1 second; the default is 15 seconds. Each network
attempt is bounded to 30 seconds and retries continue only while the daemon
runs. One-shot CLI commands attempt synchronization once. Disabling sharing
does not delete records already published; re-enabling it will share all pending
local metadata.

Use an existing credential helper or SSH agent. gitsnow does not offer a Git
login flow, and background Git commands do not prompt for credentials. SSH uses
batch mode and a connection timeout by default; an explicit `GIT_SSH_COMMAND`
is honored. The selected remote must have one identical effective fetch and
push URL, to avoid importing from one repository and silently publishing to
another or to multiple destinations. Configure a dedicated remote if needed.

### Concurrent edits

Different issue files merge automatically. If both clones change the **same
record**, or one edits a record while another deletes it, sharing stops with an
explicit conflict and the affected paths. This is record-level reconciliation,
not line-based Markdown merging or last-write-wins. The local branch and remote
branch are unchanged; an incoming conflict snapshot is also retained locally
under `refs/gitsnow/conflicts/<commit>`.

There is not yet an in-app cross-clone conflict resolver. Reconcile in a separate
metadata worktree, preserving the application checkout. Managed writes and
synchronization are refused while that branch is checked out, so the daemon
cannot move it underneath a manual edit:

```sh
./gitsnow sync --repo /path/to/repo --remote off
git -C /path/to/repo fetch origin
git -C /path/to/repo worktree add /path/to/metadata-worktree gitsnow/meta
git -C /path/to/metadata-worktree merge --no-commit --allow-unrelated-histories origin/gitsnow/meta
# Review both versions, resolve the conflicting records, git add, and git commit.
git -C /path/to/repo worktree remove /path/to/metadata-worktree
./gitsnow sync --repo /path/to/repo --remote origin
```

Use the selected remote instead of `origin` when configured differently. Never
force-push to resolve this. Deletion removes the current record, but **does not
erase Git history**.

The browser caches confirmed issue records in local storage, scoped to the last
repository served at that origin. Cached records can be read while disconnected,
but writes require successful synchronization. Optimistic changes are shown as
pending, confirmed only after Git saves, and rolled back on failure. Drafts stay
in the open editor, not across browser reloads. Conflict recovery requires
reviewing the latest saved version and explicitly applying the draft to its
revision. Browser storage contains issue text: clear site data on shared devices.

## Architecture

```text
CLI ---------------------+
                         |
React UI -> HTTP API -> issue domain -> Git metadata backend
                              |
                       SQLite read model
```

The implementation starts as a Go modular monolith:

- `internal/issues`: domain validation, Git persistence, SQLite indexing, and
  background metadata synchronization with durable sharing status.
- `internal/server`: bounded JSON API and localhost/same-origin protections.
- `cmd/gitsnow`: direct CLI operations and daemon lifecycle.
- `web`: responsive React/TypeScript UI, optimistic operations, browser cache.
- `internal/web`: build-time embedded frontend assets.

Only the Git issue backend exists today. As the workflow grows, resource backends
and agent runtimes must remain independent: selecting GitHub issues must not
choose an agent provider, and choosing Copilot must not require GitHub issues.
Each resource family will have one authoritative backend, selected explicitly;
Git is the default. Capability differences and backend migrations must be
explicit, rather than hidden behind automatic bidirectional mirroring.

The next vertical slice is a durable run queue, owner-controlled runner, isolated
workspace, and one subscription-authenticated runtime adapter, followed by native
change requests, revision-bound review, validation, and safe merge. Copilot CLI,
Claude Code, and Codex CLI are the intended runtimes. gitsnow will orchestrate
unmodified provider CLIs, not implement inference API clients or pool
subscriptions. Native login stays on the runner. Provider policies, headless
capabilities, quotas, and supported authentication modes must be verified per
adapter; no silent fallback to paid API usage.

Initial performance budgets are under 100 ms for visible interaction feedback,
under 200 ms for a warm cached issue list, and p95 under 50 ms for indexed
queries. These are **targets, not measured guarantees**. Current reads load the
issue collection; pagination and incremental client synchronization are needed
before large-repository support is claimed.

## Development

```sh
npm --prefix web ci
make test
make build
```

`make test` type-checks and builds the UI, runs UI tests, runs Go tests with the
race detector, and runs `go vet`. Build the frontend before compiling Go packages
that import the embedded assets. Generated assets and the binary are ignored.

For frontend development, run the daemon in one terminal, then:

```sh
npm --prefix web run dev
```

Vite binds to `127.0.0.1` and proxies `/api` to the daemon on port 7331. Both
processes remain local. CI exercises the same build and test path.

To measure warm indexed reads over 1,000 Git-backed issue records:

```sh
go test ./internal/issues -run '^$' -bench BenchmarkCachedList1000 -benchtime=100x
```

## HTTP API

| Method | Path | Result |
|--------|------|--------|
| GET | `/api/repository` | Repository path, metadata ref, backend |
| POST | `/api/sync` | Request background sharing; HTTP 202 with sharing status |
| GET | `/api/issues?status=open&q=text` | `{ "issues": [...] }` |
| POST | `/api/issues` | Create `{ id?, title, body, status? }` |
| GET | `/api/issues/{id}` | Read an issue |
| PUT | `/api/issues/{id}` | Replace `{ title, body, status, revision }` |
| DELETE | `/api/issues/{id}` | Delete with `{ revision }` |

Writes require `Content-Type: application/json`. Unknown fields, invalid
statuses, empty titles, oversized bodies, and stale revisions are rejected.
Send `X-Gitsnow-Repository` with the path returned by `/api/repository` to reject
requests if the daemon has switched repositories. The browser sends this header
for issue reads and writes. Errors have the shape
`{ "error": { "code": "...", "message": "..." } }`; conflicts return HTTP 409.

`/api/repository` includes a `sync` object with `state`, `remote`, `message`,
optional `lastSuccess` and `syncedRevision`, and conflict paths. It reads local
status only and never waits for a fetch or push. A remote synchronization error
does not turn a successfully persisted HTTP issue mutation into a failed save;
clients must check sharing status separately.

Issue bodies are limited to 1 MiB and request bodies to 2 MiB. Internal operation
errors are logged by the daemon; clients must reload to reconcile before retrying
because Git may have saved a change even if updating the index or delivering the
response failed. Credentials must never be stored in issue metadata.