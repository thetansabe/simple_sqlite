package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	// Available if you need it!
	// "github.com/xwb1989/sqlparser"
)

// readVarint reads a SQLite variable-length integer from data at the given offset.
// Returns the decoded value and the number of bytes consumed.
//
// SQLite varint encoding: each byte uses the high bit as a "more bytes" flag
// and the lower 7 bits as data. The 9th byte (if reached) uses all 8 bits.
func readVarint(data []byte, offset int) (int64, int) {
	var result int64
	for i := 0; i < 9; i++ {
		b := data[offset+i]
		if i == 8 {
			result = (result << 8) | int64(b)
			return result, 9
		}
		result = (result << 7) | int64(b&0x7f)
		if b&0x80 == 0 {
			return result, i + 1
		}
	}
	return result, 9
}

// Usage: your_program.sh sample.db .dbinfo
func main() {
	databaseFilePath := os.Args[1]
	command := os.Args[2]

	switch command {
	case ".dbinfo":
		databaseFile, err := os.Open(databaseFilePath)
		if err != nil {
			log.Fatal(err)
		}

		// The first 100 bytes of every SQLite file are the file header.
		// Byte offsets 16-17 hold the page size as a big-endian uint16.
		fileHeader := make([]byte, 100)
		if _, err = databaseFile.Read(fileHeader); err != nil {
			log.Fatal(err)
		}
		pageSize := int(binary.BigEndian.Uint16(fileHeader[16:18]))

		fmt.Printf("database page size: %v\n", pageSize)

		// Page 1 holds the sqlite_schema table (all tables/indexes/views/triggers).
		// Re-read the full first page from the beginning of the file.
		page1 := make([]byte, pageSize)
		databaseFile.Seek(0, 0)
		if _, err = databaseFile.Read(page1); err != nil {
			log.Fatal(err)
		}

		// The B-tree page header for page 1 starts at byte 100 (right after
		// the file header). For a leaf table page the header layout is:
		//   [0]      page type  (0x0d = table leaf)
		//   [1-2]    first freeblock offset
		//   [3-4]    number of cells  ← we want this
		//   [5-6]    cell content area start
		//   [7]      fragmented free bytes
		btreeHeaderStart := 100
		cellCount := int(binary.BigEndian.Uint16(page1[btreeHeaderStart+3 : btreeHeaderStart+5]))

		// The cell pointer array immediately follows the 8-byte B-tree header.
		// Each entry is a 2-byte offset from the start of the page pointing to
		// the cell's content.
		cellPtrsStart := btreeHeaderStart + 8

		tableCount := 0
		for i := 0; i < cellCount; i++ {
			ptrOffset := cellPtrsStart + i*2
			cellOffset := int(binary.BigEndian.Uint16(page1[ptrOffset : ptrOffset+2]))

			// Each table-leaf cell is laid out as:
			//   payload_size  (varint)
			//   row_id        (varint)
			//   payload       (record)
			pos := cellOffset
			_, n := readVarint(page1, pos) // payload size
			pos += n
			_, n = readVarint(page1, pos) // row ID
			pos += n

			// The payload is a SQLite record:
			//   header_length  (varint, includes itself)
			//   serial_type_0  (varint) ← type column of sqlite_schema
			//   serial_type_1  (varint)
			//   ...
			//   value_0        (bytes determined by serial_type_0)
			//   value_1        ...
			recordStart := pos
			headerLen, n := readVarint(page1, pos)
			pos += n

			// Read serial type for the first column (sqlite_schema.type).
			// TEXT serial type: odd number >= 13; text length = (serialType - 13) / 2
			serialType, _ := readVarint(page1, pos)

			valuesStart := recordStart + int(headerLen)
			if serialType >= 13 && serialType%2 == 1 {
				textLen := int((serialType - 13) / 2)
				typeValue := string(page1[valuesStart : valuesStart+textLen])
				if typeValue == "table" {
					tableCount++
				}
			}
		}

		fmt.Printf("number of tables: %v\n", tableCount)

	default:
		fmt.Println("Unknown command", command)
		os.Exit(1)
	}
}
