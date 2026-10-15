export interface Issue {
  id: string;
  title: string;
  body: string;
  status: "open" | "closed";
  createdAt: string;
  updatedAt: string;
  revision: string;
}

export interface Repository {
  path: string;
  metadataRef: string;
  backend: string;
  sync?: SyncStatus;
}

export interface SyncStatus {
  state: "local_only" | "pending" | "syncing" | "synced" | "error" | "conflict";
  remote: string;
  message: string;
  conflicts?: string[];
  lastSuccess?: string;
  syncedRevision?: string;
}

export interface IssueInput {
  id?: string;
  title: string;
  body: string;
  status: Issue["status"];
  revision?: string;
}

export interface Snapshot {
  repository: Repository;
  issues: Issue[];
}

export class APIError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isIssue(value: unknown): value is Issue {
  return (
    isRecord(value) &&
    typeof value.id === "string" &&
    /^[a-f0-9]{32}$/.test(value.id) &&
    typeof value.title === "string" &&
    typeof value.body === "string" &&
    (value.status === "open" || value.status === "closed") &&
    typeof value.createdAt === "string" &&
    typeof value.updatedAt === "string" &&
    typeof value.revision === "string" &&
    /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(value.revision)
  );
}

function isRepository(value: unknown): value is Repository {
  return (
    isRecord(value) &&
    typeof value.path === "string" &&
    typeof value.metadataRef === "string" &&
    value.backend === "git" &&
    (value.sync === undefined || isSyncStatus(value.sync))
  );
}

function isSyncStatus(value: unknown): value is SyncStatus {
  return isRecord(value) &&
    typeof value.state === "string" &&
    ["local_only", "pending", "syncing", "synced", "error", "conflict"].includes(value.state) &&
    typeof value.remote === "string" &&
    typeof value.message === "string" &&
    (value.lastSuccess === undefined || typeof value.lastSuccess === "string") &&
    (value.syncedRevision === undefined || typeof value.syncedRevision === "string") &&
    (value.conflicts === undefined || (Array.isArray(value.conflicts) && value.conflicts.every((path) => typeof path === "string")));
}

async function request(path: string, options?: RequestInit): Promise<unknown> {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...options?.headers },
    signal: options?.signal
      ? AbortSignal.any([options.signal, AbortSignal.timeout(25_000)])
      : AbortSignal.timeout(25_000),
  });
  if (response.status === 204) return undefined;
  const body: unknown = await response.json();
  if (!response.ok) {
    const message =
      isRecord(body) && isRecord(body.error) && typeof body.error.message === "string"
        ? body.error.message
        : `Request failed (${response.status})`;
    throw new APIError(message, response.status);
  }
  return body;
}

export async function getRepository(signal: AbortSignal): Promise<Repository> {
  const value = await request("/api/repository", { signal });
  if (!isRepository(value)) throw new Error("The daemon returned invalid repository information");
  return value;
}

export async function requestSync(repositoryPath: string): Promise<SyncStatus> {
  const value = await request("/api/sync", {
    method: "POST", headers: { "X-Gitsnow-Repository": repositoryPath },
  });
  if (!isSyncStatus(value)) throw new Error("The daemon returned invalid sharing status");
  return value;
}

export async function listIssues(signal: AbortSignal, repositoryPath: string): Promise<Issue[]> {
  const value = await request("/api/issues", {
    signal, headers: { "X-Gitsnow-Repository": repositoryPath },
  });
  if (!isRecord(value) || !Array.isArray(value.issues) || !value.issues.every(isIssue)) {
    throw new Error("The daemon returned an invalid issue list");
  }
  return value.issues;
}

export async function saveIssue(input: IssueInput, repositoryPath: string, id?: string): Promise<Issue> {
  const value = await request(id ? `/api/issues/${id}` : "/api/issues", {
    method: id ? "PUT" : "POST",
    body: JSON.stringify(input),
    headers: { "X-Gitsnow-Repository": repositoryPath },
  });
  if (!isIssue(value)) throw new Error("The daemon returned an invalid saved issue; reload to reconcile");
  return value;
}

export async function deleteIssue(issue: Issue, repositoryPath: string): Promise<void> {
  await request(`/api/issues/${issue.id}`, {
    method: "DELETE",
    body: JSON.stringify({ revision: issue.revision }),
    headers: { "X-Gitsnow-Repository": repositoryPath },
  });
}

const cacheKey = "gitsnow:issues:v1";

export function readCache(): { snapshot?: Snapshot; warning?: string } {
  try {
    const raw = localStorage.getItem(cacheKey);
    if (!raw) return {};
    const value: unknown = JSON.parse(raw);
    if (
      !isRecord(value) ||
      !isRepository(value.repository) ||
      !Array.isArray(value.issues) ||
      !value.issues.every(isIssue)
    ) {
      return { warning: "The browser cache is invalid. Reload to fetch records from Git." };
    }
    return { snapshot: { repository: value.repository, issues: value.issues } };
  } catch (error) {
    return { warning: `Browser cache could not be read: ${errorMessage(error)}` };
  }
}

export function writeCache(snapshot: Snapshot): string | undefined {
  try {
    localStorage.setItem(cacheKey, JSON.stringify(snapshot));
  } catch (error) {
    return `Changes are saved locally, but browser caching is unavailable: ${errorMessage(error)}`;
  }
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
