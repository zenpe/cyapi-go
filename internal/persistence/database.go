package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"cyapi-go/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite"
)

type Database struct {
	ORM    *gorm.DB
	SQL    *sql.DB
	Driver string
}

func Open(ctx context.Context, c config.Database) (*Database, error) {
	var dialector gorm.Dialector
	switch c.Driver {
	case "sqlite":
		if err := os.MkdirAll(filepath.Dir(c.DSN), 0700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		// The driver applies each _pragma on every physical connection, including replacements.

		q := url.Values{}
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "busy_timeout("+strconv.FormatInt(c.BusyTimeout.Milliseconds(), 10)+")")
		q.Add("_pragma", "synchronous(FULL)")

		dialector = sqlite.New(sqlite.Config{DriverName: "sqlite", DSN: filepath.ToSlash(c.DSN) + "?" + q.Encode()})
	case "postgres":
		dialector = postgres.New(postgres.Config{DSN: c.DSN, PreferSimpleProtocol: true})
	default:
		return nil, errors.New("unsupported database driver")
	}
	orm, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true, TranslateError: true})
	if err != nil {
		if c.Driver == "sqlite" {
			return nil, fmt.Errorf("sqlite initialization: %w", err)
		}
		return nil, errors.New("database initialization failed")
	}
	raw, err := orm.DB()
	if err != nil {
		return nil, errors.New("database handle unavailable")
	}
	if c.Driver == "sqlite" {
		raw.SetMaxOpenConns(1)
		raw.SetMaxIdleConns(1)
	} else {
		raw.SetMaxOpenConns(4)
		raw.SetMaxIdleConns(2)
		raw.SetConnMaxLifetime(30 * time.Minute)
	}
	db := &Database{ORM: orm, SQL: raw, Driver: c.Driver}
	if err = raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, errors.New("database connection failed")
	}
	return db, nil
}
func (d *Database) Close() error { return d.SQL.Close() }
