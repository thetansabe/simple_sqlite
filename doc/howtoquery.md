# How to Query SQLite

## Table of Contents

- [1. Count rows in a table — `SELECT COUNT(*)`](#1-count-rows-in-a-table--select-count)
  - [1.1 Find the table's rootpage](#11-find-the-tables-rootpage)
  - [1.2 Navigate to that page](#12-navigate-to-that-page)
  - [1.3 Read cell count from the page header](#13-read-cell-count-from-the-page-header)
  - [1.4 Parse the SQL query](#14-parse-the-sql-query)
- [2. Read data from a single column — `SELECT col FROM table`](#2-read-data-from-a-single-column--select-col-from-table)
  - [2.1 Read the CREATE TABLE sql to get column order](#21-read-the-create-table-sql-to-get-column-order)
  - [2.2 Skip to the right column using serialByteLen](#22-skip-to-the-right-column-using-serialbyteLen)
  - [2.3 Read cell pointer array and extract values](#23-read-cell-pointer-array-and-extract-values)
  - [2.4 Within-page vs cross-page traversal](#24-within-page-vs-cross-page-traversal)
  - [2.5 How to verify the test database is small enough](#25-how-to-verify-the-test-database-is-small-enough)
- [3. SELECT on a large table — interior page traversal](#3-select-on-a-large-table--interior-page-traversal)
  - [3.1 What a "page" actually is in a single binary file](#31-what-a-page-actually-is-in-a-single-binary-file)
  - [3.2 What changes when the table is large](#32-what-changes-when-the-table-is-large)
  - [3.3 Interior page cell structure](#33-interior-page-cell-structure)
  - [3.4 Gather all leaf pages via BFS](#34-gather-all-leaf-pages-via-bfs)
  - [3.5 Updated executeSelect flow](#35-updated-executeselect-flow)

---

## 1. Count rows in a table — `SELECT COUNT(*)`

Goal: given `SELECT COUNT(*) FROM apples`, print `4`.

The key insight: SQLite stores every table as a B-tree. Each **cell** in a leaf
page is one row. So counting cells on the root page = counting rows (for a
table small enough to fit on one page).

### 1.1 Find the table's rootpage

`sqlite_schema` (always on page 1) has one row per table, index, view, or
trigger. Column 3 is `rootpage` — the page number where that table's B-tree
starts.

```
sqlite_schema row for "apples":
  col 0  type     = "table"
  col 1  name     = "apples"
  col 2  tbl_name = "apples"
  col 3  rootpage = 2          ← this is what we need
  col 4  sql      = "CREATE TABLE apples (...)"
```

Code: `parseSchemaRows` in `dbinfo.go` reads page 1 and returns every schema
row including `rootpage`. `countTableRows` in `dbinfo.go` loops through them
to find the matching table name.

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ section "Decoding the sqlite_schema table":
> *"The rootpage field in this table is the page at which the root of the table can be located."*

### 1.2 Navigate to that page

Pages are **1-indexed**. Page 1 starts at file offset 0, page 2 at `pageSize`,
page N at `(N-1) * pageSize`.

```go
offset := int64(rootpage-1) * int64(pageSize)
f.Seek(offset, 0)
```

For `rootpage = 2` and `pageSize = 4096`:

```
offset = (2-1) * 4096 = 4096
```

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ same section:
> *"To navigate to page 6 of the database, we need to multiply the page size
> by the page number -1 (as SQLite pages start numbering at 1)."*

### 1.3 Read cell count from the page header

Every B-tree page starts with an 8-byte header (for non-page-1 pages, offset 0).
Bytes 3–4 hold the cell count as a big-endian uint16.

```
page byte 0     = page type  (0x0D = leaf table, 0x05 = interior table)
page bytes 1-2  = first freeblock offset
page bytes 3-4  = cell count  ← number of rows
page bytes 5-6  = cell content area start
page byte 7     = fragmented free bytes
```

```go
cellCount := int(binary.BigEndian.Uint16(page[3:5]))
```

For a **leaf page (0x0D)**, each cell = one row, so `cellCount = COUNT(*)`.

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ section "Decoding the sqlite_schema table", B-tree page header table:
> *"The two-byte integer at offset 3 gives the number of cells on the page."*

### 1.4 Parse the SQL query

Use `sqlparser.Parse` to get a typed AST. `COUNT(*)` comes back as a
`*sqlparser.FuncExpr` with name `"count"`, not a column name. This lets us
dispatch cleanly without string matching.

```go
// select.go — parseSelect()
case *sqlparser.FuncExpr:
    if strings.ToLower(inner.Name.String()) == "count" {
        q.isCount = true
    }
```

```go
// main.go
if q.isCount {
    count, _ := countTableRows(databaseFilePath, q.table)
    fmt.Println(count)
}
```

---

## 2. Read data from a single column — `SELECT col FROM table`

Goal: given `SELECT name FROM apples`, print one name per line.

Three new problems vs COUNT:
1. Need to know **which column index** `name` is (0? 1? 2?).
2. Need to **skip earlier columns** to reach it inside each cell's binary payload.
3. Need to **iterate all cells**, not just count them.

### 2.1 Read the CREATE TABLE sql to get column order

Column values are stored in the record **by position** (index 0, 1, 2…), not
by name. To find the index of `name`, parse the original `CREATE TABLE`
statement stored in `sqlite_schema.sql` (column 4).

```sql
CREATE TABLE apples (
    id    integer primary key autoincrement,
    name  text,    ← index 1
    color text     ← index 2
)
```

```go
// select.go — columnNamesFromSQL()
// Extracts everything between ( and ), splits by comma, takes first token of each.
// Filters out constraint keywords: PRIMARY, UNIQUE, CHECK, FOREIGN, CONSTRAINT.
names := columnNamesFromSQL(row.sql)  // → ["id", "name", "color"]
idx   := columnIndex(row.sql, "name") // → 1
```

We also need `sqlite_schema.sql` in the first place — that's why `schemaRow`
now has a `sql` field and `parseSchemaRow` in `dbinfo.go` reads serial type 4.

### 2.2 Skip to the right column using serialByteLen

Inside a cell's record payload, all column values are packed back-to-back with
no separators. To reach column index `i`, you must **sum the byte widths of
columns 0 through i-1**.

```
record header:  headerLen | serialType[0] | serialType[1] | serialType[2] | ...
record values:  [col 0 bytes][col 1 bytes][col 2 bytes] ...
                 ↑ skip these ↑            ↑ read this ↑
```

`serialByteLen` converts any serial type to its byte width:

| Serial type | Meaning    | Bytes              |
|-------------|------------|--------------------|
| 0           | NULL       | 0                  |
| 1–6         | integer    | 1 / 2 / 3 / 4 / 6 / 8 |
| 7           | float      | 8                  |
| 8, 9        | literal 0/1| 0                  |
| ≥12 even    | BLOB       | `(N-12)/2`         |
| ≥13 odd     | TEXT       | `(N-13)/2`         |

```go
// select.go — readCellColumns()
valPos := recordStart + int(headerLen)  // jump to values section
for i := range colIdx {
    valPos += serialByteLen(serialTypes[i])  // skip columns before target
}
value := readTextValue(page, valPos, serialTypes[colIdx])
```

**Doc source:** [sqlite.org/fileformat.html](https://www.sqlite.org/fileformat.html)
→ section 2.1 "Record Format", serial type table.

### 2.3 Read cell pointer array and extract values

After the 8-byte B-tree page header, there is a **cell pointer array** — one
2-byte big-endian offset per cell, pointing to where that cell's data lives
in the page.

```
page bytes 0-7:   B-tree page header (cellCount at bytes 3-4)
page bytes 8-...: cell pointer array [offset0][offset1][offset2]...
                  each 2 bytes, big-endian, points into the page
...elsewhere...:  actual cell data (packed from the end of the page upward)
```

```go
// select.go — executeSelect()
cellCount := int(binary.BigEndian.Uint16(page[3:5]))
for i := range cellCount {
    ptrOffset  := 8 + i*2
    cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
    values     := readCellColumns(page, cellOffset, colIndices)
    fmt.Println(strings.Join(values, "|"))
}
```

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ section "Examining the root page":
> *"We now have the offsets to all of the records within this page."*

### 2.4 Within-page vs cross-page traversal

**What "interior" means:**

A B-tree is a tree structure with **levels**. When a table grows too big to
fit on one page, SQLite splits it across multiple pages and adds a routing
layer on top. That routing layer is called an **interior page**.

```
SMALL TABLE (all rows fit on 1 page):

  rootpage → [Leaf page 0x0D]
               row1, row2, row3, row4   ← data lives here directly

LARGE TABLE (rows overflow across many pages):

  rootpage → [Interior page 0x05]       ← NO data here, only routing
               ↙          ↓          ↘
         [Leaf 0x0D] [Leaf 0x0D] [Leaf 0x0D]
          rows 1-150  rows 151-300  rows 301-500
```

- **Leaf page (0x0D):** the bottom of the tree. Cells = actual row data.
- **Interior page (0x05):** a middle/top layer. Cells = child page pointers + keys. No row data at all.

The word "interior" comes from tree terminology: leaf nodes are at the edges,
interior nodes are in the middle connecting them.

---

The cell pointer loop above only works when the **rootpage is a leaf (0x0D)**.
There are two distinct kinds of traversal — the loop above handles one, stage
rf3 requires the other:

**Within a single page — cell pointer array** (what sections 1–2.3 implement):
```
Page 2 (leaf 0x0D):
  header bytes 3-4 = 4 cells
  cell pointers: [offset0][offset1][offset2][offset3]
       ↓              ↓         ↓         ↓
  "Granny Smith"   "Fuji"  "Honeycrisp"  "Golden Delicious"
```
All rows live on one page. Loop over cell pointers → done. ✅

**Across multiple pages — interior → leaf navigation** (stage rf3 and beyond):
```
Page 2 (interior 0x05):            ← rootpage, no row data here
  [ptr→page3][key=150][ptr→page4][key=300][right-most→page5]
       ↓                   ↓                    ↓
   Page 3               Page 4               Page 5
   rows 1-150         rows 151-300         rows 301-500
```
To get all rows you must follow each pointer to its child page, then loop over
cells there. If `page[0] == 0x05`, reading `page[3:5]` gives you the count of
**pointer cells**, not rows — wrong answer.

Interior page cells have a different structure than leaf cells:
```
leaf cell:     [payload_size varint][rowid varint][record bytes...]
interior cell: [left_child_page uint32][rowid varint]   ← no record, just routing
```

Interior page header is also **12 bytes** (not 8) — it has an extra 4-byte
right-most pointer at bytes 8–11. Cell pointer array starts at byte 12.

The fix for stage rf3 is to check `page[0]` first:
```go
if page[0] == 0x0D {
    // leaf: cell pointer array at offset 8, cells contain row data
} else if page[0] == 0x05 {
    // interior: cell pointer array at offset 12, cells contain child page numbers
    // recurse into each child page to collect rows
}
```

### 2.5 How to verify the test database is small enough

Run these commands to confirm every table fits on a single leaf page and no
cross-page traversal is needed.

**Check total page count:**
```sh
ls -la sample.db
sqlite3 sample.db "PRAGMA page_size; PRAGMA page_count;"
```
```
-rw-r--r-- 16384 sample.db
4096   ← page size
4      ← total pages

16384 / 4096 = 4 pages:
  page 1 = sqlite_schema
  page 2 = apples
  page 3 = sqlite_sequence
  page 4 = oranges
```
4 pages for 3 tables + schema = 1 page each. No room for interior pages.

**Confirm rootpage values:**
```sh
sqlite3 sample.db "SELECT name, rootpage FROM sqlite_schema WHERE type='table';"
```
```
apples|2
sqlite_sequence|3
oranges|4
```

**Check the page type byte directly:**
```sh
# byte 0 of page 2 (offset 4096) = page type
xxd -s 4096 -l 1 sample.db
```
```
00001000: 0d   ← 0x0D = leaf table ✅  (0x05 = interior = needs traversal)
```

**Rule of thumb:** if a future test database has `PRAGMA page_count` much
larger than the number of tables, some tables span multiple pages and you need
cross-page traversal.


## 3. SELECT on a large table — interior page traversal

### 3.1 What a "page" actually is in a single binary file

SQLite is a **single binary file** with no sub-directories, no index file, no
separate metadata. The "page" concept is just a way to divide that one file into
equal-sized slots — like an array of fixed-size blocks:

```
sqlite.db (one file):
┌──────────┬──────────┬──────────┬──────────┬──────────┐
│  page 1  │  page 2  │  page 3  │  page 4  │  page 5  │
│  4096 B  │  4096 B  │  4096 B  │  4096 B  │  4096 B  │
└──────────┴──────────┴──────────┴──────────┴──────────┘
  offset 0   offset     offset     offset     offset
             4096       8192       12288      16384
```

**Critical point: the page header does NOT store the range of the page.**
Finding page N is pure arithmetic — no directory, no range pointer:

```go
offset := int64(pageNum-1) * int64(pageSize)  // that's it
```

The page size (e.g. 4096) is stored **once** at bytes 16–17 of the very first
page (the 100-byte file header). After that, every page is the same fixed size.

The **page header** (first bytes of each page slot) stores:
- Byte 0: page **type** (`0x0D` = leaf table, `0x05` = interior table)
- Bytes 3–4: **cell count** — how many cells are on this page
- Bytes 8–11: **rightmost child pointer** (interior pages only — explains below)

It tells you *what's inside*, not *where the page is* (that's already known from
arithmetic). Think of the header as the label on a drawer, not a map to find it.

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "building blocks" in section "Examining the database":
> *"Pages are the building blocks of the SQLite database and the entire database
> is made up of pages."*
> *"It is worth noting that SQLite pages start numbering from one and not zero."*

→ search "database page size" in section "Examining the database header"
(the header table row at offset 16):
> *"The database page size in bytes. Must be a power of two between 512 and 32768
> inclusive, or the value 1 representing a page size of 65536."*

---

### 3.2 What changes when the table is large

For a small table (our test db), rootpage is a **leaf** — it holds row data directly:

```
rootpage (leaf 0x0D):
  header: 8 bytes  ← cell pointers start at offset 8
  cells: [row1][row2][row3][row4]   ← actual data
```

For a large table (hundreds of rows), rootpage becomes an **interior** page — a
routing layer that holds child pointers, not data:

```
rootpage (interior 0x05):
  header: 12 bytes  ← 4 extra bytes at [8:12] = rightmost child pointer
  cell pointers start at offset 12 (not 8!)
  cells: [ptr→page3][key=150] [ptr→page4][key=300]
              ↓                      ↓
          page 3 (leaf)          page 4 (leaf)        page 5 (leaf) ← rightmost
          rows 1–150             rows 151–300          rows 301–500
```

**Two differences from leaf pages:**

| | Leaf (`0x0D`) | Interior (`0x05`) |
|---|---|---|
| Header size | 8 bytes | 12 bytes (extra 4 = rightmost child) |
| Cell pointer array starts at | offset 8 | offset 12 |
| Cell content | `[payload_size varint][rowid varint][record…]` | `[uint32 left_child][varint key]` |
| Contains row data | ✅ | ❌ routing only |

If you read an interior page as if it were a leaf, `page[3:5]` gives the number
of routing cells (e.g. 3), not row count — and the "cell data" you read is child
page pointers, not records. The output is garbage. ❌

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "interior page or a leaf page" in section "SQLite and b-tree":
> *"A b-tree page is either an interior page or a leaf page. A leaf page contains
> keys and in the case of a table b-tree each key has associated data. An interior
> page contains K keys together with K+1 pointers to child b-tree pages. A
> 'pointer' in an interior b-tree page is just the 32-bit unsigned integer page
> number of the child page."*

→ search "b-tree page type" in section "Decoding the sqlite_schema table"
(the page header table) for the `0x0D`/`0x05` type byte values.

---

### 3.3 Interior page cell structure

Each cell on an interior page encodes one boundary between two child pages:

```
interior cell layout (sqlite.org/fileformat.html section 2.3.3):

  bytes 0–3:  uint32 big-endian  → left child page number
  bytes 4–…:  varint             → key (rowid boundary — rows ≤ key go left)
```

The **rightmost child** is not in a cell — it lives in the page header at bytes
8–11 (a 4-byte big-endian page number). This is why the interior header is 12
bytes instead of 8.

Reading all children of an interior page in order:

```go
// header bytes 8-11 = rightmost child
rightmost := int(binary.BigEndian.Uint32(page[8:12]))

// cell pointer array starts at offset 12 for interior pages
cellCount := int(binary.BigEndian.Uint16(page[3:5]))
for i := range cellCount {
    ptrOffset  := 12 + i*2
    cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
    leftChild  := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
    // leftChild is a page number — seek to (leftChild-1)*pageSize to read it
}
// after all cells, also process rightmost
```

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "right-most pointer" in section "Decoding the sqlite_schema table"
(the page header table row at offset 8):
> *"The four-byte page number at offset 8 is the right-most pointer. This value
> appears in the header of interior b-tree pages only and is omitted from all
> other pages."*

→ search "4 byte big endian integer that is the page number of the left child"
in the same section:
> *"So at each cell offset we are expecting a 4 byte big endian integer that is
> the page number of the left child and a single varint immediately after."*

---

### 3.4 Gather all leaf pages via BFS

Because interior pages can themselves have interior children (for very large
tables), the correct approach is a breadth-first or depth-first traversal
starting from rootpage, collecting every leaf page:

```
BFS queue starting with [rootPageNum]:

  dequeue page N
  read page N from file at (N-1)*pageSize

  if page[0] == 0x0D  (leaf):
      add to leaves list → iterate its cells for row data

  if page[0] == 0x05  (interior):
      for each cell in pointer array (starting at offset 12):
          enqueue left_child page number
      enqueue rightmost child (from page[8:12])
```

In Go:

```go
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
        case 0x0D: // leaf table
            leaves = append(leaves, page)

        case 0x05: // interior table
            cellCount := int(binary.BigEndian.Uint16(page[3:5]))
            for i := range cellCount {
                ptrOffset  := 12 + i*2
                cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
                leftChild  := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
                queue = append(queue, leftChild)
            }
            rightmost := int(binary.BigEndian.Uint32(page[8:12]))
            queue = append(queue, rightmost)
        }
    }
    return leaves, nil
}
```

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "map out the other pages" at the end of the article:
> *"In order to decode the data for the 'albums' table, we would go to page 2
> (offset 1024) and decode the page header there. If it was an interior page,
> we would have to map out the other pages associated with it, if it was a leaf
> page then we could extract the records from this page."*

---

### 3.5 Updated executeSelect flow

With `gatherLeafPages`, `executeSelect` changes only at the "read page" step:

```
Before (single leaf assumed):
  seek to rootpage → read page → iterate cells

After (handles interior + leaf):
  gatherLeafPages(rootPageNum) → returns [][]byte (all leaf pages)
  for each leaf page:
      iterate cells (same readCellColumns logic, unchanged)
```

The WHERE filter, column mapping, and `readCellColumns` are all unchanged — they
operate on one cell at a time and don't care which page that cell came from.

---

### 3.6 The distinction: within-page vs cross-page (summary)

**Within a single page — cell pointer array** (small table, leaf rootpage):
```
Page 2 (leaf 0x0D):
  header bytes 3-4 = 4 cells
  cell pointers: [offset0][offset1][offset2][offset3]
       ↓              ↓         ↓         ↓
  "Granny Smith"   "Fuji"  "Honeycrisp"  "Golden Delicious"
```
We loop over all 4 pointers on this one page and get all 4 rows. ✅

**Across multiple pages — interior → leaf navigation** (large table):
```
Page 2 (interior 0x05):            ← rootpage points here
  [ptr→page3][key=150][ptr→page4][key=300][right-most→page5]
       ↓                   ↓                    ↓
   Page 3               Page 4               Page 5
   rows 1-150         rows 151-300         rows 301-500
```
Must follow each pointer to a child page, then loop over cells there.
Reading page 2 as if it were a leaf produces garbage — its "cells" are routing
entries, not records. ❌

---

### 3.7 How to verify the test database is small enough

Run these two commands. If every rootpage is a leaf, you're safe to read cells
directly without any cross-page traversal.

**Step 1 — check total pages vs page size:**

```sh
# file size / page size = total pages
# if total pages == number of tables + 1 (for sqlite_schema),
# there is literally no room for interior pages
ls -la sample.db
sqlite3 sample.db "PRAGMA page_size; PRAGMA page_count;"
```

Example output for our test database:
```
-rw-r--r-- 16384 sample.db

4096   ← page size
4      ← total pages

16384 / 4096 = 4 pages total
page 1 = sqlite_schema
page 2 = apples        (rootpage=2)
page 3 = sqlite_sequence
page 4 = oranges       (rootpage=4)
```
4 pages, 3 tables + schema = exactly 1 page each. No interior pages possible.

**Step 2 — confirm rootpage values:**

```sh
sqlite3 sample.db "SELECT name, rootpage FROM sqlite_schema WHERE type='table';"
```

Example output:
```
apples|2
sqlite_sequence|3
oranges|4
```

Each table has its own single page. If a table had an interior page, it would
consume at least 3 pages (1 interior + 2 leaves minimum), and you'd see gaps
or a much larger file.

**Step 3 — check page type byte directly** (byte 0 of the page):

```sh
sqlite3 sample.db "SELECT hex(data) FROM (
  SELECT substr(data,1,1) as data
  FROM (SELECT readfile('sample.db') as data)
)" 2>/dev/null || \
# simpler: use xxd to read byte 4096 (start of page 2)
xxd -s 4096 -l 1 sample.db
```

```
# page 2, byte 0:
0x0d   ← leaf table page ✅
# if it were 0x05 = interior table page, you'd need cross-page traversal
```

**Rule of thumb:** if `file_size / page_size <= number_of_tables + 2`, every
table fits on one page and you never need to traverse interior nodes.

