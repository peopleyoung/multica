import { useQuery } from "@tanstack/react-query";
import { Text } from "@/components/ui/text";
import { nativeModelOptions } from "@/data/queries/native-model";
import { runtimeListOptions } from "@/data/queries/runtimes";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useT } from "@/lib/i18n";

export function AgentRuntimeLabel({ runtimeId }: { runtimeId: string | null }) {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { t } = useT("workspace");
  const { data: runtimes } = useQuery(runtimeListOptions(wsId));
  const runtime = runtimes?.find((item) => item.id === runtimeId);
  const native = runtime?.provider === "native";
  const model = useQuery(nativeModelOptions(wsId, native ? runtimeId : null));
  const label = native
    ? model.isError ? t("agent.model_unavailable") : model.data || t("agent.managed_model")
    : t("agent.needs_migration");
  return (
    <Text className="text-xs text-muted-foreground mt-0.5" numberOfLines={2}>
      {runtime?.name || runtime?.device_info || t("agent.machine_unavailable")} · {label}
    </Text>
  );
}
