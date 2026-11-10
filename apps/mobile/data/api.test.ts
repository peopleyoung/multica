import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "./api";

// api.ts refuses to load without a base URL; set it before the import runs.
vi.hoisted(() => {
  process.env.EXPO_PUBLIC_API_URL = "https://api.example.test";
});

// The real store pulls in expo-secure-store; the client only needs the slug.
vi.mock("@/data/workspace-store", () => ({ getCurrentSlug: () => null }));

describe("api.deleteComment", () => {
  const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));

  beforeEach(() => {
    fetchMock.mockClear();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // #8296: servers that keep a deleted comment's replies also route
  // /keep-replies; older servers do not, so a keep-replies delete that reaches
  // one fails instead of deleting the replies too.
  it.each([
    [{ keepReplies: true }, "https://api.example.test/api/comments/comment-1/keep-replies"],
    [{ keepReplies: false }, "https://api.example.test/api/comments/comment-1"],
    [undefined, "https://api.example.test/api/comments/comment-1"],
  ])("with %j sends DELETE %s", async (opts, url) => {
    await api.deleteComment("comment-1", opts);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(url, expect.objectContaining({ method: "DELETE" }));
  });
});

describe("api.nativeModel", () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });
  it("parses fixed deployment metadata and fails visibly on malformed responses", async () => {
    vi.spyOn(console, "log").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ status: "completed", supported: false, models: [{ id: "fixed", default: true }] })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ models: "malformed" })));
    vi.stubGlobal("fetch", fetchMock);
    expect((await api.nativeModel("runtime")).models?.[0].id).toBe("fixed");
    expect((await api.nativeModel("runtime")).status).toBe("failed");
    expect(fetchMock).toHaveBeenCalledWith("https://api.example.test/api/runtimes/runtime/models", expect.objectContaining({ method: "POST" }));
  });
});

describe("api.passwordAccount", () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });
  it("registers without an email and validates the returned session", async () => {
    vi.spyOn(console, "log").mockImplementation(() => {});
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ token: "mobile-session", user: { id: "mobile-user", username: "alice" } })));
    vi.stubGlobal("fetch", fetchMock);
    const result = await api.registerAccount("alice", "Strong password 2026");
    expect(fetchMock).toHaveBeenCalledWith("https://api.example.test/auth/register", expect.objectContaining({ method: "POST", body: JSON.stringify({ username: "alice", password: "Strong password 2026" }) }));
    expect(result.user.id).toBe("mobile-user");
  });
  it("rejects malformed credentials before the auth store can save them", async () => {
    vi.spyOn(console, "log").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ token: "", user: { id: "user" } }))));
    await expect(api.passwordLogin("alice", "Strong password 2026")).rejects.toThrow("Invalid sign-in response");
  });
});
