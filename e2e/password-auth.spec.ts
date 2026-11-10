import { test, expect } from "@playwright/test";
import { TestApiClient } from "./fixtures";

for (const withEmail of [false, true]) {
  test(`password registration and login ${withEmail ? "with optional email" : "without email"}`, async ({ page, context }) => {
    test.setTimeout(180_000);
    page.setDefaultNavigationTimeout(90_000);
    const api = new TestApiClient();
    const username = `e2e_password_${Date.now().toString(36)}${withEmail ? "e" : "n"}`;
    const password = "Browser password 2026!";
    const email = withEmail ? `${username}@example.com` : "";
    try {
      await page.goto("/login");
      await page.getByRole("button", { name: "Create an account", exact: true }).click();
      await page.locator("#login-username").fill(username);
      await page.locator("#login-password").fill(password);
      await page.locator("#confirm-password").fill(password);
      if (withEmail) await page.locator("#login-email").fill(email);
      const registration = page.waitForResponse((response) => response.url().endsWith("/auth/register"));
      await page.getByRole("button", { name: "Register", exact: true }).click();
      const response = await registration;
      expect(response.status()).toBe(201);
      const created = await response.json();
      expect(created.user.username).toBe(username);
      expect(created.user.email).toBe(email);
      expect(created.user).not.toHaveProperty("password_hash");
      await expect(page).toHaveURL(/\/onboarding/, { timeout: 60_000 });

      // End the browser session before exercising a fresh password login.
      await context.clearCookies();
      await page.evaluate(() => localStorage.clear());
      await page.goto("/login");
      await page.locator("#login-username").fill(username.toUpperCase());
      await page.locator("#login-password").fill("incorrect password");
      const rejected = page.waitForResponse((result) => result.url().endsWith("/auth/login"));
      await page.getByRole("button", { name: "Sign in", exact: true }).click();
      expect((await rejected).status()).toBe(401);
      await expect(page).toHaveURL(/\/login/);
      await expect(page.getByText("incorrect username or password", { exact: true })).toBeVisible();

      await page.locator("#login-password").fill(password);
      const login = page.waitForResponse((result) => result.url().endsWith("/auth/login"));
      await page.getByRole("button", { name: "Sign in", exact: true }).click();
      const signedIn = await login;
      expect(signedIn.status()).toBe(200);
      expect((await signedIn.json()).user.id).toBe(created.user.id);
      await expect(page).toHaveURL(/\/onboarding/, { timeout: 60_000 });
    } finally {
      await api.removePasswordTestAccount(username);
    }
  });
}
