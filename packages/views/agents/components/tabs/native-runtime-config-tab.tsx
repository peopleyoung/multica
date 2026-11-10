"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import type { Agent, AgentRuntime } from "@multica/core/types";
import {
  nativeRuntimeConfigReady,
  parseNativeRuntimeConfig,
  parseNativeRuntimeLimits,
  type NativeRuntimeService,
} from "@multica/core/agents";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Switch } from "@multica/ui/components/ui/switch";
import { toast } from "sonner";
import { useT } from "../../../i18n";

export function NativeRuntimeConfigTab({
  agent,
  runtime,
  canConfigure,
  onSave,
  onDirtyChange,
}: {
  agent: Agent;
  runtime: AgentRuntime;
  canConfigure: boolean;
  onSave: (updates: {
    runtime_config: Record<string, unknown>;
  }) => Promise<void>;
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const { t } = useT("agents");
  const original = useMemo(
    () => parseNativeRuntimeConfig(agent.runtime_config),
    [agent.runtime_config],
  );
  const limits = useMemo(
    () => parseNativeRuntimeLimits(runtime.metadata.native_limits),
    [runtime.metadata],
  );
  const [draft, setDraft] = useState(original);
  const [saving, setSaving] = useState(false);
  const previous = useRef(original);
  useEffect(() => {
    setDraft((current) =>
      JSON.stringify(current) === JSON.stringify(previous.current)
        ? original
        : current,
    );
    previous.current = original;
  }, [original]);
  const dirty = JSON.stringify(draft) !== JSON.stringify(original);
  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);
  const ready = nativeRuntimeConfigReady(draft, limits);
  const editService = (index: number, update: Partial<NativeRuntimeService>) =>
    setDraft((current) => ({
      ...current,
      services: current.services.map((service, i) =>
        i === index ? { ...service, ...update } : service,
      ),
    }));
  const save = async () => {
    if (!ready || saving || !canConfigure) return;
    setSaving(true);
    try {
      await onSave({ runtime_config: { ...draft } });
      toast.success(t(($) => $.tab_body.runtime_config.saved_toast));
    } catch {
      toast.error(t(($) => $.tab_body.runtime_config.save_failed_toast));
    } finally {
      setSaving(false);
    }
  };
  return (
    <div className="space-y-5">
      <p className="text-caption text-muted-foreground">
        {t(($) => $.tab_body.native_runtime.host_limits)}
      </p>
      {!canConfigure && (
        <p className="text-caption">
          {t(($) => $.tab_body.native_runtime.owner_only)}
        </p>
      )}
      {runtime.status !== "online" && (
        <p role="status" className="text-caption">
          {t(($) => $.tab_body.native_runtime.offline)}
        </p>
      )}
      <fieldset disabled={!canConfigure || saving} className="space-y-5">
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="native-remote">
            {t(($) => $.tab_body.native_runtime.remote_environment)}
          </Label>
          <Switch
            id="native-remote"
            checked={draft.environment === "multica_jobs"}
            disabled={
              !canConfigure ||
              saving ||
              (draft.environment !== "multica_jobs" &&
                !draft.services.some((service) => service.name === "multica_jobs"))
            }
            onCheckedChange={(checked) =>
              setDraft((value) => ({
                ...value,
                environment: checked ? "multica_jobs" : "local",
              }))
            }
          />
        </div>
        <p className="text-caption text-muted-foreground">
          {t(($) => $.tab_body.native_runtime.remote_environment_help)}
        </p>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="native-write">
            {t(($) => $.tab_body.native_runtime.write_files)}
          </Label>
          <Switch
            id="native-write"
            checked={draft.write_files}
            disabled={
              !canConfigure ||
              saving ||
              (!limits.write_files && !draft.write_files)
            }
            onCheckedChange={(checked) =>
              setDraft((value) => ({ ...value, write_files: checked }))
            }
          />
        </div>
        <p className="text-caption text-muted-foreground">
          {t(($) =>
            limits.write_files
              ? $.tab_body.native_runtime.host_allowed
              : $.tab_body.native_runtime.host_disabled,
          )}
        </p>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="native-shell">
            {t(($) => $.tab_body.native_runtime.shell)}
          </Label>
          <Switch
            id="native-shell"
            checked={draft.shell}
            disabled={
              !canConfigure || saving || (!limits.shell && !draft.shell)
            }
            onCheckedChange={(checked) =>
              setDraft((value) => ({ ...value, shell: checked }))
            }
          />
        </div>
        <p className="text-caption text-muted-foreground">
          {t(($) => $.tab_body.native_runtime.shell_effect)}
        </p>
        <p className="text-caption text-muted-foreground">
          {t(($) =>
            limits.shell
              ? $.tab_body.native_runtime.host_allowed
              : $.tab_body.native_runtime.host_disabled,
          )}
        </p>
        <div className="space-y-3">
          <h3 className="text-body-sm font-medium">
            {t(($) => $.tab_body.native_runtime.services)}
          </h3>
          <p className="break-words text-caption text-muted-foreground">
            {t(($) => $.tab_body.native_runtime.approved_origins)}:{" "}
            {limits.http_origins.join(", ") ||
              t(($) => $.tab_body.native_runtime.none)}
          </p>
          {draft.services.map((service, index) => (
            <fieldset key={index} className="space-y-3 rounded-md border p-3">
              <legend className="px-1 text-caption">
                {t(($) => $.tab_body.native_runtime.service_number, {
                  number: index + 1,
                })}
              </legend>
              <div className="space-y-1">
                <Label htmlFor={`service-name-${index}`}>
                  {t(($) => $.tab_body.native_runtime.name)}
                </Label>
                <Input
                  id={`service-name-${index}`}
                  value={service.name}
                  onChange={(e) => editService(index, { name: e.target.value })}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor={`service-url-${index}`}>
                  {t(($) => $.tab_body.native_runtime.base_url)}
                </Label>
                <Input
                  id={`service-url-${index}`}
                  type="url"
                  value={service.base_url}
                  onChange={(e) =>
                    editService(index, { base_url: e.target.value })
                  }
                  placeholder="https://cvat.example/api"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor={`service-env-${index}`}>
                  {t(($) => $.tab_body.native_runtime.token_env)}
                </Label>
                <Input
                  id={`service-env-${index}`}
                  value={service.token_env}
                  onChange={(e) =>
                    editService(index, { token_env: e.target.value })
                  }
                  placeholder={t(
                    ($) => $.tab_body.native_runtime.token_env_placeholder,
                  )}
                  autoComplete="off"
                />
              </div>
              <p className="text-caption text-muted-foreground">
                {t(($) => $.tab_body.native_runtime.env_help)}
              </p>
              <div className="space-y-1">
                <Label htmlFor={`service-auth-${index}`}>
                  {t(($) => $.tab_body.native_runtime.auth_scheme)}
                </Label>
                <select
                  id={`service-auth-${index}`}
                  className="h-9 w-full rounded-md border bg-background px-3 text-body-sm"
                  value={service.auth_scheme || "Bearer"}
                  onChange={(e) =>
                    editService(index, {
                      auth_scheme:
                        e.target.value === "Token" ? "Token" : "Bearer",
                    })
                  }
                >
                  <option value="Bearer">
                    {t(($) => $.tab_body.native_runtime.auth_bearer)}
                  </option>
                  <option value="Token">
                    {t(($) => $.tab_body.native_runtime.auth_token)}
                  </option>
                </select>
              </div>
              <div className="flex items-center justify-between gap-4">
                <Label htmlFor={`service-write-${index}`}>
                  {t(($) => $.tab_body.native_runtime.service_write)}
                </Label>
                <Switch
                  id={`service-write-${index}`}
                  checked={service.allow_write}
                  disabled={!canConfigure || saving}
                  onCheckedChange={(checked) =>
                    editService(index, { allow_write: checked })
                  }
                />
              </div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() =>
                  setDraft((value) => ({
                    ...value,
                    services: value.services.filter((_, i) => i !== index),
                  }))
                }
              >
                {t(($) => $.tab_body.native_runtime.remove_service)}
              </Button>
            </fieldset>
          ))}
          <Button
            type="button"
            variant="outline"
            disabled={
              draft.services.length >= 8 || limits.http_origins.length === 0
            }
            onClick={() =>
              setDraft((value) => ({
                ...value,
                services: [
                  ...value.services,
                  {
                    name: "",
                    base_url: "",
                    token_env: "",
                    auth_scheme: "Token",
                    allow_write: false,
                  },
                ],
              }))
            }
          >
            {t(($) => $.tab_body.native_runtime.add_service)}
          </Button>
        </div>
      </fieldset>
      {!ready && (
        <p role="alert" className="text-caption text-destructive">
          {t(($) => $.tab_body.native_runtime.invalid)}
        </p>
      )}
      <details className="space-y-2 text-caption text-muted-foreground">
        <summary className="cursor-pointer">
          {t(($) => $.tab_body.native_runtime.host_setup)}
        </summary>
        <p className="mt-2 break-words">
          {t(($) => $.tab_body.native_runtime.host_setup_help)}
        </p>
      </details>
      {canConfigure && (
        <Button
          type="button"
          disabled={!dirty || !ready || saving}
          aria-busy={saving}
          onClick={save}
        >
          {t(($) => $.tab_body.native_runtime.save)}
        </Button>
      )}
    </div>
  );
}
