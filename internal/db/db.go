// Package db 负责选择数据库驱动并创建 GORM 连接，是持久化层的统一入口。
package db

import (
	"fmt"
	"log"
	"os"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 当前由启动流程保证全局只创建一条 *gorm.DB；*gorm.DB 本身是并发安全的连接池句柄，无需为每次请求重复创建。

const (
	dbFilename           = "mcp-gateway.db"
	deprecatedDBFilename = "mcp.db"
)

// getSQLiteDBPath 选择默认 SQLite 文件。优先新名称，同时兼容旧版本留下的 mcp.db。
func getSQLiteDBPath() string {
	// 已有新库时直接沿用，避免意外创建第二份数据。
	if _, err := os.Stat(dbFilename); err == nil {
		return dbFilename
	}

	// 只存在旧库时继续使用，并给出迁移提示，防止升级后看起来像“数据丢失”。
	if _, err := os.Stat(deprecatedDBFilename); err == nil {
		log.Printf("[db] WARNING: Using deprecated database file '%s'. Please consider renaming it to '%s' for future compatibility.", deprecatedDBFilename, dbFilename)
		return deprecatedDBFilename
	}

	// 两者都不存在时，GORM 会在首次连接时创建新文件。
	return dbFilename
}

// resolveSQLiteDBPath 让显式配置拥有最高优先级；未配置时才执行兼容旧文件的默认选择逻辑。
func resolveSQLiteDBPath(configuredPath string) string {
	if configuredPath != "" {
		return configuredPath
	}
	return getSQLiteDBPath()
}

// NewDBConnection 根据 DSN 选择数据库：空 DSN 使用本地 SQLite，非空 DSN 视为 PostgreSQL。
// SQLite 开启 busy_timeout 和 WAL，以降低并发读写时直接报 database is locked 的概率。
func NewDBConnection(dsn string, sqliteDBPath string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	if dsn == "" {
		dbPath := resolveSQLiteDBPath(sqliteDBPath)
		log.Printf("[db] Using sqlite database at %s", dbPath)
		dialector = sqlite.Open(fmt.Sprintf("%s?_busy_timeout=5000&_journal_mode=WAL", dbPath))
	} else {
		log.Printf("[db] Using postgres database")
		dialector = postgres.Open(dsn)
	}

	c := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	}
	db, err := gorm.Open(dialector, c)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return db, nil
}
