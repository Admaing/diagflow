export interface ToolResult {
  success: boolean;
  data: string;
  error?: string;
}

export interface ToolCallOptions {
  clusterId?: string;
}

export interface ToolExecutor {
  call(tool: string, params: Record<string, unknown>, options?: ToolCallOptions): Promise<ToolResult>;
  close(): Promise<void>;
}
