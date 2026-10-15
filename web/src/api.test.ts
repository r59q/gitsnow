import { afterEach, expect, it, vi } from "vitest";
import { getRepository, listIssues, readCache } from "./api";

afterEach(() => vi.unstubAllGlobals());

it("rejects malformed cached data visibly instead of treating it as authoritative", () => {
  localStorage.setItem("gitsnow:issues:v1", "{");
  expect(readCache().warning).toContain("could not be read");
  localStorage.setItem("gitsnow:issues:v1", JSON.stringify({ issues: [] }));
  expect(readCache().warning).toContain("invalid");
});

it("does not accept malformed issue responses", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ issues: [{ id: "invalid" }] }), { status: 200 })));
  await expect(listIssues(new AbortController().signal, "/work/repository")).rejects.toThrow("invalid issue list");
});

it("binds issue snapshots to the repository that was verified", async () => {
  const fetch = vi.fn().mockResolvedValue(new Response('{"issues":[]}', { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  expect(await listIssues(new AbortController().signal, "/work/repository")).toEqual([]);
  expect(fetch.mock.calls[0][1].headers).toMatchObject({ "X-Gitsnow-Repository": "/work/repository" });
});

it("rejects invalid sharing state from the daemon", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
    path: "/repo", metadataRef: "refs/heads/gitsnow/meta", backend: "git",
    sync: { state: "silently-shared", remote: "origin", message: "Not a valid state" },
  }), { status: 200 })));
  await expect(getRepository(new AbortController().signal)).rejects.toThrow("invalid repository information");
});
