// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  nativeRuntimeConfigReady,
  parseNativeRuntimeConfig,
  parseNativeRuntimeLimits,
} from "./native-runtime-config";

describe("native runtime settings", () => {
  it("keeps missing or malformed host permissions disabled", () => {
    for (const input of [
      undefined,
      null,
      [],
      { write_files: "true", shell: 1 },
      { http_origins: [false] },
    ]) {
      expect(parseNativeRuntimeLimits(input)).toEqual({
        write_files: false,
        shell: false,
        http_origins: [],
      });
    }
    expect(parseNativeRuntimeConfig({})).toEqual({
      write_files: false,
      shell: false,
      services: [],
    });
    expect(
      parseNativeRuntimeConfig({ services: "malformed" }).services,
    ).toEqual([]);
  });
  it("requires host approval, unique service names and credential variable references", () => {
    const limits = {
      write_files: true,
      shell: false,
      http_origins: ["https://cvat.example"],
    };
    const config = parseNativeRuntimeConfig({
      write_files: true,
      services: [
        {
          name: "cvat",
          base_url: "https://cvat.example/api",
          token_env: "CVAT_TOKEN",
          auth_scheme: "Token",
        },
      ],
    });
    expect(nativeRuntimeConfigReady(config, limits)).toBe(true);
    expect(nativeRuntimeConfigReady({ ...config, shell: true }, limits)).toBe(
      false,
    );
    expect(
      nativeRuntimeConfigReady(
        { ...config, services: [...config.services, ...config.services] },
        limits,
      ),
    ).toBe(false);
    expect(
      nativeRuntimeConfigReady(
        {
          ...config,
          services: [{ ...config.services[0]!, token_env: "MULTICA_TOKEN" }],
        },
        limits,
      ),
    ).toBe(false);
    expect(
      nativeRuntimeConfigReady(config, { ...limits, http_origins: [] }),
    ).toBe(false);
  });
});

it("preserves the remote environment and requires an admitted writable service", () => {
  const config = parseNativeRuntimeConfig({ environment: "multica_jobs", shell: true, services: [{ name: "multica_jobs", base_url: "https://jobs.example", allow_write: true }] });
  expect(config.environment).toBe("multica_jobs");
  const limits = { shell: true, write_files: true, http_origins: ["https://jobs.example"] };
  expect(nativeRuntimeConfigReady(config, limits)).toBe(true);
  expect(nativeRuntimeConfigReady({ ...config, services: [] }, limits)).toBe(false);
  expect(nativeRuntimeConfigReady({ ...config, services: config.services.map((service) => ({ ...service, allow_write: false })) }, limits)).toBe(false);
  expect(parseNativeRuntimeConfig({ environment: "future" }).environment).toBeUndefined();
});
