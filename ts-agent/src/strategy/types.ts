export type StepAction = "fingerprint_match" | "tool_call" | "llm_decide";

export interface DecideChoice {
  value: string;
  description?: string;
}

export interface Step {
  action: StepAction;
  description?: string;
  tool?: string;
  params: Record<string, unknown>;
  priority: number;
  decide_prompt?: string;
  decide_choices?: DecideChoice[];
  if_decision?: string;
}

export interface Strategy {
  component: string;
  problemType: string;
  version?: string;
  description?: string;
  steps: Step[];
  knowledgeBase?: Record<string, unknown>;
  validation?: Record<string, unknown>;
  output?: Record<string, unknown>;
  source: string;
}
