import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { isRecord, readCache, writeCache } from "./api";
import type { Issue, Repository } from "./api";

const repository: Repository = { path: "/work/example", metadataRef: "refs/heads/gitsnow/meta", backend: "git" };
const issue: Issue = {
  id: "a".repeat(32), title: "An existing issue", body: "Acceptance criteria",
  status: "open", revision: "b".repeat(40),
  createdAt: "2026-01-01T10:00:00Z", updatedAt: "2026-01-01T10:00:00Z",
};

function response(value: unknown, status = 200) {
  return new Response(status === 204 ? null : JSON.stringify(value), {
    status, headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => vi.unstubAllGlobals());

describe("issue workflow", () => {
  it("renders the repository and filters cached issue records", async () => {
    const fetch = vi.fn().mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [issue] }));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    expect(await screen.findByText(issue.title)).toBeInTheDocument();
    expect(screen.getByText("example")).toBeInTheDocument();
    fireEvent.change(screen.getByRole("searchbox"), { target: { value: "absent" } });
    expect(screen.getByText("No matching issues")).toBeInTheDocument();
    expect(screen.queryByText(issue.title)).not.toBeInTheDocument();
  });

  it("creates an optimistic issue, then marks it saved only after Git confirms", async () => {
    const user = userEvent.setup();
    let confirm: ((value: Response) => void) | undefined;
    const fetch = vi.fn().mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [] }))
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { confirm = resolve; }));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await waitFor(() => expect(screen.getByRole("button", { name: "+ New issue" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "+ New issue" }));
    const dialog = screen.getByRole("dialog");
    await user.type(within(dialog).getByLabelText(/^Title/), "Mobile workflow");
    await user.type(within(dialog).getByLabelText(/Description/), "Works from a phone.");
    await user.click(within(dialog).getByRole("button", { name: "Save issue" }));
    expect(screen.getByText("Mobile workflow")).toBeInTheDocument();
    expect(screen.getByText("Pending")).toBeInTheDocument();
    expect(screen.queryByText("Issue saved locally.")).not.toBeInTheDocument();
    const call = fetch.mock.calls[2];
    const submitted: unknown = JSON.parse(call[1].body);
    expect(submitted).toMatchObject({ title: "Mobile workflow", status: "open" });
    if (!isRecord(submitted)) throw new Error("Invalid create payload");
    confirm?.(response({ ...issue, ...submitted }, 201));
    expect(await screen.findByText("Issue saved locally.")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByText("Pending")).not.toBeInTheDocument();
  });

  it("rolls back a rejected edit and preserves the draft", async () => {
    const user = userEvent.setup();
    const fetch = vi.fn().mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [issue] }))
      .mockResolvedValueOnce(response({ error: { message: "issue changed; reload before trying again" } }, 409));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await user.click(await screen.findByRole("button", { name: /An existing issue/ }));
    const title = within(screen.getByRole("dialog")).getByLabelText(/^Title/);
    await user.clear(title);
    await user.type(title, "My draft");
    await user.click(screen.getByRole("button", { name: "Save issue" }));
    await waitFor(() => expect(screen.getAllByRole("alert")[0]).toHaveTextContent("issue changed"));
    expect(screen.getByText("An existing issue")).toBeInTheDocument();
    expect(title).toHaveValue("My draft");
    expect(screen.getByRole("button", { name: "Save issue" })).toBeDisabled();
  });

  it("uses the reviewed revision when deleting", async () => {
    const user = userEvent.setup();
    const fetch = vi.fn().mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [issue] }))
      .mockResolvedValueOnce(response(undefined, 204));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await user.click(await screen.findByRole("button", { name: /An existing issue/ }));
    await user.click(screen.getByRole("button", { name: "Delete issue" }));
    await user.click(screen.getByRole("button", { name: "Confirm delete" }));
    expect(await screen.findByText("Issue deleted. Its history remains in Git.")).toBeInTheDocument();
    expect(fetch.mock.calls[2][1]).toMatchObject({ method: "DELETE", body: JSON.stringify({ revision: issue.revision }) });
    expect(screen.queryByText(issue.title)).not.toBeInTheDocument();
  });

  it("shows cached records offline without permitting mutations", async () => {
    writeCache({ repository, issues: [issue] });
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Network unavailable")));
    render(<App />);
    expect(screen.getByText(issue.title)).toBeInTheDocument();
    expect(await screen.findByRole("alert")).toHaveTextContent("Network unavailable");
    expect(screen.getByRole("button", { name: "+ New issue" })).toBeDisabled();
    expect(screen.getByText("Cached records are read-only until synchronized")).toBeInTheDocument();
  });

  it("does not write an old repository's draft after the daemon switches repositories", async () => {
    const user = userEvent.setup();
    const fetch = vi.fn().mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [issue] }))
      .mockResolvedValueOnce(response({ ...repository, path: "/work/different" }))
      .mockResolvedValueOnce(response({ issues: [] }));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await user.click(await screen.findByRole("button", { name: /An existing issue/ }));
    fireEvent.focus(window);
    await waitFor(() => expect(screen.getByText("different")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Save issue" })).toBeDisabled();
  });

  it("requires an explicit revision choice after a conflict, without discarding the draft", async () => {
    const user = userEvent.setup();
    const latest = { ...issue, title: "Another developer's edit", revision: "c".repeat(40) };
    const fetch = vi.fn().mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [issue] }))
      .mockResolvedValueOnce(response({ error: { message: "issue changed" } }, 409))
      .mockResolvedValueOnce(response(repository))
      .mockResolvedValueOnce(response({ issues: [latest] }))
      .mockResolvedValueOnce(response({ ...latest, title: "My draft", revision: "d".repeat(40) }));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await user.click(await screen.findByRole("button", { name: /An existing issue/ }));
    const dialog = screen.getByRole("dialog");
    const title = within(dialog).getByLabelText(/^Title/);
    await user.clear(title);
    await user.type(title, "My draft");
    await user.click(within(dialog).getByRole("button", { name: "Save issue" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent("issue changed"));
    await user.click(within(dialog).getByRole("button", { name: "Reload" }));
    const apply = await screen.findByRole("button", { name: "Apply draft to latest revision" });
    expect(title).toHaveValue("My draft");
    expect(screen.getByRole("button", { name: "Save issue" })).toBeDisabled();
    await user.click(apply);
    await user.click(screen.getByRole("button", { name: "Save issue" }));
    expect(await screen.findByText("Issue saved locally.")).toBeInTheDocument();
    expect(JSON.parse(fetch.mock.calls[5][1].body)).toMatchObject({ title: "My draft", revision: latest.revision });
    expect(fetch.mock.calls[5][1].headers).toMatchObject({ "X-Gitsnow-Repository": repository.path });
  });

  it("distinguishes confirmed local saves from remote sharing, including in the browser cache", async () => {
    const user = userEvent.setup();
    const sharedRepository: Repository = {
      ...repository,
      sync: { state: "synced", remote: "origin", message: "Shared with origin" },
    };
    const fetch = vi.fn().mockResolvedValueOnce(response(sharedRepository))
      .mockResolvedValueOnce(response({ issues: [issue] }))
      .mockResolvedValueOnce(response({ ...issue, title: "Not published yet", revision: "c".repeat(40) }));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await user.click(await screen.findByRole("button", { name: /An existing issue/ }));
    const title = within(screen.getByRole("dialog")).getByLabelText(/^Title/);
    await user.clear(title);
    await user.type(title, "Not published yet");
    await user.click(screen.getByRole("button", { name: "Save issue" }));
    expect(await screen.findByText("Issue saved locally. Remote sharing is tracked below.")).toBeInTheDocument();
    expect(screen.getByText("Sharing pending")).toBeInTheDocument();
    expect(screen.queryByText("Shared with origin")).not.toBeInTheDocument();
    expect(readCache().snapshot?.repository.sync?.state).toBe("pending");
    expect(readCache().snapshot?.issues[0].title).toBe("Not published yet");
  });

  it("shows remote failure without disabling local issue creation, and queues a sharing retry", async () => {
    const user = userEvent.setup();
    const blockedRepository: Repository = {
      ...repository,
      sync: { state: "error", remote: "origin", message: "Not shared: remote unreachable. Local changes are safe." },
    };
    const fetch = vi.fn().mockResolvedValueOnce(response(blockedRepository))
      .mockResolvedValueOnce(response({ issues: [issue] }))
      .mockResolvedValueOnce(response({ state: "pending", remote: "origin", message: "Synchronization requested" }, 202));
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    expect(await screen.findByText("Not shared: remote unreachable. Local changes are safe.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "+ New issue" })).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Retry sharing" }));
    expect(await screen.findByText("Synchronization requested")).toBeInTheDocument();
    expect(fetch.mock.calls[2][0]).toBe("/api/sync");
    expect(fetch.mock.calls[2][1]).toMatchObject({ method: "POST", headers: { "X-Gitsnow-Repository": repository.path } });
  });

  it("lists conflicting issue paths without claiming they were published", async () => {
    const conflicted: Repository = {
      ...repository,
      sync: {
        state: "conflict", remote: "origin", message: "Not shared: conflicting edits. Both versions are preserved.",
        conflicts: ["issues/" + issue.id + ".md"],
      },
    };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response(conflicted))
      .mockResolvedValueOnce(response({ issues: [issue] })));
    render(<App />);
    expect(await screen.findByText("issues/" + issue.id + ".md")).toBeInTheDocument();
    expect(screen.getByText("Saved locally / not shared")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "+ New issue" })).toBeEnabled();
  });
});
