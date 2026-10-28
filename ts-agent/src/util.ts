export function truncate(value: string, max: number): string {
  if (max <= 0 || value.length <= max) return value;
  return `${value.slice(0, max)}…`;
}

const SENTENCE_END = /[。．.！!？?；;\n]/;

/**
 * One-line summary for evidence rows: the first sentence of `text`, capped at
 * `max` characters. Returning the first sentence instead of a truncated prefix
 * keeps `summary` and `detail` visually distinct — a prefix of the detail is
 * what made the console show every evidence item twice.
 */
export function firstSentence(text: string, max = 120): string {
  const normalized = text.replace(/\s+/g, " ").trim();
  if (!normalized) return "";
  const match = SENTENCE_END.exec(normalized);
  const sentence = match ? normalized.slice(0, match.index + 1) : normalized;
  return truncate(sentence, max);
}

/** True when a summary adds nothing over its detail (same text, or ignored only by trailing punctuation). */
export function isRedundantSummary(summary: string, detail: string | undefined): boolean {
  if (!detail) return false;
  const strip = (value: string) => value.replace(/[\s…。．.]+$/u, "");
  return strip(summary) === strip(detail);
}

export function safeJson(value: unknown): string {
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}

export function newEventId(prefix = "ts-diag"): string {
  const suffix = Math.random().toString(36).slice(2, 10);
  return `${prefix}-${Date.now()}-${suffix}`;
}

export function stripCodeFence(text: string): string {
  const fenced = text.match(/```(?:json)?\s*([\s\S]*?)```/i);
  return fenced?.[1]?.trim() ?? text.trim();
}

/** Find the first balanced JSON object in a model response. */
export function extractJsonObject(text: string): unknown {
  const source = stripCodeFence(text);
  for (let start = 0; start < source.length; start++) {
    if (source[start] !== "{") continue;
    let depth = 0;
    let inString = false;
    let escaped = false;
    for (let i = start; i < source.length; i++) {
      const ch = source[i];
      if (escaped) {
        escaped = false;
        continue;
      }
      if (ch === "\\") {
        escaped = true;
        continue;
      }
      if (ch === '"') {
        inString = !inString;
        continue;
      }
      if (inString) continue;
      if (ch === "{") depth++;
      if (ch === "}") {
        depth--;
        if (depth === 0) {
          try {
            return JSON.parse(source.slice(start, i + 1));
          } catch {
            break;
          }
        }
      }
    }
  }
  return undefined;
}
