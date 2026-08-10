package main

import (
	"encoding/binary"
	"os"
	"strings"
)

// dbInfo holds parsed metadata from a SQLite database file.
type dbInfo struct {
	pageSize   int
	tableCount int
}

// schemaRow holds the first two columns of a sqlite_schema record.
// sqlite_schema column order: type, name, tbl_name, rootpage, sql
type schemaRow struct {
	typ  string // "table", "index", "view", "trigger"
	name string // e.g. "apples", "oranges"
}

// readDbInfo reads the page size and number of tables from a SQLite database file.
func readDbInfo(path string) (dbInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return dbInfo{}, err
	}
	defer f.Close()

	pageSize, err := readPageSize(f)
	if err != nil {
		return dbInfo{}, err
	}

	page1, err := readPage1(f, pageSize)
	if err != nil {
		return dbInfo{}, err
	}

	tableCount := countTables(page1)

	return dbInfo{pageSize: pageSize, tableCount: tableCount}, nil
}

// readTableNames returns the names of all tables in the database.
func readTableNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pageSize, err := readPageSize(f)
	if err != nil {
		return nil, err
	}

	page1, err := readPage1(f, pageSize)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, row := range parseSchemaRows(page1) {
		if row.typ == "table" && !strings.HasPrefix(row.name, "sqlite_") {
			names = append(names, row.name)
		}
	}
	return names, nil
}

// readPageSize reads the database page size from the 100-byte file header.
// Bytes 16-17 hold the page size as a big-endian uint16.
func readPageSize(f *os.File) (int, error) {
	header := make([]byte, 100)
	if _, err := f.Read(header); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(header[16:18])), nil
}

// readPage1 reads the full first page from the beginning of the file.
// Page 1 contains the sqlite_schema table (all tables/indexes/views/triggers).
// https://www.sqlite.org/schematab.html
func readPage1(f *os.File, pageSize int) ([]byte, error) {
	page := make([]byte, pageSize)
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	if _, err := f.Read(page); err != nil {
		return nil, err
	}
	return page, nil
}

// countTables parses page 1's B-tree and counts rows where type = "table".
func countTables(page1 []byte) int {
	count := 0
	for _, row := range parseSchemaRows(page1) {
		if row.typ == "table" {
			count++
		}
	}
	return count
}

// parseSchemaRows reads every cell in page 1's B-tree and returns a schemaRow
// (type + name) for each one.
//
// Page 1's B-tree header starts at byte 100 (after the 100-byte file header).
// Header layout (leaf table page, from sqlite.org/fileformat.html):
//
//	[0]    page type  (0x0d = leaf table)
//	[1-2]  first freeblock offset
//	[3-4]  number of cells
//	[5-6]  cell content area start
//	[7]    fragmented free bytes
func parseSchemaRows(page1 []byte) []schemaRow {
	const btreeHeaderStart = 100
	cellCount := int(binary.BigEndian.Uint16(page1[btreeHeaderStart+3 : btreeHeaderStart+5]))

	// Cell pointer array immediately follows the 8-byte B-tree header.
	// Each entry is a 2-byte offset pointing to a cell's content within the page.
	cellPtrsStart := btreeHeaderStart + 8

	rows := make([]schemaRow, 0, cellCount)
	for i := range cellCount {
		ptrOffset := cellPtrsStart + i*2
		cellOffset := int(binary.BigEndian.Uint16(page1[ptrOffset : ptrOffset+2]))
		rows = append(rows, parseSchemaRow(page1, cellOffset))
	}
	return rows
}

// parseSchemaRow parses a single table-leaf cell and returns the type and name
// columns from the sqlite_schema record inside it.
//
// Table-leaf cell layout:
//
//	payload_size  (varint)  — total bytes of payload
//	row_id        (varint)  — integer primary key
//	record        (payload) — the actual column data
//
// Record layout:
//
//	header_length   (varint, counts itself)
//	serial_type[0]  (varint) — describes col 0 (type)
//	serial_type[1]  (varint) — describes col 1 (name)
//	...more serial types...
//	value[0]        — col 0 bytes (length from serial_type[0])
//	value[1]        — col 1 bytes (length from serial_type[1])
//	...more values...
//
// TEXT serial type encoding (from sqlite.org/fileformat.html):
//
//	odd number >= 13 → TEXT, byte length = (serialType - 13) / 2
func parseSchemaRow(page1 []byte, cellOffset int) schemaRow {
	pos := cellOffset
	_, n := readVarint(page1, pos) // skip payload size
	pos += n
	_, n = readVarint(page1, pos) // skip row ID
	pos += n

	// Record starts here. headerLen tells us where the values section begins.
	recordStart := pos
	headerLen, n := readVarint(page1, pos)
	pos += n

	// Read serial types for col 0 (type) and col 1 (name).
	serialType0, n := readVarint(page1, pos)
	pos += n
	serialType1, _ := readVarint(page1, pos)

	// Values section starts at recordStart + headerLen.
	valPos := recordStart + int(headerLen)

	// Decode col 0 (type) — TEXT: odd >= 13, length = (N-13)/2
	col0 := readTextValue(page1, valPos, serialType0)
	valPos += textByteLen(serialType0)

	// Decode col 1 (name) — same encoding
	col1 := readTextValue(page1, valPos, serialType1)

	return schemaRow{typ: col0, name: col1}
}

// readTextValue extracts a TEXT value from page data given its serial type.
// Returns empty string if the serial type is not TEXT.
func readTextValue(page1 []byte, offset int, serialType int64) string {
	if serialType < 13 || serialType%2 == 0 {
		return ""
	}
	length := textByteLen(serialType)
	return string(page1[offset : offset+length])
}

// textByteLen returns the byte length of a TEXT value from its serial type.
// Formula from sqlite.org/fileformat.html: length = (serialType - 13) / 2
func textByteLen(serialType int64) int {
	if serialType < 13 || serialType%2 == 0 {
		return 0
	}
	return int((serialType - 13) / 2)
}

