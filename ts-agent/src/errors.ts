export class NoConclusionError extends Error {
  readonly code = "no_conclusion";

  constructor(message: string) {
    super(message);
    this.name = "NoConclusionError";
  }
}

export class LlmUnavailableError extends Error {
  readonly code = "llm_unavailable";

  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "LlmUnavailableError";
  }
}

export class StageTimeoutError extends Error {
  readonly code = "stage_timeout";

  constructor(message: string) {
    super(message);
    this.name = "StageTimeoutError";
  }
}

export class ConfigError extends Error {
  readonly code = "config_error";

  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}
