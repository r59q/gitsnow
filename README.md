# gitsnow

Local-first repository management, designed to connect issues, agent workers,
and code review without depending on a particular Git hosting platform.

**Current milestone:** Git-backed issue CRUD, a CLI, a localhost daemon, and a
responsive React UI. Agent execution, change requests, projects, comments,
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

No network synchronization happens automatically:

```sh
git push origin gitsnow/meta
```

Teammates can continue using the application branch without adopting gitsnow.
In a fresh clone that has fetched this branch, explicitly create its local
tracking branch before starting gitsnow:

```sh
git branch --track gitsnow/meta origin/gitsnow/meta
```

gitsnow rejects a missing local metadata branch when a fetched remote-tracking
one exists, rather than silently creating a competing empty history.
To bring in remote metadata changes, fetch them and explicitly fast-forward or
reconcile the metadata branch. Automatic multi-clone reconciliation is not
implemented; divergent histories must not be resolved with a force push.
Deletion removes the current record, but **does not erase Git history**.

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

- `internal/issues`: domain validation, Git persistence, SQLite indexing.
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

Issue bodies are limited to 1 MiB and request bodies to 2 MiB. Internal operation
errors are logged by the daemon; clients must reload to reconcile before retrying
because Git may have saved a change even if updating the index or delivering the
response failed. Credentials must never be stored in issue metadata.