"use client";

import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useCurrentWorkspace } from "@multica/core/paths";
import { useCurrentMember } from "@multica/core/permissions";
import { workspaceModelsOptions } from "@multica/core/workspace/queries";
import {
  useSaveWorkspaceModel,
  useDeleteWorkspaceModel,
} from "@multica/core/workspace/mutations";
import type {
  WorkspaceModel,
  WorkspaceModelInput,
} from "@multica/core/types/workspace-model";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@multica/ui/components/ui/select";
import {
  Dialog,
  DialogTrigger,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
  DialogClose,
} from "@multica/ui/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
  AlertDialogAction,
} from "@multica/ui/components/ui/alert-dialog";
import {
  SettingsTab,
  SettingsCard,
  SettingsReadOnlyNotice,
} from "./settings-layout";
import { useT } from "../../i18n";
import { ModelConnectionTest } from "./model-connection-test";

const modelAPIs = [
  { value: "chat_completions", label: "Chat Completions" },
  { value: "responses", label: "Responses" },
] as const;

const newModel: WorkspaceModelInput = {
  name: "",
  base_url: "",
  model: "",
  is_default: false,
  enabled: true,
  capabilities: {
    api: "chat_completions",
    streaming_tool_calls: false,
    json_object: false,
    context_tokens: 128000,
    max_output_tokens: 8192,
    max_tool_argument_bytes: 65536,
    max_tools: 128,
    image_input: false,
  },
};

export function ModelsTab() {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const wsId = workspace?.id ?? "";
  const { role } = useCurrentMember(wsId);
  const canManage = role === "owner" || role === "admin";
  const models = useQuery(workspaceModelsOptions(wsId));
  const save = useSaveWorkspaceModel(wsId);
  const remove = useDeleteWorkspaceModel(wsId);
  const [editing, setEditing] = useState<WorkspaceModel | null>(null);
  const [draft, setDraft] = useState<WorkspaceModelInput>(newModel);
  const [apiKey, setAPIKey] = useState("");
  const [clearKey, setClearKey] = useState(false);
  const [open, setOpen] = useState(false);
  const [deleting, setDeleting] = useState<WorkspaceModel | null>(null);
  const [error, setError] = useState("");
  const [deleteError, setDeleteError] = useState("");

  function startEditor(model: WorkspaceModel | null) {
    setEditing(model);
    setDraft(
      model
        ? {
            name: model.name,
            base_url: model.base_url,
            model: model.model,
            is_default: model.is_default,
            enabled: model.enabled,
            capabilities: { ...model.capabilities },
          }
        : {
            ...newModel,
            capabilities: { ...newModel.capabilities },
            is_default: models.data?.length === 0,
          },
    );
    setAPIKey("");
    setClearKey(false);
    setError("");
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    try {
      await save.mutateAsync({
        id: editing?.id,
        input: {
          ...draft,
          ...(clearKey ? { api_key: "" } : apiKey ? { api_key: apiKey } : {}),
        },
      });
      setAPIKey("");
      setOpen(false);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : t(($) => $.models.save_failed),
      );
    }
  }

  const editor = (
    <DialogContent className="max-h-[85vh] overflow-y-auto">
      <form onSubmit={submit} className="space-y-4">
        <DialogHeader>
          <DialogTitle>
            {editing ? t(($) => $.models.edit) : t(($) => $.models.add)}
          </DialogTitle>
        </DialogHeader>
        <div className="space-y-1.5">
          <Label htmlFor="model-name">{t(($) => $.models.name)}</Label>
          <Input
            id="model-name"
            required
            maxLength={100}
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="model-base-url">{t(($) => $.models.base_url)}</Label>
          <Input
            id="model-base-url"
            type="url"
            required
            placeholder="https://api.example.com/v1"
            value={draft.base_url}
            onChange={(e) => setDraft({ ...draft, base_url: e.target.value })}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="model-upstream">{t(($) => $.models.model_id)}</Label>
          <Input
            id="model-upstream"
            required
            maxLength={200}
            value={draft.model}
            onChange={(e) => setDraft({ ...draft, model: e.target.value })}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="model-api">{t(($) => $.models.api)}</Label>
          <Select
            items={modelAPIs}
            value={draft.capabilities.api ?? "chat_completions"}
            onValueChange={(value) => {
              if (value === "chat_completions" || value === "responses") {
                setDraft({ ...draft, capabilities: { ...draft.capabilities, api: value } });
              }
            }}
          >
            <SelectTrigger id="model-api" className="w-full"><SelectValue /></SelectTrigger>
            <SelectContent>
              {modelAPIs.map(({ value, label }) => (
                <SelectItem key={value} value={value}>{label}</SelectItem>
              ))}
            </SelectContent>
          </Select>
          <p className="text-caption text-muted-foreground">{t(($) => $.models.api_help)}</p>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="model-api-key">{t(($) => $.models.api_key)}</Label>
          <Input
            id="model-api-key"
            type="password"
            autoComplete="new-password"
            disabled={clearKey}
            value={apiKey}
            onChange={(e) => setAPIKey(e.target.value)}
          />
          <p className="text-caption text-muted-foreground">
            {editing?.has_api_key
              ? t(($) => $.models.key_keep)
              : t(($) => $.models.key_help)}
          </p>
        </div>
        {editing?.has_api_key && (
          <label className="flex items-center gap-2 text-body">
            <input
              type="checkbox"
              checked={clearKey}
              onChange={(e) => setClearKey(e.target.checked)}
            />
            {t(($) => $.models.clear_key)}
          </label>
        )}
        <fieldset className="space-y-3">
          <legend className="mb-2 text-label font-medium">
            {t(($) => $.models.capabilities)}
          </legend>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.models.capabilities_help)}
          </p>
          <div className="grid grid-cols-2 gap-3">
            {(
              [
                ["context_tokens", t(($) => $.models.context_tokens)],
                ["max_output_tokens", t(($) => $.models.output_tokens)],
                ["max_tool_argument_bytes", t(($) => $.models.tool_bytes)],
                ["max_tools", t(($) => $.models.tools)],
              ] as const
            ).map(([key, label]) => (
              <div key={key} className="space-y-1.5">
                <Label htmlFor={`model-${key}`}>{label}</Label>
                <Input
                  id={`model-${key}`}
                  type="number"
                  required
                  min={1}
                  max={
                    key === "max_tools"
                      ? 128
                      : key === "max_tool_argument_bytes"
                        ? 1048576
                        : 10000000
                  }
                  value={draft.capabilities[key]}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      capabilities: {
                        ...draft.capabilities,
                        [key]: Number(e.target.value),
                      },
                    })
                  }
                />
              </div>
            ))}
          </div>
          <label className="flex items-center gap-2 text-body">
            <input
              type="checkbox"
              required
              checked={draft.capabilities.streaming_tool_calls}
              onChange={(e) =>
                setDraft({
                  ...draft,
                  capabilities: {
                    ...draft.capabilities,
                    streaming_tool_calls: e.target.checked,
                  },
                })
              }
            />
            {t(($) => $.models.streaming_tools)}
          </label>
          <label className="flex items-center gap-2 text-body">
            <input
              type="checkbox"
              checked={draft.capabilities.image_input}
              onChange={(e) =>
                setDraft({
                  ...draft,
                  capabilities: {
                    ...draft.capabilities,
                    image_input: e.target.checked,
                  },
                })
              }
            />
            {t(($) => $.models.images)}
          </label>
          <label className="flex items-center gap-2 text-body">
            <input
              type="checkbox"
              checked={draft.capabilities.json_object}
              onChange={(e) =>
                setDraft({
                  ...draft,
                  capabilities: {
                    ...draft.capabilities,
                    json_object: e.target.checked,
                  },
                })
              }
            />
            {t(($) => $.models.json)}
          </label>
        </fieldset>
        <label className="flex items-center gap-2 text-body">
          <input
            type="checkbox"
            checked={draft.is_default}
            onChange={(e) =>
              setDraft({
                ...draft,
                is_default: e.target.checked,
                ...(e.target.checked ? { enabled: true } : {}),
              })
            }
          />
          {t(($) => $.models.default)}
        </label>
        <label className="flex items-center gap-2 text-body">
          <input
            type="checkbox"
            disabled={draft.is_default}
            checked={draft.enabled}
            onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
          />
          {t(($) => $.models.enabled)}
        </label>
        {error && (
          <p role="alert" className="text-body text-destructive">
            {error}
          </p>
        )}
        <p className="text-caption text-muted-foreground">{t(($) => $.models.connection.help)}</p>
        <ModelConnectionTest
          wsId={wsId}
          modelId={editing?.id}
          config={draft}
          apiKey={clearKey ? "" : apiKey || undefined}
          disabled={save.isPending}
        />
        <DialogFooter>
          <DialogClose
            render={
              <Button
                type="button"
                variant="outline"
                disabled={save.isPending}
              />
            }
          >
            {t(($) => $.models.cancel)}
          </DialogClose>
          <Button
            type="submit"
            disabled={save.isPending}
            aria-busy={save.isPending}
          >
            {save.isPending
              ? t(($) => $.models.saving)
              : t(($) => $.models.save)}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!save.isPending) {
          setOpen(next);
          if (!next) setAPIKey("");
        }
      }}
    >
      <SettingsTab
        title={t(($) => $.models.title)}
        scope="workspace"
        actions={
          canManage ? (
            <DialogTrigger
              render={<Button onClick={() => startEditor(null)} />}
            >
              <Plus />
              {t(($) => $.models.add)}
            </DialogTrigger>
          ) : undefined
        }
      >
        {!canManage && <SettingsReadOnlyNotice wsId={wsId} />}
        {canManage && <p className="text-caption text-muted-foreground">{t(($) => $.models.connection.help)}</p>}
        {models.isPending ? (
          <p role="status">{t(($) => $.models.loading)}</p>
        ) : models.isError ? (
          <p role="alert">
            {t(($) => $.models.load_failed)}{" "}
            <Button variant="outline" onClick={() => models.refetch()}>
              {t(($) => $.models.retry)}
            </Button>
          </p>
        ) : !models.data?.length ? (
          <p className="text-body text-muted-foreground">
            {t(($) => $.models.empty)}
          </p>
        ) : (
          <SettingsCard>
            <ul className="divide-y divide-border">
              {models.data.map((model) => (
                <li
                  key={model.id}
                  className="flex flex-wrap items-center justify-between gap-3 p-4"
                >
                  <div className="min-w-0 flex-1">
                    <p className="break-words text-body font-medium">
                      {model.name}
                      {model.is_default && (
                        <span className="ml-2 text-caption text-muted-foreground">
                          {t(($) => $.models.default_badge)}
                        </span>
                      )}
                      {!model.enabled && (
                        <span className="ml-2 text-caption text-muted-foreground">
                          {t(($) => $.models.disabled)}
                        </span>
                      )}
                    </p>
                    <p className="break-all text-caption text-muted-foreground">
                      {model.model}
                    </p>
                  </div>
                  {canManage && (
                    <div className="flex flex-wrap items-start gap-2">
                      <ModelConnectionTest wsId={wsId} modelId={model.id} config={model} name={model.name} />
                      <DialogTrigger
                        render={
                          <Button
                            variant="outline"
                            onClick={() => startEditor(model)}
                            aria-label={`${t(($) => $.models.edit)} ${model.name}`}
                          />
                        }
                      >
                        {t(($) => $.models.edit)}
                      </DialogTrigger>
                      <Button
                        variant="ghost"
                        onClick={() => {
                          setDeleting(model);
                          setDeleteError("");
                        }}
                        aria-label={`${t(($) => $.models.delete)} ${model.name}`}
                      >
                        {t(($) => $.models.delete)}
                      </Button>
                    </div>
                  )}
                </li>
              ))}
            </ul>
          </SettingsCard>
        )}
      </SettingsTab>
      {editor}
      <AlertDialog
        open={!!deleting}
        onOpenChange={(next) => {
          if (!next && !remove.isPending) setDeleting(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.models.delete_title, { name: deleting?.name })}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.models.delete_help)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {deleteError && (
            <p role="alert" className="text-destructive">
              {deleteError}
            </p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={remove.isPending}>
              {t(($) => $.models.cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={remove.isPending}
              onClick={async (event) => {
                event.preventDefault();
                if (!deleting) return;
                try {
                  await remove.mutateAsync(deleting.id);
                  setDeleting(null);
                } catch (err) {
                  setDeleteError(
                    err instanceof Error
                      ? err.message
                      : t(($) => $.models.delete_failed),
                  );
                }
              }}
            >
              {t(($) => $.models.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Dialog>
  );
}
