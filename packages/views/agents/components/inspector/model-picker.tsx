"use client";

import { useState } from "react";
import { toast } from "sonner";
import { useT } from "../../../i18n";
import { ModelDropdown } from "../model-dropdown";

export function ModelPicker({
  runtimeId,
  runtimeProvider,
  runtimeOnline,
  value,
  canEdit = true,
  onChange,
}: {
  runtimeId: string | null;
  runtimeProvider?: string;
  runtimeOnline: boolean;
  value: string;
  canEdit?: boolean;
  variant?: "chip" | "field";
  showLabel?: boolean;
  onChange: (next: string) => Promise<void> | void;
}) {
  const { t } = useT("agents");
  const [saving, setSaving] = useState(false);
  return (
    <ModelDropdown
      runtimeId={runtimeId}
      runtimeProvider={runtimeProvider}
      runtimeOnline={runtimeOnline}
      value={value}
      disabled={!canEdit || saving}
      onChange={async (next) => {
        setSaving(true);
        try {
          await onChange(next);
        } catch (error) {
          toast.error(
            error instanceof Error
              ? error.message
              : t(($) => $.model_dropdown.save_failed),
          );
        } finally {
          setSaving(false);
        }
      }}
    />
  );
}
