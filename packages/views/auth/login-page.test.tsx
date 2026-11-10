import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enAuth from "../locales/en/auth.json";

const mocks = vi.hoisted(() => ({
  login: vi.fn(), register: vi.fn(), list: vi.fn(), seed: vi.fn(),
  getMe: vi.fn(), issueToken: vi.fn(), setToken: vi.fn(),
}));
vi.mock("@tanstack/react-query", () => ({ useQueryClient: () => ({ setQueryData: mocks.seed }) }));
vi.mock("@multica/core/auth", () => ({ useAuthStore: Object.assign(
  (select: (state: { expired: boolean }) => unknown) => select({ expired: false }),
  { getState: () => ({ passwordLogin: mocks.login, registerAccount: mocks.register }) },
) }));
vi.mock("@multica/core/api", () => ({ api: {
  passwordLogin: mocks.login, registerAccount: mocks.register,
  listWorkspaces: mocks.list, getMe: mocks.getMe,
  issueCliToken: mocks.issueToken, setToken: mocks.setToken,
} }));
import { LoginPage, validateCliCallback } from "./login-page";

function setup() {
 const success = vi.fn();
 render(<I18nProvider locale="en" resources={{ en: { auth: enAuth } }}><LoginPage onSuccess={success} /></I18nProvider>);
 return { user: userEvent.setup(), success };
}
async function fill(user: ReturnType<typeof userEvent.setup>) {
 await user.type(screen.getByLabelText("Username"), "alice");
 await user.type(screen.getByLabelText("Password", { exact: true }), "Strong password 2026");
}

beforeEach(() => {
 vi.clearAllMocks();
 mocks.login.mockResolvedValue({ id: "u" });
 mocks.register.mockResolvedValue({ id: "u" });
 mocks.list.mockResolvedValue([]);
});

describe("password authentication", () => {
 it("signs in, seeds workspaces and continues without a code", async () => {
  const { user, success } = setup(); await fill(user);
  await user.click(screen.getByRole("button", { name: /^Sign in$/ }));
  await waitFor(() => expect(success).toHaveBeenCalledOnce());
  expect(mocks.login).toHaveBeenCalledWith("alice", "Strong password 2026");
  expect(mocks.seed).toHaveBeenCalled();
  expect(screen.queryByText("Check your email")).not.toBeInTheDocument();
 });
 it("registers with no email and clears password fields", async () => {
  const { user, success } = setup();
  await user.click(screen.getByRole("button", { name: "Create an account" }));
  await fill(user);
  await user.type(screen.getByLabelText("Confirm password"), "Strong password 2026");
  await user.click(screen.getByRole("button", { name: /^Register$/ }));
  await waitFor(() => expect(success).toHaveBeenCalledOnce());
  expect(mocks.register).toHaveBeenCalledWith("alice", "Strong password 2026", undefined);
  expect(screen.getByLabelText("Password", { exact: true })).toHaveValue("");
 });
 it("requires matching confirmation before registering", async () => {
  const { user } = setup();
  await user.click(screen.getByRole("button", { name: "Create an account" }));
  await fill(user); await user.type(screen.getByLabelText("Confirm password"), "different password");
  await user.click(screen.getByRole("button", { name: /^Register$/ }));
  expect(await screen.findByText("Passwords do not match.")).toBeVisible();
  expect(mocks.register).not.toHaveBeenCalled();
 });
 it("shows credential errors and keeps the form available for retry", async () => {
  mocks.login.mockRejectedValue(new Error("Incorrect username or password"));
  const { user, success } = setup(); await fill(user);
  await user.click(screen.getByRole("button", { name: /^Sign in$/ }));
  expect(await screen.findByText("Incorrect username or password")).toBeVisible();
  expect(success).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: /^Sign in$/ })).toBeEnabled();
 });
 it("clears passwords when switching forms", async () => {
  const { user } = setup(); await fill(user);
  await user.click(screen.getByRole("button", { name: "Create an account" }));
  expect(screen.getByLabelText("Password", { exact: true })).toHaveValue("");
 });
});

it("restricts CLI callbacks to HTTP on localhost or private networks", () => {
 for (const url of ["http://localhost:1234/callback", "http://127.0.0.1/a", "http://192.168.1.1/a"]) expect(validateCliCallback(url)).toBe(true);
 for (const url of ["https://example.com", "http://example.com", "javascript:alert(1)", "invalid"]) expect(validateCliCallback(url)).toBe(false);
});
