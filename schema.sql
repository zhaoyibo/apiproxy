CREATE TABLE IF NOT EXISTS api_keys (
  id          BIGINT        NOT NULL AUTO_INCREMENT                    COMMENT '主键',
  name        VARCHAR(255)  NOT NULL DEFAULT ''                        COMMENT '显示名称',
  key_code    VARCHAR(512)  NOT NULL DEFAULT ''                        COMMENT 'API Key 值',
  parent_id   BIGINT        NOT NULL DEFAULT -1                        COMMENT '父 Key ID，-1 表示根 Key',
  quota_cny   DECIMAL(8,2)   NOT NULL DEFAULT -1                       COMMENT '额度上限（元），-1 表示无限制',
  is_active   TINYINT(1)    NOT NULL DEFAULT 1                         COMMENT '是否启用，1 启用 0 禁用',
  created_at  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP         COMMENT '创建时间',
  PRIMARY KEY (id),
  UNIQUE INDEX idx_key_code (key_code),
  INDEX idx_parent (parent_id)
);

CREATE TABLE IF NOT EXISTS daily_stats (
  id                  BIGINT         NOT NULL AUTO_INCREMENT           COMMENT '主键',
  key_id              BIGINT         NOT NULL DEFAULT 0                COMMENT '关联的子 Key ID（api_keys.id）',
  date                DATE           NOT NULL DEFAULT '1970-01-01'     COMMENT '统计日期',
  model               VARCHAR(100)   NOT NULL DEFAULT ''               COMMENT '模型名称',
  input_tokens        BIGINT         NOT NULL DEFAULT 0                COMMENT '输入 Token 数',
  output_tokens       BIGINT         NOT NULL DEFAULT 0                COMMENT '输出 Token 数',
  cache_write_tokens  BIGINT         NOT NULL DEFAULT 0                COMMENT '缓存写入 Token 数',
  cache_hit_tokens    BIGINT         NOT NULL DEFAULT 0                COMMENT '缓存命中 Token 数',
  cost_cny            DECIMAL(24,12) NOT NULL DEFAULT 0                COMMENT '费用（元）',
  PRIMARY KEY (id),
  UNIQUE INDEX idx_key_date_model (key_id, date, model)
);

CREATE TABLE IF NOT EXISTS model_prices (
  id              BIGINT         NOT NULL AUTO_INCREMENT               COMMENT '主键',
  model           VARCHAR(100)   NOT NULL DEFAULT ''                   COMMENT '模型名称',
  context_min     BIGINT         NOT NULL DEFAULT 0                    COMMENT '上下文长度下限（含）',
  context_max     BIGINT         NOT NULL DEFAULT -1                   COMMENT '上下文长度上限（不含），-1 表示无上限',
  input_cny       DECIMAL(10,4)  NOT NULL DEFAULT 0                    COMMENT '输入单价（元/百万 Token）',
  output_cny      DECIMAL(10,4)  NOT NULL DEFAULT 0                    COMMENT '输出单价（元/百万 Token）',
  cache_hit_cny   DECIMAL(10,4)  NOT NULL DEFAULT 0                    COMMENT '缓存命中单价（元/百万 Token），0 表示不适用',
  cache_write_cny DECIMAL(10,4)  NOT NULL DEFAULT 0                    COMMENT '缓存写入单价（元/百万 Token），0 表示不适用',
  updated_at      DATETIME       NOT NULL DEFAULT CURRENT_TIMESTAMP
                                          ON UPDATE CURRENT_TIMESTAMP  COMMENT '最后更新时间',
  PRIMARY KEY (id),
  INDEX idx_model (model)
);
