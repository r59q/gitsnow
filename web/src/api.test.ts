import { afterEach, expect, it, vi } from "vitest";
import { listIssues, readCache } from "./api";

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
