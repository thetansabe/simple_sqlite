package main

import (
	"encoding/binary"
	"os"
)

// dbInfo holds parsed metadata from a SQLite database file.
type dbInfo struct {
	pageSize   int
	tableCount int
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

// readPageSize reads the database page size from the 100-byte file header.
// Bytes 16-17 hold the page size as a big-endian uint16.
// visualization: https://torymur.github.io/sqlite-repr (from byte 16 to 17 is the page size)
func readPageSize(f *os.File) (int, error) {
	header := make([]byte, 100)
	if _, err := f.Read(header); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(header[16:18])), nil
}

// readPage1 reads the full first page from the beginning of the file.
//
//	https://www.sqlite.org/schematab.html tells us Page 1 contains the sqlite_schema table (all tables/indexes/views/triggers).
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

// From sqlite.org/fileformat.html, B-tree Pages section:
// The b-tree corresponding to the sqlite_schema table is always a table b-tree and always has a root page of 1
// o page 1 = root of the sqlite_schema B-tree = where all table/index/view/trigger entries are stored.
// countTables parses page 1's B-tree and counts rows where type = "table".
func countTables(page1 []byte) int {
	// Page 1's B-tree header starts at byte 100 (after the 100-byte file header).\
	// fast find out via: https://torymur.github.io/sqlite-repr
	// Header layout (leaf table page):
	//   [0]    page type  (0x0d = leaf table)
	//   [1-2]  first freeblock offset
	//   [3-4]  number of cells
	//   [5-6]  cell content area start
	//   [7]    fragmented free bytes
	const btreeHeaderStart = 100
	cellCount := int(binary.BigEndian.Uint16(page1[btreeHeaderStart+3 : btreeHeaderStart+5]))

	// Cell pointer array immediately follows the 8-byte B-tree header.
	// Each entry is a 2-byte offset pointing to a cell's content within the page.
	cellPtrsStart := btreeHeaderStart + 8

	tableCount := 0
	for i := range cellCount {
		ptrOffset := cellPtrsStart + i*2
		cellOffset := int(binary.BigEndian.Uint16(page1[ptrOffset : ptrOffset+2]))
		if readCellType(page1, cellOffset) == "table" {
			tableCount++
		}
	}
	return tableCount
}

// readCellType parses a single table-leaf cell and returns the value of the
// first column (sqlite_schema.type), e.g. "table", "index", "view", "trigger".
func readCellType(page1 []byte, cellOffset int) string {
	// Table-leaf cell layout:
	//   payload_size  (varint)
	//   row_id        (varint)
	//   record        (payload)
	pos := cellOffset
	_, n := readVarint(page1, pos) // skip payload size
	pos += n
	_, n = readVarint(page1, pos) // skip row ID
	pos += n

	// Because varints have no fixed size — you don't know how many bytes they consumed until you read them.
	// readVarint returns (value, bytesConsumed).
	// So pos += n is literally "move the cursor forward by however many bytes that varint used."
	// Record layout:
	//   header_length  (varint, includes itself)
	//   serial_type_0  (varint) ← type column
	//   serial_type_1  ...
	//   value_0        (bytes per serial type)
	//   value_1        ...
	recordStart := pos
	headerLen, n := readVarint(page1, pos)
	pos += n

	// from sqlite.org/fileformat.html — the serial type table.
	// SQLite encodes the type AND length of a value into a single integer called the serial type
	// Example — the word "table" is 5 chars:
	// encode: (5 * 2) + 13 = 23
	// decode: (23 - 13) / 2 = 5
	// TEXT serial type: odd number >= 13, text length = (serialType - 13) / 2
	serialType, _ := readVarint(page1, pos)
	if serialType < 13 || serialType%2 == 0 {
		return ""
	}

	textLen := int((serialType - 13) / 2)
	valuesStart := recordStart + int(headerLen)
	return string(page1[valuesStart : valuesStart+textLen])
}
