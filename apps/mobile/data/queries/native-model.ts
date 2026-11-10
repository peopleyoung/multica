import { queryOptions } from "@tanstack/react-query";
import { api } from "@/data/api";

export const nativeModelKeys = {
  all: (wsId: string | null) => ["native-model", wsId] as const,
};

export const nativeModelOptions = (wsId: string | null, runtimeId: string | null) =>
  queryOptions({
    queryKey: [...nativeModelKeys.all(wsId), runtimeId] as const,
    queryFn: async ({ signal }) => {
      const response = await api.nativeModel(runtimeId!, { signal });
      if (response.status !== "completed" || response.supported !== false) {
        throw new Error(response.error || "Native deployment model unavailable");
      }
      const model = response.models?.find((item) => item.default === true);
      if (!model) throw new Error("Native deployment model unavailable");
      return model.label || model.id;
    },
    enabled: !!wsId && !!runtimeId,
    staleTime: 60_000,
  });
