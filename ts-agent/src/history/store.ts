import mysql, { type Pool, type RowDataPacket } from "mysql2/promise";
import { errorMessage } from "../util.js";

/**
 * One completed question inside a session. `question` is what the user asked;
 * `conclusion` is the report's root_cause. Raw tool output is deliberately not
 * stored here — replaying it would let a later turn answer from stale evidence.
 */
export interface SessionTurn {
  seq: number;
  event_id: string;
  mode: string;
  component: string;
  problem: string;
  cluster_id: string;
  question: string;
  conclusion: string;
  confidence: string;
  created_at: string;
}

export interface SessionSummary {
  id: string;
  title: string;
  turns: number;
  created_at: string;
  updated_at: string;
}

export interface NewTurn {
  sessionId: string;
  eventId: string;
  mode: string;
  component: string;
  problem: string;
  clusterId: string;
  question: string;
  conclusion: string;
  confidence: string;
}

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS diagnosis_session (
    id          VARCHAR(64)  PRIMARY KEY,
    user_id     VARCHAR(64)  NOT NULL,
    title       VARCHAR(128) NOT NULL DEFAULT '',
    turns       INT          NOT NULL DEFAULT 0,
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_user_updated (user_id, updated_at)
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
  `CREATE TABLE IF NOT EXISTS diagnosis_turn (
    id          BIGINT       AUTO_INCREMENT PRIMARY KEY,
    session_id  VARCHAR(64)  NOT NULL,
    seq         INT          NOT NULL,
    event_id    VARCHAR(64)  NOT NULL,
    mode        VARCHAR(16)  NOT NULL,
    component   VARCHAR(32)  NOT NULL,
    problem     VARCHAR(64)  NOT NULL,
    cluster_id  VARCHAR(128) NOT NULL DEFAULT '',
    question    TEXT         NOT NULL,
    conclusion  TEXT         NOT NULL,
    confidence  VARCHAR(16)  NOT NULL,
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_event (event_id),
    KEY idx_session_seq (session_id, seq),
    CONSTRAINT fk_turn_session FOREIGN KEY (session_id)
      REFERENCES diagnosis_session(id) ON DELETE CASCADE
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
];

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function isoTimestamp(value: unknown): string {
  if (value instanceof Date) return value.toISOString();
  return String(value ?? "");
}

/**
 * MySQL-backed session store for the console. Absent a URL the store stays
 * disabled and the agent runs exactly as before (single-shot, no history).
 *
 * Every read that returns session content filters on `userId`: the id is
 * client-supplied, so an unguessable id must not be the only thing standing
 * between two users' sessions.
 *
 * All methods swallow database errors into `null`/`false` and log once via the
 * supplied callback. A console feature must never turn a diagnosis into a 500.
 */
export class HistoryStore {
  private readonly pool: Pool;
  private readonly maxTurns: number;
  private closed = false;

  private constructor(
    pool: Pool,
    maxTurns: number,
    private readonly log: (message: string) => void,
  ) {
    this.pool = pool;
    this.maxTurns = maxTurns;
  }

  /** Returns undefined when no URL is configured or the database is unreachable. */
  static async open(
    url: string | undefined,
    options: { poolSize: number; maxTurns: number; connectTimeoutMs: number },
    log: (message: string) => void,
  ): Promise<HistoryStore | undefined> {
    if (!url) {
      log("history: disabled (TS_AGENT_MYSQL_URL unset)");
      return undefined;
    }
    const pool = mysql.createPool({
      uri: url,
      connectionLimit: options.poolSize,
      connectTimeout: options.connectTimeoutMs,
      waitForConnections: true,
      timezone: "Z",
    });
    try {
      for (const statement of SCHEMA) await pool.query(statement);
      await pool.query("SELECT 1");
    } catch (error) {
      log(`history: disabled (${errorMessage(error)})`);
      await pool.end().catch(() => undefined);
      return undefined;
    }
    log("history: mysql connected");
    return new HistoryStore(pool, options.maxTurns, log);
  }

  async createSession(userId: string, title = ""): Promise<SessionSummary | undefined> {
    const id = `ses_${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
    try {
      await this.pool.execute(
        "INSERT INTO diagnosis_session (id, user_id, title) VALUES (?, ?, ?)",
        [id, userId, title.slice(0, 128)],
      );
      return { id, title, turns: 0, created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
    } catch (error) {
      this.log(`history: createSession failed: ${errorMessage(error)}`);
      return undefined;
    }
  }

  async listSessions(userId: string, limit = 20): Promise<SessionSummary[]> {
    try {
      const [rows] = await this.pool.execute<RowDataPacket[]>(
        `SELECT id, title, turns, created_at, updated_at FROM diagnosis_session
          WHERE user_id = ? ORDER BY updated_at DESC LIMIT ?`,
        [userId, Math.min(Math.max(limit, 1), 100)],
      );
      return rows.map((row) => ({
        id: str(row.id),
        title: str(row.title),
        turns: Number(row.turns ?? 0),
        created_at: isoTimestamp(row.created_at),
        updated_at: isoTimestamp(row.updated_at),
      }));
    } catch (error) {
      this.log(`history: listSessions failed: ${errorMessage(error)}`);
      return [];
    }
  }

  async getTurns(userId: string, sessionId: string, limit = 50): Promise<SessionTurn[]> {
    try {
      const [rows] = await this.pool.execute<RowDataPacket[]>(
        `SELECT t.seq, t.event_id, t.mode, t.component, t.problem, t.cluster_id,
                t.question, t.conclusion, t.confidence, t.created_at
           FROM diagnosis_turn t
           JOIN diagnosis_session s ON s.id = t.session_id
          WHERE t.session_id = ? AND s.user_id = ?
          ORDER BY t.seq DESC LIMIT ?`,
        [sessionId, userId, Math.min(Math.max(limit, 1), 200)],
      );
      return rows
        .map((row) => ({
          seq: Number(row.seq ?? 0),
          event_id: str(row.event_id),
          mode: str(row.mode),
          component: str(row.component),
          problem: str(row.problem),
          cluster_id: str(row.cluster_id),
          question: str(row.question),
          conclusion: str(row.conclusion),
          confidence: str(row.confidence),
          created_at: isoTimestamp(row.created_at),
        }))
        .reverse();
    } catch (error) {
      this.log(`history: getTurns failed: ${errorMessage(error)}`);
      return [];
    }
  }

  /** Existence check that also enforces ownership. */
  async ownsSession(userId: string, sessionId: string): Promise<boolean> {
    try {
      const [rows] = await this.pool.execute<RowDataPacket[]>(
        "SELECT 1 AS ok FROM diagnosis_session WHERE id = ? AND user_id = ?",
        [sessionId, userId],
      );
      return rows.length > 0;
    } catch (error) {
      this.log(`history: ownsSession failed: ${errorMessage(error)}`);
      return false;
    }
  }

  async deleteSession(userId: string, sessionId: string): Promise<boolean> {
    try {
      const [result] = await this.pool.execute(
        "DELETE FROM diagnosis_session WHERE id = ? AND user_id = ?",
        [sessionId, userId],
      );
      return Number((result as { affectedRows?: number }).affectedRows ?? 0) > 0;
    } catch (error) {
      this.log(`history: deleteSession failed: ${errorMessage(error)}`);
      return false;
    }
  }

  /**
   * The last `maxTurns` conclusions of a session, oldest first, as the context
   * block handed to the model. Returns [] when the session is not the caller's.
   */
  async contextFor(userId: string, sessionId: string): Promise<SessionTurn[]> {
    const turns = await this.getTurns(userId, sessionId, this.maxTurns);
    return turns;
  }

  async appendTurn(turn: NewTurn): Promise<boolean> {
    const conn = await this.pool.getConnection().catch(() => undefined);
    if (!conn) return false;
    try {
      await conn.beginTransaction();
      const [rows] = await conn.execute<RowDataPacket[]>(
        "SELECT turns FROM diagnosis_session WHERE id = ? FOR UPDATE",
        [turn.sessionId],
      );
      const current = rows[0];
      if (!current) {
        await conn.rollback();
        return false;
      }
      const seq = Number(current.turns ?? 0) + 1;
      await conn.execute(
        `INSERT INTO diagnosis_turn
           (session_id, seq, event_id, mode, component, problem, cluster_id,
            question, conclusion, confidence)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        [
          turn.sessionId,
          seq,
          turn.eventId,
          turn.mode,
          turn.component,
          turn.problem,
          turn.clusterId,
          turn.question,
          turn.conclusion,
          turn.confidence,
        ],
      );
      // The first question names the session, like a chat title.
      await conn.execute(
        `UPDATE diagnosis_session
            SET turns = ?,
                title = CASE WHEN title = '' THEN ? ELSE title END
          WHERE id = ?`,
        [seq, turn.question.slice(0, 120), turn.sessionId],
      );
      await conn.commit();
      return true;
    } catch (error) {
      await conn.rollback().catch(() => undefined);
      this.log(`history: appendTurn failed: ${errorMessage(error)}`);
      return false;
    } finally {
      conn.release();
    }
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    await this.pool.end().catch(() => undefined);
  }
}
