-- DiagFlow MySQL Schema
-- 诊断历史 + 用户反馈 + 集群画像 + 策略执行日志

CREATE TABLE IF NOT EXISTS diagnosis_history (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_id        VARCHAR(64) NOT NULL UNIQUE COMMENT '诊断事件唯一ID',
    session_id      VARCHAR(64) DEFAULT '' COMMENT '会话ID(同一用户多轮对话)',
    component       VARCHAR(32) NOT NULL COMMENT 'flink/hdfs/yarn/kafka',
    problem_type    VARCHAR(64) NOT NULL COMMENT '策略文件名',
    cluster_id      VARCHAR(128) DEFAULT '' COMMENT 'UHadoop集群ID',
    region          VARCHAR(32) DEFAULT '' COMMENT '地域',
    version         VARCHAR(32) DEFAULT '' COMMENT '组件版本',
    root_cause      TEXT COMMENT '根因结论',
    confidence      ENUM('high','medium','low') NOT NULL,
    suggestions     JSON COMMENT '修复建议列表',
    evidence_count  INT DEFAULT 0 COMMENT '证据条数',
    kb_matched      TINYINT(1) DEFAULT 0 COMMENT '1=KB命中(0 LLM)',
    kb_match_phase  VARCHAR(32) DEFAULT '' COMMENT 'phase1_semantic/phase2.5_fingerprint',
    phases_run      VARCHAR(128) DEFAULT '' COMMENT '实际执行的phases',
    duration_ms     INT COMMENT '诊断耗时(ms)',
    llm_calls       INT DEFAULT 0 COMMENT 'LLM调用次数',
    error_msg       TEXT COMMENT '诊断失败原因',
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,

    INDEX idx_component (component),
    INDEX idx_session (session_id, id),
    INDEX idx_confidence (confidence),
    INDEX idx_kb_matched (kb_matched),
    INDEX idx_cluster (cluster_id),
    INDEX idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;


CREATE TABLE IF NOT EXISTS diagnosis_feedback (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_id        VARCHAR(64) NOT NULL UNIQUE COMMENT '关联 diagnosis_history.event_id',
    is_correct      TINYINT(1) COMMENT '1=正确, 0=错误',
    comment         TEXT COMMENT '用户备注',
    corrected_by    VARCHAR(64) COMMENT '操作人',
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_is_correct (is_correct),
    INDEX idx_created (created_at),
    FOREIGN KEY (event_id) REFERENCES diagnosis_history(event_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;


CREATE TABLE IF NOT EXISTS cluster_profile (
    cluster_id      VARCHAR(128) NOT NULL COMMENT 'UHadoop集群ID',
    component       VARCHAR(32) NOT NULL COMMENT 'flink/hdfs/yarn/...',
    version         VARCHAR(32) DEFAULT '',
    total_diag      INT DEFAULT 0 COMMENT '诊断总次数',
    last_diag_at    DATETIME COMMENT '最近诊断时间',
    common_issues   JSON COMMENT 'Top-N故障类型 [{"problem_type":"...","count":N}]',
    kb_hit_rate     DECIMAL(5,2) COMMENT 'KB命中率(%)',
    avg_duration_ms INT COMMENT '平均诊断耗时(ms)',
    node_count      INT DEFAULT 0 COMMENT '节点数',
    installed_apps  JSON COMMENT '已安装组件列表',
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    PRIMARY KEY (cluster_id, component)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;


CREATE TABLE IF NOT EXISTS strategy_execution_log (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_id        VARCHAR(64) NOT NULL COMMENT '关联 diagnosis_history.event_id',
    phase           VARCHAR(16) NOT NULL COMMENT 'phase_1/phase_2/phase_3/phase_4/phase_5',
    tool_name       VARCHAR(64) DEFAULT '' COMMENT '工具名',
    action          VARCHAR(128) DEFAULT '' COMMENT '工具action',
    status          ENUM('success','error','timeout','skipped') NOT NULL,
    duration_ms     INT COMMENT '步骤耗时(ms)',
    summary         VARCHAR(512) DEFAULT '' COMMENT '结果摘要',
    error_detail    TEXT COMMENT '错误详情',
    step_order      INT DEFAULT 0 COMMENT '执行序号',
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,

    INDEX idx_event (event_id),
    INDEX idx_phase (phase),
    INDEX idx_tool_status (tool_name, status),
    INDEX idx_created (created_at),
    FOREIGN KEY (event_id) REFERENCES diagnosis_history(event_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
