package main

import (
	"context"
	"fmt"
	"os"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

var schema = sqlitemigration.Schema{
	Migrations: []string{`
CREATE TABLE models (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  key TEXT NOT NULL UNIQUE
);
CREATE TABLE projects (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  name TEXT NOT NULL,
  UNIQUE (user_id, name)
);
CREATE TABLE usage (
  ts TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  model_id INTEGER NOT NULL REFERENCES models(id),
  project_id INTEGER NOT NULL REFERENCES projects(id),
  tokens INTEGER NOT NULL
);
`,
	},
}

func newPool() (*sqlitemigration.Pool, error) {
	pool := sqlitemigration.NewPool("gpt-proxy-split.db", schema, sqlitemigration.Options{
		Flags: sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenWAL,
		PrepareConn: func(conn *sqlite.Conn) error {
			return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON;", nil)
		},
	})
	return pool, nil
}

func mustNewPool() *sqlitemigration.Pool {
	pool, err := newPool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}
	return pool
}

func mustGetDB(ctx context.Context, pool *sqlitemigration.Pool) *sqlite.Conn {
	conn, err := pool.Get(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting database connection: %v\n", err)
		os.Exit(1)
	}
	return conn
}

const insertProjectIDStmt = `INSERT OR IGNORE INTO projects (user_id, name) VALUES (:userID, :name)`
const selectProjectIDStmt = `SELECT id FROM projects WHERE user_id = :userID AND name = :name`

func getProjectID(conn *sqlite.Conn, userID int64, projectName string) (int64, error) {
	if err := sqlitex.ExecuteTransient(conn, insertProjectIDStmt, &sqlitex.ExecOptions{
		Named: map[string]any{
			":userID": userID,
			":name":   projectName,
		},
	}); err != nil {
		return 0, fmt.Errorf("failed to insert project ID: %w", err)
	}

	var projectID int64
	if err := sqlitex.ExecuteTransient(conn, selectProjectIDStmt, &sqlitex.ExecOptions{
		Named: map[string]any{
			":userID": userID,
			":name":   projectName,
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			projectID = stmt.GetInt64("id")
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("failed to select project ID: %w", err)
	}
	return projectID, nil
}

const insertModelIDStmt = `INSERT OR IGNORE INTO models (name) VALUES (:name)`
const selectModelIDStmt = `SELECT id FROM models WHERE name = :name`

func getModelID(conn *sqlite.Conn, modelName string) (int64, error) {
	if err := sqlitex.ExecuteTransient(conn, insertModelIDStmt, &sqlitex.ExecOptions{
		Named: map[string]any{":name": modelName},
	}); err != nil {
		return 0, fmt.Errorf("failed to insert model ID: %w", err)
	}

	var modelID int64
	if err := sqlitex.ExecuteTransient(conn, selectModelIDStmt, &sqlitex.ExecOptions{
		Named: map[string]any{":name": modelName},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			modelID = stmt.GetInt64("id")
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("failed to get model ID: %w", err)
	}

	return modelID, nil
}

const saveUsageStmt = `INSERT INTO usage (model_id, project_id, tokens) VALUES (:modelID, :projectID, :tokensUsage)`

func saveUsage(conn *sqlite.Conn, modelID int64, projectID int64, tokensUsage int) (err error) {
	defer sqlitex.Save(conn)(&err)

	if err := sqlitex.ExecuteTransient(conn, saveUsageStmt, &sqlitex.ExecOptions{
		Named: map[string]any{
			":modelID":     modelID,
			":projectID":   projectID,
			":tokensUsage": tokensUsage,
		},
	}); err != nil {
		return fmt.Errorf("failed to save usage: %w", err)
	}

	return nil
}

const listUsersStmt = `SELECT name, key FROM users ORDER BY name`

type user struct {
	name string
	key  string
}

func listUsers(conn *sqlite.Conn) ([]user, error) {
	var users []user

	if err := sqlitex.ExecuteTransient(conn, listUsersStmt, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var u user
			u.name = stmt.GetText("name")
			u.key = stmt.GetText("key")
			users = append(users, u)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}

	return users, nil
}

const setUserKeyQuery = `
INSERT INTO users (name, key)
VALUES (:userName, :apiKey)
ON CONFLICT (name) DO UPDATE SET key = :apiKey`

func setUserKey(conn *sqlite.Conn, userName string, apiKey string) (err error) {
	defer sqlitex.Save(conn)(&err)

	if err := sqlitex.ExecuteTransient(conn, setUserKeyQuery, &sqlitex.ExecOptions{
		Named: map[string]any{
			":userName": userName,
			":apiKey":   apiKey,
		},
	}); err != nil {
		return fmt.Errorf("failed to save user/key: %w", err)
	}

	return nil
}

const deleteUserQuery = `DELETE FROM users WHERE name = :userName`

func deleteUser(conn *sqlite.Conn, userName string) (_ bool, err error) {
	defer sqlitex.Save(conn)(&err)

	if err := sqlitex.ExecuteTransient(conn, deleteUserQuery, &sqlitex.ExecOptions{
		Named: map[string]any{":userName": userName},
	}); err != nil {
		return false, fmt.Errorf("failed to delete user: %w", err)
	}

	return conn.Changes() != 0, nil
}

const findUserByKeyStmt = `SELECT id, name FROM users WHERE KEY = :apiKey`

func findUserByKey(conn *sqlite.Conn, apiKey string) (int64, string, bool, error) {
	var userID int64
	var userName string
	var userFound bool
	if err := sqlitex.ExecuteTransient(conn, findUserByKeyStmt, &sqlitex.ExecOptions{
		Named: map[string]any{":apiKey": apiKey},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			userID = stmt.GetInt64("id")
			userName = stmt.GetText("name")
			userFound = true
			return nil
		},
	}); err != nil {
		return 0, "", false, fmt.Errorf("failed to find user by key: %w", err)
	}

	return userID, userName, userFound, nil
}

const getUsageStmt = `
SELECT strftime('%Y-%m', usage.ts) AS month,
  users.name AS userName,
  projects.name as projectName,
  SUM(usage.tokens) AS usage
FROM usage
JOIN projects ON projects.id = usage.project_id
JOIN users ON users.id = projects.user_id
GROUP BY month, user_id, project_id
ORDER BY month, usage DESC, user_id, project_id
`

type usage struct {
	month    string
	projects []projectUsage
}

type projectUsage struct {
	userName    string
	projectName string
	tokens      int
}

func getUsage(conn *sqlite.Conn) ([]usage, error) {
	var usages []usage

	if err := sqlitex.ExecuteTransient(conn, getUsageStmt, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			month := stmt.GetText("month")
			if len(usages) == 0 || usages[len(usages)-1].month != month {
				usages = append(usages, usage{month: month})
			}
			u := &usages[len(usages)-1]
			u.projects = append(u.projects, projectUsage{
				userName:    stmt.GetText("userName"),
				projectName: stmt.GetText("projectName"),
				tokens:      int(stmt.GetInt64("usage")),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to list usage: %w", err)
	}

	return usages, nil
}
