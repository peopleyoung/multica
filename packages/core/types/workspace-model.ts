export interface ModelCapabilities {
	api?: "chat_completions" | "responses";
  streaming_tool_calls: boolean;
  json_object: boolean;
  context_tokens: number;
  max_output_tokens: number;
  max_tool_argument_bytes: number;
  max_tools: number;
  image_input: boolean;
}

export interface WorkspaceModel {
  id: string;
  name: string;
  base_url: string;
  model: string;
  has_api_key: boolean;
  capabilities: ModelCapabilities;
  is_default: boolean;
  enabled: boolean;
}

export type ModelConnectionInput = Pick<WorkspaceModel, "base_url" | "model" | "capabilities"> & { api_key?: string };

export interface ModelConnectionResult {
  ok: boolean;
  stage: "text" | "tool_call" | "tool_result" | "complete" | "unknown";
  code: string;
  duration_ms: number;
}

export type WorkspaceModelInput = Omit<WorkspaceModel, "id" | "has_api_key"> & {
  api_key?: string;
};
