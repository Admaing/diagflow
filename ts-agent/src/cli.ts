#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { hasLlmCredential, loadConfig, validateConfig } from "./config.js";
import type { EventSink } from "./events.js";
import type { InvestigationInput } from "./report/types.js";
import { createPiRuntime } from "./runtime/pi.js";
import { errorMessage } from "./util.js";
import { runInvestigation } from "./workflow/investigate.js";

function usage(): string {
  return `DiagFlow TS agent — intent-driven monitoring investigation

Usage:
  npm run investigate -- --component flink --problem job_failure \\
    [--cluster uhadoop-xxxx] [--detail "Job FAILED"] [--job application_...] \\
    [--alert alerts.json] [--context '{"version":"1.17"}'] [--json]

Options:
  --component  Component: flink, hdfs, yarn, spark, hive, kafka
  --problem    Problem type, e.g. job_failure, checkpoint_failure, disk_full
  --cluster    UHadoop cluster instance id (production)
  --detail     Free-text symptom description
  --job        Job / application id
  --alert      Alert JSON string or path to a JSON file
  --context    Extra context JSON string
  --json       Print the report as JSON instead of Markdown
  --help       Show this help
`;
}

function parseJsonArg(value: string, label: string): unknown {
  if (value.trim().startsWith("{") || value.trim().startsWith("[")) {
    return JSON.parse(value);
  }
  const file = path.resolve(process.cwd(), value);
  const raw = fs.readFileSync(file, "utf8");
  try {
    return JSON.parse(raw) as unknown;
  } catch (error) {
    throw new Error(`${label}: ${file} is not valid JSON: ${errorMessage(error)}`);
  }
}

async function main(): Promise<void> {
  const { values } = parseArgs({
    options: {
      component: { type: "string" },
      problem: { type: "string" },
      cluster: { type: "string" },
      detail: { type: "string" },
      job: { type: "string" },
      alert: { type: "string" },
      context: { type: "string" },
      json: { type: "boolean", default: false },
      help: { type: "boolean", default: false },
    },
    strict: true,
  });

  if (values.help) {
    process.stdout.write(usage());
    return;
  }

  const component = values.component?.trim();
  const problem = values.problem?.trim();
  if (!component || !problem) {
    process.stderr.write(usage());
    process.exitCode = 2;
    return;
  }

  const config = loadConfig();
  const problems = validateConfig(config);
  if (problems.length > 0) {
    for (const problem of problems) process.stderr.write(`config: ${problem}\n`);
    process.exitCode = 2;
    return;
  }

  if (!hasLlmCredential(config)) {
    process.stderr.write(
      "warning: no LLM credential found (DEEPSEEK_API_KEY / TS_AGENT_LLM_API_KEY / ~/.pi/agent/auth.json)\n",
    );
  }

  const input: InvestigationInput = {
    component,
    problem,
    ...(values.cluster ? { clusterId: values.cluster } : {}),
    ...(values.detail ? { detail: values.detail } : {}),
    ...(values.job ? { jobId: values.job } : {}),
    ...(values.alert ? { alert: parseJsonArg(values.alert, "--alert") as Record<string, unknown> } : {}),
    ...(values.context
      ? { context: parseJsonArg(values.context, "--context") as Record<string, unknown> }
      : {}),
  };

  const sink: EventSink = (event) => {
    if (event.type === "step") {
      process.stderr.write(`[${event.kind}] ${event.text}\n`);
    } else if (event.type === "failed") {
      process.stderr.write(`[failed] ${event.code}: ${event.message}\n`);
    }
  };

  const runtime = await createPiRuntime(config, sink);
  try {
    const { report, markdown } = await runInvestigation(runtime, input, sink);
    process.stdout.write(values.json ? `${JSON.stringify(report, null, 2)}\n` : markdown);
  } catch (error) {
    process.stderr.write(`error: ${errorMessage(error)}\n`);
    process.exitCode = 1;
  } finally {
    await runtime.close();
  }
}

main()
  .then(() => process.exit(process.exitCode ?? 0))
  .catch((error) => {
    process.stderr.write(`fatal: ${errorMessage(error)}\n`);
    process.exit(1);
  });
