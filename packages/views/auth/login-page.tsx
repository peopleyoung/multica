"use client";

import { useState, useEffect, useCallback, useRef, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
  CardFooter,
} from "@multica/ui/components/ui/card";
import { Alert, AlertDescription } from "@multica/ui/components/ui/alert";
import { Input } from "@multica/ui/components/ui/input";
import { Button } from "@multica/ui/components/ui/button";
import { Label } from "@multica/ui/components/ui/label";
import { useAuthStore } from "@multica/core/auth";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { api } from "@multica/core/api";
import type { User } from "@multica/core/types";
import { useT } from "../i18n";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface CliCallbackConfig {
  /** Validated localhost callback URL */
  url: string;
  /** Opaque state to pass back to CLI */
  state: string;
}

interface LoginPageProps {
  /** Logo element rendered above the title */
  logo?: ReactNode;
  /** Called after successful login. The workspace list is seeded into React
   *  Query before this fires, so the caller can compute a destination URL. */
  onSuccess: () => void;
  /** CLI callback config for authorizing CLI tools. */
  cliCallback?: CliCallbackConfig;
  /** Called after a token is obtained (e.g. to set cookies). */
  onTokenObtained?: () => void;
  /** Slot rendered at the bottom of the sign-in card, below the
   *  form. The web shell uses it for a "Prefer the desktop
   *  app?" prompt; desktop omits it (a download prompt inside the app
   *  would be absurd). */
  extra?: ReactNode;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

export function redirectToCliCallback(url: string, token: string, state: string) {
  const separator = url.includes("?") ? "&" : "?";
  window.location.href = `${url}${separator}token=${encodeURIComponent(token)}&state=${encodeURIComponent(state)}`;
}

/**
 * Validate that a CLI callback URL points to a safe host over HTTP.
 * Allows localhost and private/LAN IPs (RFC 1918) to support self-hosted setups
 * on local VMs while blocking arbitrary public hosts.
 */
export function validateCliCallback(cliCallback: string): boolean {
  try {
    const cbUrl = new URL(cliCallback);
    if (cbUrl.protocol !== "http:") return false;
    const h = cbUrl.hostname;
    if (h === "localhost" || h === "127.0.0.1") return true;
    // Allow RFC 1918 private IPs: 10.x.x.x, 172.16-31.x.x, 192.168.x.x
    if (/^10\./.test(h)) return true;
    if (/^172\.(1[6-9]|2\d|3[01])\./.test(h)) return true;
    if (/^192\.168\./.test(h)) return true;
    return false;
  } catch {
    return false;
  }
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export function LoginPage({
  logo,
  onSuccess,
  cliCallback,
  onTokenObtained,
  extra,
}: LoginPageProps) {
  const { t } = useT("auth");
  const qc = useQueryClient();
  const [step, setStep] = useState<"login" | "register" | "cli_confirm">("login");
  const [email, setEmail] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const [existingUser, setExistingUser] = useState<User | null>(null);
  // Tracks how the existing session was detected so handleCliAuthorize
  // uses the matching token source (cookie → issueCliToken, localStorage → direct).
  const authSourceRef = useRef<"cookie" | "localStorage">("cookie");
  // The last session ended because the server rejected its credential, not
  // because the user asked to leave. Without saying so, landing here reads as
  // the app having lost their work for no reason.
  const sessionExpired = useAuthStore((state) => state.expired);

  // Check for existing session when CLI callback is present.
  // Prioritises cookie auth (= current browser session) to avoid authorising
  // the CLI with a stale or mismatched localStorage token.
  useEffect(() => {
    if (!cliCallback) return;

    // Snapshot the token before probing. The probe below is *expected* to 401
    // for a token-mode session, and a 401 ends the session — clearing this
    // very key — so reading it after the probe would always come back null
    // and the fallback could never run.
    const storedToken = localStorage.getItem("multica_token");

    // Ensure no stale bearer token interferes — we want to test the cookie first.
    api.setToken(null);

    api
      .getMe()
      .then((user) => {
        authSourceRef.current = "cookie";
        setExistingUser(user);
        setStep("cli_confirm");
      })
      .catch(() => {
        // Cookie auth failed — fall back to the token this browser had.
        if (!storedToken) return;

        localStorage.setItem("multica_token", storedToken);
        api.setToken(storedToken);
        api
          .getMe()
          .then((user) => {
            authSourceRef.current = "localStorage";
            setExistingUser(user);
            setStep("cli_confirm");
          })
          .catch(() => {
            api.setToken(null);
            localStorage.removeItem("multica_token");
          });
      });
  }, [cliCallback]);

  const handleSubmit = useCallback(async (event: React.FormEvent) => {
    event.preventDefault();
    if (step === "register" && password !== confirmPassword) {
      setError(t(($) => $.password.mismatch)); return;
    }
    setLoading(true);
    setError("");
    try {
      if (cliCallback) {
        const { token } = step === "register"
          ? await api.registerAccount(username, password, email || undefined)
          : await api.passwordLogin(username, password);
        localStorage.setItem("multica_token", token);
        api.setToken(token);
        onTokenObtained?.();
        redirectToCliCallback(cliCallback.url, token, cliCallback.state);
        return;
      }
      const store = useAuthStore.getState();
      if (step === "register") await store.registerAccount(username, password, email || undefined);
      else await store.passwordLogin(username, password);
      setPassword("");
      setConfirmPassword("");
      qc.setQueryData(workspaceKeys.list(), await api.listWorkspaces());
      onTokenObtained?.();
      onSuccess();
    } catch (err) {
      setError(err instanceof Error ? err.message : t(($) => $.password.failed));
    } finally { setLoading(false); }
  }, [step, username, password, confirmPassword, email, cliCallback, onTokenObtained, onSuccess, qc, t]);

  const handleCliAuthorize = async () => {
    if (!cliCallback) return;
    setLoading(true);

    try {
      let token: string;

      if (authSourceRef.current === "localStorage") {
        // Session was detected via localStorage — reuse that token directly.
        const stored = localStorage.getItem("multica_token");
        if (!stored) throw new Error("token missing");
        token = stored;
      } else {
        // Session was detected via cookie — obtain a bearer token from the server.
        const res = await api.issueCliToken();
        token = res.token;
      }

      onTokenObtained?.();
      redirectToCliCallback(cliCallback.url, token, cliCallback.state);
    } catch {
      setError(t(($) => $.errors.cli_auth_failed));
      setExistingUser(null);
      setStep("login");
      setLoading(false);
    }
  };

  // -------------------------------------------------------------------------
  // CLI confirm step
  // -------------------------------------------------------------------------

  if (step === "cli_confirm" && existingUser) {
    return (
      <div className="flex min-h-svh items-center justify-center">
        <Card className="w-full max-w-sm">
          <CardHeader className="text-center">
            {logo && <div className="mx-auto mb-4">{logo}</div>}
            <CardTitle className="text-display-sm">
              {t(($) => $.cli.title)}
            </CardTitle>
            <CardDescription>
              {t(($) => $.cli.description, { email: existingUser.username || existingUser.email || existingUser.name })}
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <Button
              onClick={handleCliAuthorize}
              disabled={loading}
              className="w-full"
              size="lg"
            >
              {loading
                ? t(($) => $.cli.authorizing)
                : t(($) => $.cli.authorize)}
            </Button>
            <Button
              variant="ghost"
              className="w-full"
              onClick={() => {
                setExistingUser(null);
                setStep("login");
              }}
            >
              {t(($) => $.cli.different_account)}
            </Button>
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="flex min-h-svh items-center justify-center">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          {logo && <div className="mx-auto mb-4">{logo}</div>}
          <CardTitle className="text-display-sm">
            {step === "register" ? t(($) => $.password.register_title) : t(($) => $.signin.title)}
          </CardTitle>

        </CardHeader>
        <CardContent className="space-y-4">
          {sessionExpired && (
            <Alert>
              <AlertDescription>
                {t(($) => $.errors.session_expired)}
              </AlertDescription>
            </Alert>
          )}
          <form id="login-form" onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="login-username">{t(($) => $.password.username)}</Label>
              <Input id="login-username" autoComplete="username" autoCapitalize="none" spellCheck={false} value={username} onChange={(e) => setUsername(e.target.value)} required minLength={3} maxLength={32} pattern="[a-zA-Z0-9][a-zA-Z0-9_.-]{2,31}" autoFocus disabled={loading} />
              {step === "register" && <p className="text-body-sm text-muted-foreground">{t(($) => $.password.username_help)}</p>}
            </div>
            <div className="space-y-2">
              <Label htmlFor="login-password">{t(($) => $.password.label)}</Label>
              <Input id="login-password" type="password" autoComplete={step === "register" ? "new-password" : "current-password"} value={password} onChange={(e) => setPassword(e.target.value)} required maxLength={72} disabled={loading} />
              {step === "register" && <p className="text-body-sm text-muted-foreground">{t(($) => $.password.help)}</p>}
            </div>
            {step === "register" && <>
              <div className="space-y-2">
                <Label htmlFor="confirm-password">{t(($) => $.password.confirm)}</Label>
                <Input id="confirm-password" type="password" autoComplete="new-password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} required disabled={loading} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="login-email">{t(($) => $.password.optional_email)}</Label>
                <Input id="login-email" type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} disabled={loading} />
              </div>
            </>}
            {error && (
              <p className="text-body text-destructive">{error}</p>
            )}
          </form>
        </CardContent>
        <CardFooter className="flex flex-col gap-3">
          <Button
            type="submit"
            form="login-form"
            className="w-full"
            size="lg"
            disabled={!username || !password || loading}
            aria-busy={loading}
          >
            {loading ? t(($) => $.password.submitting) : step === "register" ? t(($) => $.password.register) : t(($) => $.password.login)}
          </Button>
          <Button type="button" variant="ghost" disabled={loading} onClick={() => {
            setStep(step === "register" ? "login" : "register");
            setPassword(""); setConfirmPassword(""); setError("");
          }}>
            {step === "register" ? t(($) => $.password.have_account) : t(($) => $.password.create_account)}
          </Button>
          {extra && <div className="w-full pt-1 text-center">{extra}</div>}
        </CardFooter>
      </Card>
    </div>
  );
}
