"use client";

import { useId } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  runtimeModelsOptions,
  type RuntimeModelScope,
} from "@multica/core/runtimes";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";

export function ModelDropdown({
  runtimeId,
  runtimeProvider,
  runtimeOnline,
  value,
  onChange,
  workspace,
  disabled = false,
}: {
  runtimeId: string | null;
  runtimeProvider?: string;
  runtimeOnline: boolean;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  workspace?: RuntimeModelScope;
}) {
  const { t } = useT("agents");
  const id = useId();
  const native = !runtimeProvider || runtimeProvider === "native";
  const query = useQuery(
    runtimeModelsOptions(native && runtimeOnline ? runtimeId : null, workspace),
  );
  const models = query.data?.models ?? [];
  const defaultModel = models.find((model) => model.default);
  const defaultLabel =
    defaultModel?.label || t(($) => $.pickers.model_managed_by_runtime);
  const items = [
    {
      value: "",
      label: t(($) => $.model_dropdown.workspace_default, {
        model: defaultLabel,
      }),
    },
    ...models.map((model) => ({ value: model.id, label: model.label })),
  ];
  if (value && !models.some((model) => model.id === value))
    items.push({ value, label: value });
  if (query.isError)
    return (
      <p role="status" className="text-body text-muted-foreground">
        {t(($) => $.model_dropdown.deployment_unavailable)}
      </p>
    );
  if (!native || query.data?.supported === false)
    return (
      <div className="text-body text-muted-foreground">
        {value || defaultLabel}
      </div>
    );
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <Label htmlFor={id} className="text-caption text-muted-foreground">
        {t(($) => $.model_dropdown.label)}
      </Label>
      <Select
        items={items}
        value={value}
        onValueChange={(next) => {
          if (next !== null) onChange(next);
        }}
        disabled={disabled || query.isPending || models.length === 0}
      >
        <SelectTrigger id={id} className="w-full min-w-0">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {items.map((item) => (
            <SelectItem key={item.value} value={item.value}>
              {item.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
