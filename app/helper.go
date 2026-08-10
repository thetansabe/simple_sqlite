package main

// readVarint reads a SQLite variable-length integer from data at the given offset.
// Returns the decoded value and the number of bytes consumed.
//
// SQLite varint encoding: each byte uses the high bit as a "more bytes" flag
// and the lower 7 bits as data. The 9th byte (if reached) uses all 8 bits.
// See: https://www.sqlite.org/fileformat.html (search "varint")
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
