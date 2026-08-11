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
//  1. Skip payload-size varint and capture the rowid varint (INTEGER PRIMARY KEY).
//  2. Read the record header: headerLen + one serial type per column.
//  3. Jump to the values section (recordStart + headerLen).
//  4. Walk column by column using serialByteLen to skip unwanted columns,
//     then call readTextValue at the target column.
//  5. Serial type 0 means the column IS the rowid (INTEGER PRIMARY KEY alias).
func readCellColumns(page []byte, cellOffset int, colIndices []int) []string {
	pos := cellOffset
	_, n := readVarint(page, pos)
	pos += n // skip payload size
	rowid, n := readVarint(page, pos) // capture rowid for INTEGER PRIMARY KEY
	pos += n

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
		if serialTypes[colIdx] == 0 {
			// Serial type 0 = INTEGER PRIMARY KEY aliased to rowid — no stored bytes.
			result[r] = fmt.Sprintf("%d", rowid)
		} else {
			result[r] = readTextValue(page, valPos, serialTypes[colIdx])
		}
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

type whereClause struct {
	col string // e.g. "color"
	val string // e.g. "Yellow"
}

// selectQuery holds the parsed result of a SELECT statement.
type selectQuery struct {
	table   string
	columns []string // column names requested; empty means SELECT *
	isCount bool     // true for SELECT COUNT(*)
	where   *whereClause
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

	// Extract WHERE clause if present (not used in this implementation).
	if sel.Where != nil {
		cmp, ok := sel.Where.Expr.(*sqlparser.ComparisonExpr) // downcast like: Dog d = (Dog) animal;
		if ok && cmp.Operator == "=" {
			col := cmp.Left.(*sqlparser.ColName).Name.String()
			val := cmp.Right.(*sqlparser.SQLVal).Val
			q.where = &whereClause{col: col, val: string(val)}
		}
	}

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

// readPageN reads page number pageNum (1-indexed) from f into a new byte slice.
func readPageN(f *os.File, pageSize, pageNum int) ([]byte, error) {
	page := make([]byte, pageSize)
	if _, err := f.Seek(int64(pageNum-1)*int64(pageSize), 0); err != nil {
		return nil, err
	}
	if _, err := f.Read(page); err != nil {
		return nil, err
	}
	return page, nil
}

// gatherLeafPages does a BFS from rootPageNum, following interior page child
// pointers until it has collected every leaf page in the table's B-tree.
//
// Interior page (0x05) header layout (12 bytes):
//
//	[0]   page type
//	[1-2] first freeblock
//	[3-4] cell count
//	[5-6] cell content start
//	[7]   fragmented free bytes
//	[8-11] rightmost child page number  ← extra 4 bytes vs leaf header
//
// Interior cell layout: [uint32 left_child][varint key]
// Cell pointer array starts at offset 12 (not 8 like leaf pages).
func gatherLeafPages(f *os.File, pageSize, rootPageNum int) ([][]byte, error) {
	var leaves [][]byte
	queue := []int{rootPageNum}
	for len(queue) > 0 {
		pageNum := queue[0]
		queue = queue[1:]

		page, err := readPageN(f, pageSize, pageNum)
		if err != nil {
			return nil, err
		}

		switch page[0] {
		case 0x0D: // leaf table page — contains row data
			leaves = append(leaves, page)

		case 0x05: // interior table page — contains child pointers only
			cellCount := int(binary.BigEndian.Uint16(page[3:5]))
			for i := range cellCount {
				ptrOffset := 12 + i*2 // cell pointer array starts at 12, not 8
				cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
				leftChild := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
				queue = append(queue, leftChild)
			}
			// rightmost child is in the header, not in a cell
			rightmost := int(binary.BigEndian.Uint32(page[8:12]))
			queue = append(queue, rightmost)
		}
	}
	return leaves, nil
}

// ── Section 4: Index scan ─────────────────────────────────────────────────────

// indexedColumn extracts the first column name from a CREATE INDEX SQL statement.
// "CREATE INDEX idx_companies_country ON companies (country)" → "country"
func indexedColumn(sql string) string {
	start := strings.Index(sql, "(")
	end := strings.LastIndex(sql, ")")
	if start == -1 || end == -1 {
		return ""
	}
	col := strings.TrimSpace(sql[start+1 : end])
	parts := strings.Fields(col)
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(parts[0])
}

// readIndexRecord parses a SQLite index record starting at pos.
//
// Index record layout:
//
//	[headerLen varint][serialType0 varint][serialType1 varint]
//	[indexed_value bytes][rowid bytes]
//
// The rowid is always stored as the last column of the index record.
func readIndexRecord(page []byte, pos int) (string, int64) {
	recordStart := pos
	headerLen, n := readVarint(page, pos)
	pos += n
	serialType0, n := readVarint(page, pos)
	pos += n
	serialType1, _ := readVarint(page, pos)

	valPos := recordStart + int(headerLen)
	colVal := readTextValue(page, valPos, serialType0)
	valPos += serialByteLen(serialType0)
	rowid := readIntValue(page, valPos, serialType1)
	return colVal, rowid
}

// readIndexLeafCell parses an index leaf cell (page type 0x0A).
//
// Layout: [payload_size varint][record]
func readIndexLeafCell(page []byte, cellOffset int) (string, int64) {
	pos := cellOffset
	_, n := readVarint(page, pos)
	pos += n // skip payload size
	return readIndexRecord(page, pos)
}

// readIndexInteriorCell parses an index interior cell (page type 0x02).
//
// Layout: [uint32 left_child][payload_size varint][record]
func readIndexInteriorCell(page []byte, cellOffset int) (int, string, int64) {
	leftChild := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
	pos := cellOffset + 4
	_, n := readVarint(page, pos)
	pos += n // skip payload size
	colVal, rowid := readIndexRecord(page, pos)
	return leftChild, colVal, rowid
}

// searchIndex does a BFS over the index B-tree rooted at indexRoot,
// collecting all rowids where the indexed value equals searchVal.
//
// Index page types:
//
//	0x0A = index leaf page     (8-byte header,  cells: [payload_size][record])
//	0x02 = index interior page (12-byte header, cells: [uint32 left_child][payload_size][record])
//
// SQLite is a B-tree (not B+ tree): interior cells store actual records
// (the dividing key IS the record). Interior cells must be checked for
// matches, not just leaf cells.
func searchIndex(f *os.File, pageSize, indexRoot int, searchVal string) ([]int64, error) {
	var rowids []int64
	queue := []int{indexRoot}

	for len(queue) > 0 {
		pageNum := queue[0]
		queue = queue[1:]

		page, err := readPageN(f, pageSize, pageNum)
		if err != nil {
			return nil, err
		}

		cellCount := int(binary.BigEndian.Uint16(page[3:5]))

		switch page[0] {
		case 0x0A: // index leaf page
			for i := range cellCount {
				ptrOffset := 8 + i*2
				cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
				colVal, rowid := readIndexLeafCell(page, cellOffset)
				if colVal == searchVal {
					rowids = append(rowids, rowid)
				}
			}

		case 0x02: // index interior page — cells also hold real records
			for i := range cellCount {
				ptrOffset := 12 + i*2
				cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
				leftChild, colVal, rowid := readIndexInteriorCell(page, cellOffset)
				if colVal == searchVal {
					rowids = append(rowids, rowid)
				}
				queue = append(queue, leftChild)
			}
			// rightmost child is in the 12-byte header, not in a cell
			rightmost := int(binary.BigEndian.Uint32(page[8:12]))
			queue = append(queue, rightmost)
		}
	}

	return rowids, nil
}

// readCellRowid reads the rowid (2nd varint) from a table leaf cell.
//
// Table leaf cell layout: [payload_size varint][rowid varint][record]
func readCellRowid(page []byte, cellOffset int) int64 {
	pos := cellOffset
	_, n := readVarint(page, pos)
	pos += n // skip payload size
	rowid, _ := readVarint(page, pos)
	return rowid
}

// lookupByRowid descends the table B-tree to find and return the cell
// whose rowid equals the given value. Returns (page, cellOffset).
//
// Table interior page (0x05) cell: [uint32 left_child][varint key]
// Navigation rule: rowid <= key → follow left_child; else continue; fallthrough → rightmost.
func lookupByRowid(f *os.File, pageSize, tableRoot int, rowid int64) ([]byte, int, error) {
	pageNum := tableRoot
	for {
		page, err := readPageN(f, pageSize, pageNum)
		if err != nil {
			return nil, 0, err
		}

		cellCount := int(binary.BigEndian.Uint16(page[3:5]))

		switch page[0] {
		case 0x0D: // table leaf — linear scan for the rowid
			for i := range cellCount {
				ptrOffset := 8 + i*2
				cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
				if readCellRowid(page, cellOffset) == rowid {
					return page, cellOffset, nil
				}
			}
			return nil, 0, fmt.Errorf("rowid %d not found in table", rowid)

		case 0x05: // table interior — descend the correct branch
			nextPage := int(binary.BigEndian.Uint32(page[8:12])) // rightmost default
			for i := range cellCount {
				ptrOffset := 12 + i*2
				cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
				leftChild := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
				key, _ := readVarint(page, cellOffset+4)
				if rowid <= key {
					nextPage = leftChild
					break
				}
			}
			pageNum = nextPage
		}
	}
}

// executeSelect executes a parsed SELECT query against the database at path.
// separated by "|".
//
// Flow:
//  1. Read sqlite_schema to find rootpage + CREATE TABLE sql for the table.
//  2. Parse CREATE TABLE to map each requested column name to its index.
//  3. If a WHERE clause is present and a matching index exists, use the
//     index scan path: searchIndex → lookupByRowid for each matched rowid.
//  4. Otherwise fall back to the full table scan (gatherLeafPages BFS).
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

	// If a WHERE clause is present, look for a covering index in sqlite_schema.
	// A covering index must have: type='index', tblName==q.table, and its
	// indexed column must match q.where.col.
	if q.where != nil {
		indexRootpage := 0
		for _, row := range parseSchemaRows(page1) {
			if row.typ == "index" && row.tblName == q.table &&
				strings.EqualFold(indexedColumn(row.sql), q.where.col) {
				indexRootpage = row.rootpage
				break
			}
		}

		if indexRootpage != 0 {
			// Index scan: find matching rowids via the index, then point-lookup each row.
			rowids, err := searchIndex(f, pageSize, indexRootpage, q.where.val)
			if err != nil {
				return err
			}
			for _, rowid := range rowids {
				page, cellOffset, err := lookupByRowid(f, pageSize, rootpage, rowid)
				if err != nil {
					return err
				}
				values := readCellColumns(page, cellOffset, colIndices)
				fmt.Println(strings.Join(values, "|"))
			}
			return nil
		}
	}

	// resolve the WHERE col index
	whereIdx := -1
	if q.where != nil {
		whereIdx = columnIndex(createSQL, q.where.col)
	}

	// Collect all leaf pages via BFS — handles both single-leaf (small table)
	// and interior-rooted (large table) B-trees transparently.
	leaves, err := gatherLeafPages(f, pageSize, rootpage)
	if err != nil {
		return err
	}

	for _, page := range leaves {
		// Leaf page header (8 bytes): cell pointer array starts at offset 8.
		//   [0]   page type (0x0D)
		//   [3-4] cell count
		cellCount := int(binary.BigEndian.Uint16(page[3:5]))
		for i := range cellCount {
			ptrOffset := 8 + i*2
			cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))

			if whereIdx != -1 {
				whereVal := readCellColumns(page, cellOffset, []int{whereIdx})[0]
				if whereVal != q.where.val {
					continue
				}
			}

			values := readCellColumns(page, cellOffset, colIndices)
			fmt.Println(strings.Join(values, "|"))
		}
	}
	return nil
}
