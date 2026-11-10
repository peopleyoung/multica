import { z } from "zod";
import { parseWithFallback } from "../api/schema";

export const nativeServiceSchema = z.object({
  name: z.string().regex(/^[a-zA-Z][a-zA-Z0-9_-]{0,39}$/),
  base_url: z
    .string()
    .url()
    .refine((raw) => {
      try {
        const u = new URL(raw);
        return (
          ["http:", "https:"].includes(u.protocol) &&
          !u.username &&
          !u.password &&
          !u.search &&
          !u.hash &&
          !u.pathname.includes("..")
        );
      } catch {
        return false;
      }
    }),
  token_env: z.string().optional().default(""),
  auth_scheme: z.enum(["", "Bearer", "Token"]).optional().default("Bearer"),
  allow_write: z.boolean().optional().default(false),
});
const nativeConfigSchema = z.object({
  environment: z.enum(["local", "multica_jobs"]).optional().catch(undefined),
  write_files: z.boolean().optional().default(false),
  shell: z.boolean().optional().default(false),
  services: z.array(nativeServiceSchema).max(8).optional().default([]),
});
const nativeLimitsSchema = z.object({
  write_files: z.boolean().optional().default(false),
  shell: z.boolean().optional().default(false),
  http_origins: z
    .array(z.string().url())
    .nullable()
    .optional()
    .transform((value) => value ?? []),
});
export type NativeRuntimeConfig = z.infer<typeof nativeConfigSchema>;
export type NativeRuntimeService = z.infer<typeof nativeServiceSchema>;
export type NativeRuntimeLimits = z.infer<typeof nativeLimitsSchema>;
export const emptyNativeRuntimeConfig: NativeRuntimeConfig = {
  write_files: false,
  shell: false,
  services: [],
};
export function parseNativeRuntimeConfig(raw: unknown): NativeRuntimeConfig {
  return parseWithFallback(raw, nativeConfigSchema, emptyNativeRuntimeConfig, {
    endpoint: "agent.runtime_config.native",
  });
}
export function parseNativeRuntimeLimits(raw: unknown): NativeRuntimeLimits {
  return parseWithFallback(
    raw,
    nativeLimitsSchema,
    { write_files: false, shell: false, http_origins: [] },
    { endpoint: "runtime.metadata.native_limits" },
  );
}
export function nativeRuntimeConfigReady(
  config: NativeRuntimeConfig,
  limits: NativeRuntimeLimits,
): boolean {
  const remote = config.services.find(
    (service) => service.name === "multica_jobs",
  );
  return (
    (config.environment !== "multica_jobs" ||
      (!!remote && (!(config.shell || config.write_files) || remote.allow_write))) &&
    (!config.write_files || limits.write_files) &&
    (!config.shell || limits.shell) &&
    config.services.every((service) => {
      if (!nativeServiceSchema.safeParse(service).success) return false;
      if (
        service.token_env &&
        (!/^[A-Z][A-Z0-9_]{0,99}$/.test(service.token_env) ||
          service.token_env.startsWith("MULTICA_") ||
          service.token_env === "OPENAI_API_KEY")
      )
        return false;
      return limits.http_origins.some(
        (origin) => new URL(origin).origin === new URL(service.base_url).origin,
      );
    }) &&
    new Set(config.services.map((service) => service.name)).size ===
      config.services.length
  );
}
