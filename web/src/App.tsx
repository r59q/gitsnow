import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent, KeyboardEvent } from "react";
import {
  APIError,
  deleteIssue,
  errorMessage,
  getRepository,
  listIssues,
  readCache,
  requestSync,
  saveIssue,
  writeCache,
} from "./api";
import type { Issue, IssueInput, Repository } from "./api";

type Editor = { issue?: Issue; newID?: string; repositoryPath: string };

export function App() {
  const [cache] = useState(readCache);
  const [repository, setRepository] = useState<Repository | undefined>(cache.snapshot?.repository);
  const [issues, setIssues] = useState<Issue[]>(cache.snapshot?.issues ?? []);
  const [warning, setWarning] = useState(cache.warning);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [ready, setReady] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [retryingSync, setRetryingSync] = useState(false);
  const [syncError, setSyncError] = useState<string>();
  const [status, setStatus] = useState<"all" | Issue["status"]>("open");
  const [search, setSearch] = useState("");
  const [editor, setEditor] = useState<Editor>();
  const mutation = useRef(false);
  const generation = useRef(0);
  const fetchController = useRef<AbortController | undefined>(undefined);

  const persist = useCallback((repo: Repository, records: Issue[]) => {
    const message = writeCache({ repository: repo, issues: records });
    if (message) setWarning(message);
  }, []);

  const refresh = useCallback(async () => {
    if (mutation.current) return;
    if (fetchController.current && !fetchController.current.signal.aborted) return;
    const controller = new AbortController();
    fetchController.current = controller;
    const current = ++generation.current;
    setRefreshing(true);
    try {
      const repo = await getRepository(controller.signal);
      const records = await listIssues(controller.signal, repo.path);
      if (current !== generation.current) return;
      setRepository(repo);
      setIssues(records);
      setReady(true);
      setError(undefined);
      setSyncError(undefined);
      persist(repo, records);
    } catch (error) {
      if (current !== generation.current || controller.signal.aborted) return;
      setReady(false);
      setError(`Cannot synchronize with the local daemon: ${errorMessage(error)}`);
    } finally {
      if (current === generation.current) setRefreshing(false);
      if (fetchController.current === controller) fetchController.current = undefined;
    }
  }, [persist]);

  useEffect(() => {
    void refresh();
    const onFocus = () => { void refresh(); };
    window.addEventListener("focus", onFocus);
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void refresh();
    }, 15_000);
    return () => {
      ++generation.current;
      fetchController.current?.abort();
      window.removeEventListener("focus", onFocus);
      window.clearInterval(timer);
    };
  }, [refresh]);

  useEffect(() => {
    if (!ready || busy || !repository?.sync || !["pending", "syncing"].includes(repository.sync.state)) return;
    const timer = window.setTimeout(() => { void refresh(); }, 1000);
    return () => window.clearTimeout(timer);
  }, [repository?.sync, ready, busy, refresh]);

  async function mutate(optimistic: Issue[], id: string, operation: () => Promise<Issue[]>, message: string) {
    if (mutation.current || !ready || !repository) return false;
    const previous = issues;
    mutation.current = true;
    ++generation.current;
    fetchController.current?.abort();
    setRefreshing(false);
    setBusy(id);
    setIssues(optimistic);
    setError(undefined);
    setNotice(undefined);
    try {
      const records = await operation();
      setIssues(records);
      let savedRepository = repository;
      if (repository.sync?.remote && !["error", "conflict"].includes(repository.sync.state)) {
        savedRepository = {
          ...repository,
          sync: {
            ...repository.sync,
            state: "pending",
            message: "Saved locally; waiting to share with " + repository.sync.remote,
          },
        };
      }
      setRepository(savedRepository);
      persist(savedRepository, records);
      setNotice(message);
      return true;
    } catch (error) {
      setIssues(previous);
      const reconcile = !(error instanceof APIError && (error.status === 400 || error.status === 413));
      setReady(!reconcile);
      setError(`${errorMessage(error)}. Your draft is preserved.${reconcile ? " Reload to reconcile before retrying." : ""}`);
      return false;
    } finally {
      mutation.current = false;
      setBusy(undefined);
    }
  }

  async function save(input: IssueInput) {
    if (!editor || editor.repositoryPath !== repository?.path) return;
    const id = editor.issue?.id ?? editor.newID;
    if (!id) throw new Error("Missing issue ID");
    const now = new Date().toISOString();
    const optimistic: Issue = {
      id, title: input.title.trim(), body: input.body, status: input.status,
      createdAt: editor.issue?.createdAt ?? now, updatedAt: now,
      revision: editor.issue?.revision ?? "",
    };
    const records = editor.issue
      ? issues.map((issue) => issue.id === id ? optimistic : issue)
      : [optimistic, ...issues];
    const succeeded = await mutate(records, id, async () => {
      const saved = await saveIssue({ ...input, ...(editor.issue ? {} : { id }) }, editor.repositoryPath, editor.issue?.id);
      return records.map((issue) => issue.id === id ? saved : issue);
    }, "Issue saved locally." + (repository?.sync?.remote ? " Remote sharing is tracked below." : ""));
    if (succeeded) setEditor(undefined);
  }

  async function remove(issue: Issue) {
    if (!editor || editor.repositoryPath !== repository?.path) return;
    const records = issues.filter((record) => record.id !== issue.id);
    const succeeded = await mutate(records, issue.id, async () => {
      await deleteIssue(issue, editor.repositoryPath);
      return records;
    }, "Issue deleted. Its history remains in Git.");
    if (succeeded) setEditor(undefined);
  }

  function openEditor(issue?: Issue) {
    if (!repository) return;
    setNotice(undefined);
    setEditor({
      issue,
      newID: issue ? undefined : crypto.randomUUID().replaceAll("-", ""),
      repositoryPath: repository.path,
    });
  }

  async function retrySync() {
    if (!repository || !ready) return;
    const path = repository.path;
    setRetryingSync(true);
    setSyncError(undefined);
    try {
      const sync = await requestSync(path);
      setRepository((current) => current?.path === path ? { ...current, sync } : current);
    } catch (error) {
      setSyncError(`Could not request sharing: ${errorMessage(error)}`);
    } finally {
      setRetryingSync(false);
    }
  }

  const query = search.trim().toLocaleLowerCase();
  const visible = issues.filter((issue) =>
    (status === "all" || issue.status === status) &&
    `${issue.title}\n${issue.body}`.toLocaleLowerCase().includes(query),
  );
  const repoName = repository?.path.split("/").filter(Boolean).at(-1);
  const openCount = issues.filter((issue) => issue.status === "open").length;
  const sync = repository?.sync;
  const sharingBlocked = sync?.state === "error" || sync?.state === "conflict";

  return (
    <div className="app">
      <header className="topbar">
        <a className="wordmark" href="/">git<span>snow</span><span className="alpha">early access</span></a>
        <span className={`connection ${ready && !sharingBlocked ? "connected" : ""}`}>
          <span className="connection-dot" />
          {busy ? "Saving locally" : !ready ? "Unverified / cached" :
            sync?.state === "synced" ? `Shared with ${sync.remote}` :
              sync?.state === "syncing" ? "Sharing metadata" :
                sync?.state === "pending" ? "Sharing pending" :
                  sharingBlocked ? "Saved locally / not shared" : "Local Git backend"}
        </span>
      </header>

      <main className="workspace">
        <div className="repository-label">
          <span className="repo-icon" aria-hidden="true">/</span>
          <span title={repository?.path}>{repoName ?? "Connecting to repository"}</span>
          <span className="backend-tag">Git-backed</span>
        </div>
        <div className="page-heading">
          <div>
            <p className="eyebrow">Your repository. Your workflow.</p>
            <h1>Issues</h1>
            <p className="subtitle">Turn an idea into work, without leaving your repository.</p>
          </div>
          <button className="primary" disabled={!ready || !!busy} onClick={() => openEditor()}>+ New issue</button>
        </div>

        <div className="messages" aria-live="polite">
          {sync && <div className={`message ${sharingBlocked ? "warning" : sync.state === "synced" ? "success" : "sharing"}`} role="status">
            <div>
              <strong>{!ready ? "Last known sharing status" : sync.state === "local_only" ? "Local only" :
                sync.state === "synced" ? `Shared with ${sync.remote}` : "Saved locally / sharing pending"}</strong>
              <div>{sync.message}</div>
              {sync.conflicts?.length ? <div className="conflict-paths">{sync.conflicts.join(", ")}</div> : null}
            </div>
            {sync.remote && <button disabled={!ready || !!busy || retryingSync || sync.state === "syncing"} onClick={() => { void retrySync(); }}>
              {retryingSync ? "Requesting..." : "Retry sharing"}
            </button>}
          </div>}
          {syncError && <div className="message error" role="alert">{syncError}</div>}
          {error && <div className="message error" role="alert">
            <span>{error}</span>
            <button disabled={!!busy || refreshing} onClick={() => { void refresh(); }}>Reload</button>
          </div>}
          {warning && <div className="message warning">{warning}</div>}
          {notice && <div className="message success">{notice}</div>}
        </div>

        <section className="issue-board" aria-label="Repository issues">
          <div className="board-toolbar">
            <div className="filters" aria-label="Filter issues">
              {(["open", "closed", "all"] as const).map((value) => (
                <button key={value} aria-pressed={status === value} onClick={() => setStatus(value)}>
                  {value === "open" ? "Open" : value === "closed" ? "Closed" : "All"}
                  <span className="count">{value === "open" ? openCount : value === "closed" ? issues.length - openCount : issues.length}</span>
                </button>
              ))}
            </div>
            <div className="search-group">
              <input type="search" aria-label="Search issues" placeholder="Search issues..." value={search} onChange={(event) => setSearch(event.target.value)} />
              <button className="refresh-button" aria-label="Refresh issues" disabled={!!busy || refreshing} onClick={() => { void refresh(); }}>Refresh</button>
            </div>
          </div>

          {visible.length > 0 ? (
            <ul className="issue-list">
              {visible.map((issue) => (
                <li key={issue.id}>
                  <button className="issue-row" onClick={() => openEditor(issue)} disabled={!!busy}>
                    <span className={`issue-indicator ${issue.status}`} aria-hidden="true">{issue.status === "closed" ? "x" : ""}</span>
                    <span className="issue-summary">
                      <span className="issue-title">{issue.title}</span>
                      <span className="issue-meta">{issue.id.slice(0, 8)} · {issue.status} · {new Date(issue.createdAt).toLocaleDateString()}</span>
                    </span>
                    <span className={`status-badge ${issue.status}`}>{busy === issue.id ? "Pending" : issue.status}</span>
                  </button>
                </li>
              ))}
            </ul>
          ) : (
            <div className="empty-state">
              <div className="empty-icon" aria-hidden="true">+</div>
              <h2>{search || status !== "open" ? "No matching issues" : "A clean slate"}</h2>
              <p>{search ? "Try another search or filter." : "Create an issue with a clear goal and acceptance criteria."}</p>
              {status === "open" && !search && <button className="secondary" disabled={!ready || !!busy} onClick={() => openEditor()}>Create your first issue</button>}
            </div>
          )}
          <footer className="board-footer">
            <span>{visible.length} {visible.length === 1 ? "issue" : "issues"}</span>
            <span>{ready ? sync?.state === "synced" ? `Shared on ${sync.remote}` : "Saved locally on gitsnow/meta" : "Cached records are read-only until synchronized"}</span>
          </footer>
        </section>

        <p className="foundation-note">Git-backed issues are the first building block. Agent runs, reviews, and forge integrations are next.</p>
      </main>

      {editor && (
        <IssueEditor
          key={editor.issue?.id ?? editor.newID}
          issue={editor.issue}
          latestIssue={issues.find((issue) => issue.id === editor.issue?.id)}
          saving={!!busy}
          writable={ready && editor.repositoryPath === repository?.path && (!editor.issue || issues.some((issue) => issue.id === editor.issue?.id))}
          error={editor.repositoryPath !== repository?.path
            ? "This draft belongs to a different repository. Close it before creating or editing issues here."
            : editor.issue && ready && !issues.some((issue) => issue.id === editor.issue?.id)
              ? "This issue was deleted. Your draft is still available to copy."
              : error}
          onSave={save}
          onDelete={remove}
          onReload={() => { void refresh(); }}
          refreshing={refreshing}
          onClose={() => { if (!busy) setEditor(undefined); }}
        />
      )}
    </div>
  );
}

interface EditorProps {
  issue?: Issue;
  latestIssue?: Issue;
  saving: boolean;
  writable: boolean;
  refreshing: boolean;
  error?: string;
  onSave: (input: IssueInput) => Promise<void>;
  onDelete: (issue: Issue) => Promise<void>;
  onReload: () => void;
  onClose: () => void;
}

function IssueEditor({ issue, latestIssue, saving, writable, refreshing, error, onSave, onDelete, onReload, onClose }: EditorProps) {
  const [title, setTitle] = useState(issue?.title ?? "");
  const [body, setBody] = useState(issue?.body ?? "");
  const [status, setStatus] = useState<Issue["status"]>(issue?.status ?? "open");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [revision, setRevision] = useState(issue?.revision);
  const titleLength = Array.from(title).length;
  const panel = useRef<HTMLElement>(null);
  const titleInput = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const previous = document.activeElement;
    titleInput.current?.focus();
    return () => { if (previous instanceof HTMLElement) previous.focus(); };
  }, []);

  function keyboard(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape" && !saving) onClose();
    if (event.key !== "Tab") return;
    const items = panel.current?.querySelectorAll<HTMLElement>("button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled)");
    if (!items?.length) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    void onSave({ title, body, status, revision });
  }

  return (
    <div className="editor-backdrop">
      <section ref={panel} className="editor" role="dialog" aria-modal="true" aria-labelledby="editor-heading" onKeyDown={keyboard}>
        <div className="editor-header">
          <div>
            <p className="eyebrow">{issue ? issue.id.slice(0, 8) : "Start something"}</p>
            <h2 id="editor-heading">{issue ? "Edit issue" : "New issue"}</h2>
          </div>
          <button className="close-button" aria-label="Close editor" disabled={saving} onClick={onClose}>x</button>
        </div>
        <form onSubmit={submit}>
          <label htmlFor="issue-title">Title <span className="label-hint">{titleLength}/200</span></label>
          <input ref={titleInput} id="issue-title" required value={title} disabled={saving} onChange={(event) => setTitle(event.target.value)} placeholder="What needs to change?" />

          <label htmlFor="issue-body">Description <span className="label-hint">Markdown</span></label>
          <textarea id="issue-body" rows={10} value={body} disabled={saving} onChange={(event) => setBody(event.target.value)} placeholder={"Describe the goal.\n\n## Acceptance criteria\n- What does done look like?"} />

          <label htmlFor="issue-status">Status</label>
          <select id="issue-status" value={status} disabled={saving} onChange={(event) => {
            const value = event.target.value;
            if (value === "open" || value === "closed") setStatus(value);
          }}>
            <option value="open">Open</option>
            <option value="closed">Closed</option>
          </select>

          {error && <div className="message error" role="alert">
            <span>{error}</span>
            <button type="button" disabled={saving || refreshing} onClick={onReload}>Reload</button>
          </div>}
          {latestIssue && !saving && latestIssue.revision !== revision && <div className="revision-conflict">
            <p>The saved issue changed. Review it before applying your draft.</p>
            <details>
              <summary>Latest saved version</summary>
              <strong>{latestIssue.title}</strong>
              <p>Status: {latestIssue.status}</p>
              <pre>{latestIssue.body}</pre>
            </details>
            <button type="button" className="secondary" disabled={!writable} onClick={() => setRevision(latestIssue.revision)}>Apply draft to latest revision</button>
          </div>}
          <p className="editor-note">{saving ? "Pending: writing to the local Git metadata branch..." : "Saved locally as Markdown on gitsnow/meta. Remote sharing is separate; your application files stay untouched."}</p>
          <div className="editor-actions">
            {issue && <button type="button" className="danger-link" disabled={saving || !writable} onClick={() => setConfirmDelete(true)}>Delete issue</button>}
            <button type="button" className="secondary" disabled={saving} onClick={onClose}>Cancel</button>
            <button className="primary" disabled={saving || !writable || !title.trim() || titleLength > 200 || (!!latestIssue && latestIssue.revision !== revision)}>{saving ? "Saving..." : "Save issue"}</button>
          </div>
        </form>
        {confirmDelete && issue && <div className="delete-confirmation" role="alert">
          <p>Delete this issue? The record will be removed, but Git history will retain it.</p>
          <button className="danger" disabled={saving || !writable} onClick={() => { void onDelete({ ...issue, revision: revision ?? issue.revision }); }}>Confirm delete</button>
          <button className="secondary" disabled={saving} onClick={() => setConfirmDelete(false)}>Cancel deletion</button>
        </div>}
      </section>
    </div>
  );
}
