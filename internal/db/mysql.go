package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func NewMySQL(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

var tables = []string{
	`CREATE TABLE IF NOT EXISTS api_keys (
  id          BIGINT        NOT NULL AUTO_INCREMENT                    COMMENT '主键',
  name        VARCHAR(255)  NOT NULL DEFAULT ''                        COMMENT '显示名称',
  key_code    VARCHAR(512)  NOT NULL DEFAULT ''                        COMMENT 'API Key 值',
  parent_id   BIGINT        NOT NULL DEFAULT -1                        COMMENT '父 Key ID，-1 表示根 Key',
  quota_cny   BIGINT        NOT NULL DEFAULT -1                        COMMENT '额度上限（厘，0.001 CNY），-1 表示无限制',
  is_active   TINYINT(1)    NOT NULL DEFAULT 1                         COMMENT '是否启用，1 启用 0 禁用',
  created_at  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP         COMMENT '创建时间',
  PRIMARY KEY (id),
  UNIQUE INDEX idx_key_code (key_code),
  INDEX idx_parent (parent_id)
)`,
	`CREATE TABLE IF NOT EXISTS daily_stats (
  id                  BIGINT         NOT NULL AUTO_INCREMENT           COMMENT '主键',
  key_id              BIGINT         NOT NULL DEFAULT 0                COMMENT '关联的子 Key ID（api_keys.id）',
  date                DATE           NOT NULL DEFAULT '1970-01-01'     COMMENT '统计日期',
  model               VARCHAR(100)   NOT NULL DEFAULT ''               COMMENT '模型名称',
  input_tokens        BIGINT         NOT NULL DEFAULT 0                COMMENT '输入 Token 数',
  output_tokens       BIGINT         NOT NULL DEFAULT 0                COMMENT '输出 Token 数',
  cache_write_tokens  BIGINT         NOT NULL DEFAULT 0                COMMENT '缓存写入 Token 数',
  cache_hit_tokens    BIGINT         NOT NULL DEFAULT 0                COMMENT '缓存命中 Token 数',
  cost_cny            BIGINT         NOT NULL DEFAULT 0                COMMENT '费用（厘，0.001 CNY）',
  PRIMARY KEY (id),
  UNIQUE INDEX idx_key_date_model (key_id, date, model)
)`,
	`CREATE TABLE IF NOT EXISTS model_prices (
  id              BIGINT         NOT NULL AUTO_INCREMENT               COMMENT '主键',
  model           VARCHAR(100)   NOT NULL DEFAULT ''                   COMMENT '模型名称',
  context_min     BIGINT         NOT NULL DEFAULT 0                    COMMENT '上下文长度下限（含）',
  context_max     BIGINT         NOT NULL DEFAULT -1                   COMMENT '上下文长度上限（不含），-1 表示无上限',
  input_cny       BIGINT         NOT NULL DEFAULT 0                    COMMENT '输入单价（厘/百万 Token）',
  output_cny      BIGINT         NOT NULL DEFAULT 0                    COMMENT '输出单价（厘/百万 Token）',
  cache_hit_cny   BIGINT         NOT NULL DEFAULT 0                    COMMENT '缓存命中单价（厘/百万 Token），0 表示不适用',
  cache_write_cny BIGINT         NOT NULL DEFAULT 0                    COMMENT '缓存写入单价（厘/百万 Token），0 表示不适用',
  updated_at      DATETIME       NOT NULL DEFAULT CURRENT_TIMESTAMP
                                          ON UPDATE CURRENT_TIMESTAMP  COMMENT '最后更新时间',
  PRIMARY KEY (id),
  INDEX idx_model (model)
)`,
}

// addColumnIfMissing adds a column to a table only if it doesn't already exist.
func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, column,
	).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func Migrate(db *sql.DB) error {
	for _, stmt := range tables {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := addColumnIfMissing(db, "daily_stats", "call_count", "BIGINT NOT NULL DEFAULT 0 COMMENT '成功调用次数'"); err != nil {
		return fmt.Errorf("migrate alter call_count: %w", err)
	}
	if err := addColumnIfMissing(db, "daily_stats", "fail_count", "BIGINT NOT NULL DEFAULT 0 COMMENT '失败调用次数'"); err != nil {
		return fmt.Errorf("migrate alter fail_count: %w", err)
	}
	return nil
}
