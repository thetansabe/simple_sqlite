package tests

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

func TestSelectCountStar(t *testing.T) {
	dbPath := createSmallTableDB(t)
	assertQueryOutput(t, dbPath, "SELECT count(*) FROM fruits;", false)
}

func TestSelectSingleColumn(t *testing.T) {
	dbPath := createSmallTableDB(t)
	assertQueryOutput(t, dbPath, "SELECT name FROM fruits;", false)
}

func TestSelectWhereOnSmallSinglePageTable(t *testing.T) {
	dbPath := createSmallTableDB(t)
	assertPageType(t, dbPath, "fruits", 0x0D)
	assertQueryOutput(t, dbPath, "SELECT id, name FROM fruits WHERE color = 'Yellow';", false)
}

func TestSelectOnLargeTableAcrossPages(t *testing.T) {
	dbPath := createLargeTableDB(t)
	assertPageType(t, dbPath, "heroes", 0x05)
	assertQueryOutput(t, dbPath, "SELECT id, name FROM heroes;", false)
}

func TestSelectWhereOnIndexTableAcrossLeaves(t *testing.T) {
	dbPath := createIndexedTableDB(t)
	assertPageType(t, dbPath, "idx_companies_country", 0x02)
	assertQueryOutput(t, dbPath, "SELECT name FROM companies WHERE country = 'eritrea';", true)
}

func assertQueryOutput(t *testing.T, dbPath, query string, sortLines bool) {
	t.Helper()

	want := runSQLite(t, dbPath, query)
	got := runProgram(t, dbPath, query)

	if sortLines {
		want = normalizeSortedOutput(want)
		got = normalizeSortedOutput(got)
	} else {
		want = strings.TrimSpace(want)
		got = strings.TrimSpace(got)
	}

	if got != want {
		t.Fatalf("query %q output mismatch\nwant:\n%s\n\ngot:\n%s", query, want, got)
	}
}

func normalizeSortedOutput(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func createSmallTableDB(t *testing.T) string {
	t.Helper()
	return createDB(t, `
PRAGMA page_size = 512;
VACUUM;
CREATE TABLE fruits (id INTEGER PRIMARY KEY, name TEXT, color TEXT);
INSERT INTO fruits(name, color) VALUES
  ('Granny Smith', 'Green'),
  ('Fuji', 'Red'),
  ('Golden Delicious', 'Yellow');
`)
}

func createLargeTableDB(t *testing.T) string {
	t.Helper()
	return createDB(t, `
PRAGMA page_size = 512;
VACUUM;
CREATE TABLE heroes (id INTEGER PRIMARY KEY, name TEXT, bio TEXT);
WITH RECURSIVE seq(x) AS (
  SELECT 1
  UNION ALL
  SELECT x + 1 FROM seq WHERE x < 600
)
INSERT INTO heroes(name, bio)
SELECT
  printf('Hero %04d', x),
  printf('Biography %04d abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz', x)
FROM seq;
`)
}

func createIndexedTableDB(t *testing.T) string {
	t.Helper()
	return createDB(t, `
PRAGMA page_size = 512;
VACUUM;
CREATE TABLE companies (id INTEGER PRIMARY KEY, name TEXT, country TEXT);
WITH RECURSIVE seq(x) AS (
  SELECT 1
  UNION ALL
  SELECT x + 1 FROM seq WHERE x < 900
)
INSERT INTO companies(name, country)
SELECT
  printf('Company %04d', x),
  CASE
    WHEN x <= 400 THEN 'eritrea'
    WHEN x <= 700 THEN 'canada'
    ELSE 'japan'
  END
FROM seq;
CREATE INDEX idx_companies_country ON companies(country);
`)
}

func createDB(t *testing.T, setupSQL string) string {
	t.Helper()
	requireSQLite(t)

	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	cmd := exec.Command("sqlite3", dbPath)
	cmd.Stdin = strings.NewReader(setupSQL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create sqlite fixture: %v\n%s", err, output)
	}
	return dbPath
}

func assertPageType(t *testing.T, dbPath, objectName string, want byte) {
	t.Helper()

	rootPage, err := strconv.Atoi(strings.TrimSpace(runSQLite(t, dbPath, fmt.Sprintf(
		"SELECT rootpage FROM sqlite_schema WHERE name = '%s';",
		objectName,
	))))
	if err != nil {
		t.Fatalf("parse rootpage for %s: %v", objectName, err)
	}

	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database %s: %v", dbPath, err)
	}

	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	pageOffset := (rootPage - 1) * pageSize
	if rootPage == 1 {
		pageOffset += 100
	}

	got := data[pageOffset]
	if got != want {
		t.Fatalf("object %s root page type = 0x%02x, want 0x%02x", objectName, got, want)
	}
}

func runSQLite(t *testing.T, dbPath, query string) string {
	t.Helper()
	requireSQLite(t)

	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 query %q failed: %v\n%s", query, err, output)
	}
	return string(output)
}

func runProgram(t *testing.T, dbPath, query string) string {
	t.Helper()

	cmd := exec.Command(programPath(t), dbPath, query)
	cmd.Dir = repoRoot()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("program query %q failed: %v\n%s", query, err, output)
	}
	return string(output)
}

func programPath(t *testing.T) string {
	t.Helper()

	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "simple-sqlite-bin-*")
		if err != nil {
			buildErr = err
			return
		}

		buildPath = filepath.Join(dir, "simple-sqlite")
		cmd := exec.Command("go", "build", "-o", buildPath, "./app")
		cmd.Dir = repoRoot()
		output, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("%w\n%s", err, output)
		}
	})

	if buildErr != nil {
		t.Fatalf("build program: %v", buildErr)
	}
	return buildPath
}

func requireSQLite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is required for integration tests")
	}
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}
