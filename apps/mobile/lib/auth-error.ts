/** Map password authentication failures to actionable, localized copy. */
type Translate = (key: string) => string;

export function mapAuthError(err: unknown, fallback: string, t?: Translate): string {
  if (!(err instanceof Error)) return fallback;
  const message = err.message.toLowerCase();
  const translated = (key: string, english: string) => t ? t(`password.${key}`) : english;
  if (message.includes("incorrect username or password")) {
    return translated("invalid_credentials", "Incorrect username or password.");
  }
  if (message.includes("already in use")) {
    return translated("account_conflict", "Username or contact email is already in use.");
  }
  if (message.includes("registration is disabled")) {
    return translated("signup_disabled", "Registration is closed. Ask an administrator for an account.");
  }
  if (message.includes("email allowlists")) {
    return translated("admin_required", "Ask an administrator to create your account.");
  }
  if (message.includes("account disabled")) {
    return translated("account_disabled", "This account is disabled.");
  }
  if (message.includes("invalid contact email")) {
    return translated("invalid_email", "Enter a valid email or leave it empty.");
  }
  if (message.startsWith("password must") || message.startsWith("password exceeds")) {
    return translated("help", "Password must be 12–72 bytes.");
  }
  if (message.startsWith("username must")) {
    return translated("username_help", "Username must be 3–32 letters, digits, dots, underscores or hyphens.");
  }
  if (/rate.?limit|too many|throttle/.test(message)) {
    return translated("rate_limited", "Too many attempts. Wait a moment and try again.");
  }
  if (/network|fetch|timeout|unreachable/.test(message)) {
    return translated("network", "Can't reach Multica. Check your connection and retry.");
  }
  return fallback;
}
