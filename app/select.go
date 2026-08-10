package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"github.com/xwb1989/sqlparser"
)

// ── Step 2: serialByteLen + readCellColumns ──────────────────────────────────

// serialByteLen returns the byte width of any SQLite serial type.
//
// Serial type table (sqlite.org/fileformat.html section 2.1):
//
//	0        → NULL,        0 bytes
//	1–6      → integer,     1/2/3/4/6/8 bytes
//	7        → IEEE float,  8 bytes
//	8, 9     → constants 0/1, 0 bytes
//	≥12 even → BLOB,       (N-12)/2 bytes
//	≥13 odd  → TEXT,       (N-13)/2 bytes
func serialByteLen(serialType int64) int {
	switch {
	case serialType == 0:
		return 0
	case serialType >= 1 && serialType <= 6:
		return intByteLen(serialType)
	case serialType == 7:
		return 8
	case serialType == 8 || serialType == 9:
		return 0
	case serialType >= 12 && serialType%2 == 0:
		return int((serialType - 12) / 2) // BLOB
	case serialType >= 13 && serialType%2 == 1:
		return int((serialType - 13) / 2) // TEXT
	}
	return 0
}

// readCellColumns extracts the string values at the given column indices
// from a single table-leaf cell.
//
// How it works:
//  1. Skip payload-size and rowid varints (they precede the record).
//  2. Read the record header: headerLen + one serial type per column.
//  3. Jump to the values section (recordStart + headerLen).
//  4. Walk column by column using serialByteLen to skip unwanted columns,
//     then call readTextValue at the target column.
func readCellColumns(page []byte, cellOffset int, colIndices []int) []string {
	pos := cellOffset
	_, n := readVarint(page, pos)
	pos += n // skip payload size
	_, n = readVarint(page, pos)
	pos += n // skip rowid

	recordStart := pos
	headerLen, n := readVarint(page, pos)
	pos += n

	// Find the highest column index we need so we know how many serial types to read.
	maxIdx := 0
	for _, idx := range colIndices {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	serialTypes := make([]int64, maxIdx+1)
	for i := range maxIdx + 1 {
		serialTypes[i], n = readVarint(page, pos)
		pos += n
	}

	// For each requested column, skip earlier columns then read the value.
	// Each call starts fresh from the beginning of the values section.
	result := make([]string, len(colIndices))
	valuesStart := recordStart + int(headerLen)
	for r, colIdx := range colIndices {
		valPos := valuesStart
		for i := range colIdx {
			valPos += serialByteLen(serialTypes[i])
		}
		result[r] = readTextValue(page, valPos, serialTypes[colIdx])
	}
	return result
}

// ── Step 3: parse CREATE TABLE to map column name → index ───────────────────

// columnNamesFromSQL extracts column names from a CREATE TABLE statement in order.
//
// Example: "CREATE TABLE apples (id integer, name text, color text)"
// Returns: ["id", "name", "color"]
//
// Table-level constraints (PRIMARY KEY, UNIQUE, CHECK, FOREIGN KEY) start with
// a keyword and are filtered out.
func columnNamesFromSQL(createSQL string) []string {
	start := strings.Index(createSQL, "(")
	end := strings.LastIndex(createSQL, ")")
	if start == -1 || end == -1 {
		return nil
	}

	constraintKeywords := map[string]bool{
		"primary": true, "unique": true, "check": true,
		"foreign": true, "constraint": true,
	}

	var names []string
	for _, def := range strings.Split(createSQL[start+1:end], ",") {
		parts := strings.Fields(strings.TrimSpace(def))
		if len(parts) > 0 && !constraintKeywords[strings.ToLower(parts[0])] {
			names = append(names, parts[0])
		}
	}
	return names
}

// columnIndex returns the 0-based position of colName inside a CREATE TABLE
// statement. Returns -1 if not found. Column names are compared case-insensitively.
func columnIndex(createSQL, colName string) int {
	for i, name := range columnNamesFromSQL(createSQL) {
		if strings.EqualFold(name, colName) {
			return i
		}
	}
	return -1
}

// ── Step 4: parse SELECT with sqlparser ─────────────────────────────────────

// selectQuery holds the parsed result of a SELECT statement.
type selectQuery struct {
	table   string
	columns []string // column names requested; empty means SELECT *
	isCount bool     // true for SELECT COUNT(*)
}

// parseSelect uses sqlparser to extract the table name and column list from a
// SELECT statement.
//
// Supported forms:
//   - SELECT COUNT(*) FROM t   → {table:"t", isCount:true}
//   - SELECT a, b FROM t       → {table:"t", columns:["a","b"]}
func parseSelect(sql string) (selectQuery, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return selectQuery{}, fmt.Errorf("parse error: %w", err)
	}

	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return selectQuery{}, fmt.Errorf("not a SELECT statement")
	}

	// Extract table name from the FROM clause.
	aliased, ok := sel.From[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return selectQuery{}, fmt.Errorf("unsupported FROM clause")
	}
	table := aliased.Expr.(sqlparser.TableName).Name.String()

	var q selectQuery
	q.table = table

	for _, expr := range sel.SelectExprs {
		switch e := expr.(type) {
		case *sqlparser.StarExpr:
			// SELECT * — leave columns empty
		case *sqlparser.AliasedExpr:
			switch inner := e.Expr.(type) {
			case *sqlparser.ColName:
				q.columns = append(q.columns, inner.Name.String())
			case *sqlparser.FuncExpr:
				if strings.ToLower(inner.Name.String()) == "count" {
					q.isCount = true
				}
			}
		}
	}

	return q, nil
}

// ── Step 5: executeSelect ────────────────────────────────────────────────────

// executeSelect runs a SELECT query and prints one row per line with columns
// separated by "|".
//
// Flow:
//  1. Read sqlite_schema to find rootpage + CREATE TABLE sql for the table.
//  2. Parse CREATE TABLE to map each requested column name to its index.
//  3. Navigate to rootpage, read cell pointer array (leaf page, 8-byte header).
//  4. For each cell, call readCellColumns and print the values.
func executeSelect(path string, q selectQuery) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	pageSize, err := readPageSize(f)
	if err != nil {
		return err
	}
	page1, err := readPage1(f, pageSize)
	if err != nil {
		return err
	}

	// Find the table in sqlite_schema.
	var rootpage int
	var createSQL string
	for _, row := range parseSchemaRows(page1) {
		if row.typ == "table" && row.name == q.table {
			rootpage = row.rootpage
			createSQL = row.sql
			break
		}
	}
	if rootpage == 0 {
		return fmt.Errorf("table %q not found", q.table)
	}

	// Map column names to 0-based indices within the record.
	colIndices := make([]int, len(q.columns))
	for i, col := range q.columns {
		idx := columnIndex(createSQL, col)
		if idx == -1 {
			return fmt.Errorf("column %q not found in table %q", col, q.table)
		}
		colIndices[i] = idx
	}

	// Seek to the rootpage. Pages are 1-indexed: page N starts at (N-1)*pageSize.
	if _, err := f.Seek(int64(rootpage-1)*int64(pageSize), 0); err != nil {
		return err
	}
	page := make([]byte, pageSize)
	if _, err := f.Read(page); err != nil {
		return err
	}

	// Leaf table page header (8 bytes, starts at offset 0 for non-page-1 pages):
	//   [0]   page type (0x0D)
	//   [1-2] first freeblock
	//   [3-4] cell count       ← number of rows
	//   [5-6] cell content start
	//   [7]   fragmented free bytes
	// Cell pointer array starts immediately after at offset 8.
	cellCount := int(binary.BigEndian.Uint16(page[3:5]))
	for i := range cellCount {
		ptrOffset := 8 + i*2
		cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
		values := readCellColumns(page, cellOffset, colIndices)
		fmt.Println(strings.Join(values, "|"))
	}
	return nil
}
