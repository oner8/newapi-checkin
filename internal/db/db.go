// Package db 负责打开 SQLite 并建表。
package db

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"newapi-checkin/internal/model"
)

// Open 打开（必要时创建）SQLite 数据库并执行建表。
//
// 使用 glebarez/sqlite（纯 Go 驱动），因此可以 CGO_ENABLED=0 静态编译。
// 连接池限制为 1：写入来自后台任务、读取来自 HTTP 接口，串行化可以彻底避免
// SQLITE_BUSY；WAL 与 busy_timeout 进一步提高并发下的稳定性。
func Open(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s 失败: %w（若在本地直接运行，请把 database.path 改成可写路径，例如 ./data/newapi-checkin.db）", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", path, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := gdb.AutoMigrate(&model.CheckinRecord{}, &model.RunLog{}, &model.NotifyLog{}); err != nil {
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	return gdb, nil
}

// Close 关闭数据库连接。
func Close(gdb *gorm.DB) error {
	if gdb == nil {
		return nil
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
