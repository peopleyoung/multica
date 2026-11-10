// @vitest-environment node
import { describe, expect, it } from "vitest";
import { mapAuthError } from "./auth-error";

describe("password auth errors", () => {
  it("identifies wrong credentials without telling users to resend a code", () => {
    const message = mapAuthError(new Error("incorrect username or password"), "Failed");
    expect(message).toBe("Incorrect username or password.");
    expect(message).not.toMatch(/code|resend/i);
  });

  it.each([
    ["username or contact email is already in use", "account_conflict"],
    ["user registration is disabled on this self-hosted instance", "signup_disabled"],
    ["email allowlists cannot authorize unverified accounts", "admin_required"],
    ["invalid contact email", "invalid_email"],
    ["password exceeds 72 bytes", "help"],
    ["Too many requests", "rate_limited"],
    ["Failed to fetch", "network"],
  ])("localizes %s", (message, key) => {
    expect(mapAuthError(new Error(message), "Failed", (key) => key)).toBe(`password.${key}`);
  });

  it("uses the caller's fallback for an unknown server failure", () => {
    expect(mapAuthError(new Error("internal server error"), "Try again")).toBe("Try again");
  });
});
