-- DiagFlow TS agent: console sessions.
--
-- A session is an explicit container for a sequence of questions. Turns inside
-- one session share a conclusion summary; sessions never see each other. The
-- user is anonymous for now (a UUID minted by the browser and stored in
-- localStorage), so user_id is a client-generated string, not a verified
-- identity.

CREATE TABLE IF NOT EXISTS diagnosis_session (
    id          VARCHAR(64)  PRIMARY KEY,
    user_id     VARCHAR(64)  NOT NULL,
    title       VARCHAR(128) NOT NULL DEFAULT '',
    turns       INT          NOT NULL DEFAULT 0,
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_user_updated (user_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS diagnosis_turn (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
