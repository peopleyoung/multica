import { ActivityIndicator, FlatList, RefreshControl, View } from "react-native";
import { useQuery } from "@tanstack/react-query";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { ActorAvatar } from "@/components/ui/actor-avatar";
import { AgentRuntimeLabel } from "@/components/chat/agent-runtime-label";
import { agentListOptions } from "@/data/queries/agents";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useT } from "@/lib/i18n";

export default function AgentsPage() {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { t } = useT("workspace");
  const query = useQuery(agentListOptions(wsId));
  if (query.isLoading) return <View className="flex-1 items-center justify-center bg-background"><ActivityIndicator /></View>;
  if (query.isError) return (
    <View className="flex-1 items-center justify-center gap-3 bg-background px-6">
      <Text className="text-sm text-destructive">{t("errors.load_failed", { message: query.error.message })}</Text>
      <Button variant="outline" onPress={() => query.refetch()}><Text>{t("actions.retry")}</Text></Button>
    </View>
  );
  return (
    <FlatList
      className="flex-1 bg-background"
      data={query.data?.filter((agent) => !agent.archived_at) ?? []}
      keyExtractor={(agent) => agent.id}
      refreshControl={<RefreshControl refreshing={query.isRefetching} onRefresh={query.refetch} />}
      contentContainerClassName="pb-6"
      ListEmptyComponent={<Text className="p-6 text-center text-sm text-muted-foreground">{t("agent.empty")}</Text>}
      renderItem={({ item }) => (
        <View className="flex-row items-center gap-3 px-4 py-3">
          <ActorAvatar type="agent" id={item.id} size={32} showPresence />
          <View className="flex-1">
            <Text className="text-sm font-medium text-foreground" numberOfLines={1}>{item.name}</Text>
            <AgentRuntimeLabel runtimeId={item.runtime_id ?? null} />
          </View>
        </View>
      )}
    />
  );
}
