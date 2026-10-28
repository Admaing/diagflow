import fs from "node:fs";
import path from "node:path";
import { parse } from "yaml";
import { PACKAGE_ROOT } from "../config.js";
import type { DecideChoice, Step, Strategy } from "./types.js";

const DEFAULT_STRATEGIES_DIR = path.resolve(PACKAGE_ROOT, "..", "data", "strategies");

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function asString(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function normalizeStep(raw: unknown): Step {
  const value = asRecord(raw);
  const action = value.action === "fingerprint_match" || value.action === "llm_decide"
    ? value.action
    : "tool_call";
  const priority = typeof value.priority === "number" ? value.priority : 10;
  const choices = Array.isArray(value.decide_choices)
    ? value.decide_choices
        .map((item): DecideChoice | undefined => {
          const choice = asRecord(item);
          const choiceValue = asString(choice.value);
          if (!choiceValue) return undefined;
          return {
            value: choiceValue,
            ...(asString(choice.description) ? { description: asString(choice.description) } : {}),
          };
        })
        .filter((item): item is DecideChoice => item !== undefined)
    : undefined;

  return {
    action,
    ...(asString(value.description) ? { description: asString(value.description) } : {}),
    ...(asString(value.tool) ? { tool: asString(value.tool) } : {}),
    params: asRecord(value.params),
    priority,
    ...(asString(value.decide_prompt) ? { decide_prompt: asString(value.decide_prompt) } : {}),
    ...(choices && choices.length > 0 ? { decide_choices: choices } : {}),
    ...(asString(value.if_decision) ? { if_decision: asString(value.if_decision) } : {}),
  };
}

function parseFile(file: string, component: string, problemType: string): Strategy {
  const raw = asRecord(parse(fs.readFileSync(file, "utf8")) as unknown);
  const steps = Array.isArray(raw.steps) ? raw.steps.map(normalizeStep) : [];
  return {
    component: asString(raw.component) ?? component,
    problemType: asString(raw.problem_type) ?? problemType,
    ...(asString(raw.version) ? { version: asString(raw.version) } : {}),
    ...(asString(raw.description) ? { description: asString(raw.description) } : {}),
    steps,
    knowledgeBase: asRecord(raw.knowledge_base),
    validation: asRecord(raw.validation),
    output: asRecord(raw.output),
    source: file,
  };
}

const COMPONENT_LOG_DIRS: Array<{ match: string; dir: string }> = [
  { match: "hadoop-yarn", dir: "/var/log/hadoop-yarn" },
  { match: "resourcemanager", dir: "/var/log/hadoop-yarn" },
  { match: "nodemanager", dir: "/var/log/hadoop-yarn" },
  { match: "yarn", dir: "/var/log/hadoop-yarn" },
  { match: "hadoop-hdfs", dir: "/var/log/hadoop-hdfs" },
  { match: "namenode", dir: "/var/log/hadoop-hdfs" },
  { match: "datanode", dir: "/var/log/hadoop-hdfs" },
  { match: "journalnode", dir: "/var/log/hadoop-hdfs" },
  { match: "hdfs", dir: "/var/log/hadoop-hdfs" },
  { match: "mapreduce", dir: "/var/log/hadoop-mapreduce" },
  { match: "hive", dir: "/var/log/hive" },
  { match: "hbase", dir: "/var/log/hbase" },
  { match: "spark", dir: "/var/log/spark" },
  { match: "zookeeper", dir: "/var/log/zookeeper" },
  { match: "flink", dir: "/var/log/flink" },
  { match: "hue", dir: "/var/log/hue" },
  { match: "livy", dir: "/var/log/livy" },
  { match: "hadoop", dir: "/var/log/hadoop-hdfs" },
];

function logDirForComponent(component: string): string | undefined {
  const value = component.toLowerCase().trim();
  return COMPONENT_LOG_DIRS.find((entry) => value.includes(entry.match))?.dir;
}

/**
 * Component-agnostic fallback: inspect the cluster, discover logs that exist,
 * then grep the component's own log directory. Mirrors the Go defaultStrategy.
 */
export function defaultStrategy(component: string, problemType: string): Strategy {
  const steps: Step[] = [
    {
      action: "fingerprint_match",
      description: "Check known issues first",
      params: {},
      priority: 0,
    },
    {
      action: "tool_call",
      tool: "get_cluster_info",
      description: "Inspect cluster state and installed services",
      params: {},
      priority: 1,
    },
    {
      action: "tool_call",
      tool: "list_logs",
      description: "Discover log files instead of guessing paths",
      params: {},
      priority: 2,
    },
  ];

  const logDir = logDirForComponent(component);
  if (logDir) {
    steps.push({
      action: "tool_call",
      tool: "search_logs",
      description: "Grep the component's own log directory for error signatures",
      params: { log_path: `${logDir}/*.log`, keywords: "ERROR|Exception|FATAL", roles: "master,core" },
      priority: 2,
    });
  }

  steps.push({
    action: "tool_call",
    tool: "query_metrics",
    description: "Check resource metrics",
    params: {},
    priority: 3,
  });

  return {
    component,
    problemType,
    steps,
    knowledgeBase: { fingerprint: true, semantic_search: true, bm25_search: true },
    validation: { min_evidence_count: 1 },
    output: { suggestions_min: 2, suggestions_max: 5 },
    source: "default",
  };
}

export function loadStrategy(
  component: string,
  problemType: string,
  strategiesDir = process.env.TS_AGENT_STRATEGIES_DIR ?? DEFAULT_STRATEGIES_DIR,
): Strategy {
  for (const file of [
    path.join(strategiesDir, `${component}_${problemType}.yaml`),
    path.join(strategiesDir, `${component}_default.yaml`),
  ]) {
    if (fs.existsSync(file)) {
      try {
        return parseFile(file, component, problemType);
      } catch {
        break;
      }
    }
  }
  return defaultStrategy(component, problemType);
}

/** Stable sort by priority, then group equal priorities into parallel batches. */
export function groupByPriority(steps: Step[]): Step[][] {
  const sorted = [...steps].sort((a, b) => a.priority - b.priority);
  const batches: Step[][] = [];
  for (const step of sorted) {
    const last = batches.at(-1);
    if (last && last[0]?.priority === step.priority) last.push(step);
    else batches.push([step]);
  }
  return batches;
}

export function shouldRun(step: Step, currentDecision: string): boolean {
  return !step.if_decision || step.if_decision === currentDecision;
}

const TEMPLATE = /\{\{\s*context\.(\w+)\s*\}\}/g;

function renderValue(value: unknown, context: Record<string, unknown>): unknown {
  if (typeof value === "string") {
    return value.replace(TEMPLATE, (_match, key: string) => {
      const resolved = context[key];
      return resolved === undefined || resolved === null ? "" : String(resolved);
    });
  }
  if (Array.isArray(value)) return value.map((item) => renderValue(item, context));
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
      out[key] = renderValue(item, context);
    }
    return out;
  }
  return value;
}

export function renderParams(step: Step, context: Record<string, unknown>): Record<string, unknown> {
  return renderValue(step.params, context) as Record<string, unknown>;
}

export function buildTaskPrompt(strategy: Strategy, context: Record<string, string>): string {
  return [
    `Diagnose a ${strategy.component} issue.`,
    `Problem: ${context.problem ?? "unknown"}`,
    `Cluster: ${context.cluster_id ?? "unknown"}`,
    `Region: ${context.region ?? "unknown"}`,
    `Version: ${context.version ?? "unknown"}`,
    `Detail: ${context.detail ?? ""}`,
    "",
    "Strategy-driven evidence collection has already run. Analyze the evidence",
    "collected so far, form a root cause hypothesis, and use the available tools",
    "to fill only the gaps. Then produce a structured conclusion.",
  ].join("\n");
}
