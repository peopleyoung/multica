import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";

// Specialize build copies only; the other deployment keeps upstream cookie names.
const [scope, root] = process.argv.slice(2);
if (!root || !["backend", "frontend"].includes(scope)) {
  throw new Error("Usage: namespace-cookies.mjs <backend|frontend> <build-source-root>");
}
const names = [
  "multica_auth",
  "multica_csrf",
  "multica_csrf_session",
  "multica_logged_in",
  "last_workspace_slug",
];
const counts = new Map(names.map((name) => [name, 0]));
const pattern = new RegExp(`\\b(${names.join("|")})\\b`, "g");
function rewrite(file) {
  const original = readFileSync(file, "utf8");
  const updated = original.replace(pattern, (name) => {
    counts.set(name, counts.get(name) + 1);
    return `${name}_harness_23000`;
  });
  if (updated !== original) writeFileSync(file, updated);
}
function walk(directory) {
  for (const entry of readdirSync(directory)) {
    if (["node_modules", ".next", "dist"].includes(entry)) continue;
    const path = join(directory, entry);
    if (statSync(path).isDirectory()) walk(path);
    else if (/\.(ts|tsx|mjs)$/.test(entry)) rewrite(path);
  }
}
if (scope === "backend") rewrite(join(root, "internal/auth/cookie.go"));
else {
  for (const directory of ["apps/web", "packages/core", "packages/views"]) {
    walk(join(root, directory));
  }
}
const required = scope === "backend" ? names.slice(0, 3) : names.slice(1);
for (const name of required) {
  if (!counts.get(name)) throw new Error(`Cookie no longer found in ${scope} build: ${name}`);
}
console.log(`Applied harness_23000 cookie namespace to ${scope} build sources.`);
