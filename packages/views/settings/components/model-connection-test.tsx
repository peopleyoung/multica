"use client";

import { useEffect, useMemo, useRef } from "react";
import { LoaderCircle } from "lucide-react";
import { useTestWorkspaceModel } from "@multica/core/workspace/mutations";
import type { ModelConnectionInput } from "@multica/core/types/workspace-model";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

interface ModelConnectionTestProps {
  wsId: string;
  modelId?: string;
  config: ModelConnectionInput;
  apiKey?: string;
  disabled?: boolean;
  name?: string;
}

export function ModelConnectionTest({
  wsId,
  modelId,
  config,
  apiKey,
  disabled = false,
  name,
}: ModelConnectionTestProps) {
  const { t } = useT("settings");
  const test = useTestWorkspaceModel(wsId);
  const abort = useRef<AbortController | null>(null);
  const input = useMemo(
    () => ({
      base_url: config.base_url,
      model: config.model,
      capabilities: config.capabilities,
      ...(apiKey !== undefined ? { api_key: apiKey } : {}),
    }),
    [config, apiKey],
  );
  const { reset } = test;
  useEffect(() => {
    reset();
    return () => {
      abort.current?.abort();
    };
  }, [wsId, modelId, input, reset]);

  // A result belongs only to the exact draft tested, even before effect cleanup.
  const current = test.variables?.input === input;
  const pending = current && test.isPending;
  const result = current ? test.data : undefined;
  const failed = current && test.isError;

  function failureMessage(code: string) {
    switch (code) {
      case "model_authentication_failed":
        return t(($) => $.models.connection.auth_failed);
      case "model_not_found":
        return t(($) => $.models.connection.model_not_found);
      case "model_request_rejected":
        return t(($) => $.models.connection.request_rejected);
      case "model_quota_exceeded":
      case "model_rate_limited":
        return t(($) => $.models.connection.rate_limited);
      case "model_step_cancelled":
        return t(($) => $.models.connection.timeout);
      case "model_transport_error":
        return t(($) => $.models.connection.transport_failed);
      case "model_tool_call_failed":
      case "model_tool_result_failed":
        return t(($) => $.models.connection.tools_failed);
      default:
        return t(($) => $.models.connection.failed);
    }
  }
  const stage =
    result?.stage === "text"
      ? t(($) => $.models.connection.text)
      : result?.stage === "tool_call"
        ? t(($) => $.models.connection.tool_call)
        : result?.stage === "tool_result"
          ? t(($) => $.models.connection.tool_result)
          : t(($) => $.models.connection.test);

  return (
    <div className="min-w-0 space-y-2">
      <Button
        type="button"
        variant="outline"
        disabled={
          disabled || pending || !input.base_url.trim() || !input.model.trim()
        }
        aria-busy={pending}
        aria-label={
          name ? `${t(($) => $.models.connection.test)} ${name}` : undefined
        }
        onClick={() => {
          abort.current?.abort();
          const controller = new AbortController();
          abort.current = controller;
          test.mutate({ id: modelId, input, signal: controller.signal });
        }}
      >
        {pending && <LoaderCircle className="animate-spin" />}
        {pending
          ? t(($) => $.models.connection.testing)
          : t(($) => $.models.connection.test)}
      </Button>
      {pending && (
        <p role="status" className="text-caption text-muted-foreground">
          {t(($) => $.models.connection.wait)}
        </p>
      )}
      {result && (
        <p
          role={result.ok ? "status" : "alert"}
          className={`max-w-sm break-words text-caption ${result.ok ? "text-muted-foreground" : "text-destructive"}`}
        >
          {result.ok
            ? t(($) => $.models.connection.success, { ms: result.duration_ms })
            : t(($) => $.models.connection.failure_detail, {
                stage,
                message: failureMessage(result.code),
                ms: result.duration_ms,
              })}
        </p>
      )}
      {failed && (
        <p role="alert" className="max-w-sm text-caption text-destructive">
          {t(($) => $.models.connection.failed)}
        </p>
      )}
    </div>
  );
}
