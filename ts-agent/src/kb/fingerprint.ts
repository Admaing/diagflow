import crypto from "node:crypto";

export interface KnownCase {
  fingerprint: string;
  component: string;
  errorPattern: string;
  version: string;
  rootCause: string;
  suggestions: string[];
  disputed: boolean;
  hits: number;
}

/**
 * Process-scoped fingerprint knowledge base. Mirrors the Go KB's exact-match
 * path: MD5 over `component:error_pattern:version`, no embeddings required.
 * It is intentionally not durable.
 */
export class FingerprintKB {
  private readonly cases = new Map<string, KnownCase>();

  static fingerprint(component: string, errorPattern: string, version: string): string {
    return crypto
      .createHash("md5")
      .update(`${component}:${errorPattern}:${version}`)
      .digest("hex")
      .slice(0, 16);
  }

  match(component: string, errorPattern: string, version = ""): KnownCase | undefined {
    if (!errorPattern.trim()) return undefined;
    const hit = this.cases.get(FingerprintKB.fingerprint(component, errorPattern, version));
    if (!hit || hit.disputed) return undefined;
    hit.hits++;
    return hit;
  }

  add(
    component: string,
    errorPattern: string,
    version: string,
    rootCause: string,
    suggestions: string[],
  ): string {
    const fingerprint = FingerprintKB.fingerprint(component, errorPattern, version);
    this.cases.set(fingerprint, {
      fingerprint,
      component,
      errorPattern,
      version,
      rootCause,
      suggestions: [...suggestions],
      disputed: false,
      hits: 0,
    });
    return fingerprint;
  }

  markIncorrect(fingerprint: string): void {
    const hit = this.cases.get(fingerprint);
    if (hit) hit.disputed = true;
  }

  get size(): number {
    return this.cases.size;
  }
}
