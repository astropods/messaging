// Package testpg gives each test its own empty Postgres database.
package testpg

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Env names the server to create test databases on. Tests that need Postgres skip without it.
const Env = "CHAT_TEST_DATABASE_URL"

// Enabled reports whether Env is set.
func Enabled() bool { return os.Getenv(Env) != "" }

// URL creates an empty database, drops it when the test ends, and returns its URL.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		t.Skip(Env + " is unset")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	name := "chat_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		_ = admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		_ = admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", Env, err)
	}
	u.Path = "/" + name
	return u.String()
}
