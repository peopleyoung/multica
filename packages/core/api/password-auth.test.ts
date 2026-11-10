// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { setSchemaLogger } from "./schema";
import { PasswordLoginResponseSchema } from "./schemas";

afterEach(() => { vi.unstubAllGlobals(); setSchemaLogger({ debug: () => {}, info: () => {}, warn: () => {}, error: () => {} }); });

describe("password authentication contract", () => {
 it("sends a username and password and parses an account without email", async () => {
  const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ token: "session-token", user: { id: "u1", username: "alice" } })));
  vi.stubGlobal("fetch", fetchMock);
  const result = await new ApiClient("https://api.example.test").registerAccount("alice", "Strong password 2026");
  expect(fetchMock).toHaveBeenCalledWith("https://api.example.test/auth/register", expect.objectContaining({ method: "POST", body: JSON.stringify({ username: "alice", password: "Strong password 2026" }) }));
  expect(result.user.email).toBe("");
  expect(result.user.username).toBe("alice");
 });
 it.each([null, {}, { token: "", user: { id: "u" } }, { token: "secret-session", user: {} }, { token: "secret-session", user: { id: 123 } }])("rejects a malformed success without logging credentials: %j", async (body) => {
  const warn = vi.fn(); setSchemaLogger({ debug: vi.fn(), info: vi.fn(), warn, error: vi.fn() });
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body))));
  await expect(new ApiClient("https://api.example.test").passwordLogin("alice", "private-password")).rejects.toThrow("Invalid sign-in response");
  expect(JSON.stringify(warn.mock.calls)).not.toContain("secret-session");
  expect(JSON.stringify(warn.mock.calls)).not.toContain("private-password");
 });
 it("defaults username for previously installed backends", () => {
  expect(PasswordLoginResponseSchema.parse({ token: "session", user: { id: "u" } }).user.username).toBeNull();
 });
});
