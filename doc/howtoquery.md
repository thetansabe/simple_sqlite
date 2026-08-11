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
- [4. SELECT WHERE on an indexed column — index scan](#4-select-where-on-an-indexed-column--index-scan)
  - [4.1 Why a full table scan is too slow](#41-why-a-full-table-scan-is-too-slow)
  - [4.2 What an index B-tree is and how it differs from a table B-tree](#42-what-an-index-b-tree-is-and-how-it-differs-from-a-table-b-tree)
  - [4.3 Index B-tree cell structure](#43-index-b-tree-cell-structure)
  - [4.4 Step 1 — find the index rootpage in sqlite_schema](#44-step-1--find-the-index-rootpage-in-sqlite_schema)
  - [4.5 Step 2 — search the index B-tree to collect matching rowids](#45-step-2--search-the-index-b-tree-to-collect-matching-rowids)
  - [4.6 Step 3 — look up each rowid in the table B-tree](#46-step-3--look-up-each-rowid-in-the-table-b-tree)
  - [4.7 Step 4 — wire it all up in executeSelect](#47-step-4--wire-it-all-up-in-executeselect)
  - [4.8 Special case: id is the rowid itself](#48-special-case-id-is-the-rowid-itself)
  - [4.9 Verification commands](#49-verification-commands)
- [5. Real O(log N) index B-tree descent](#5-real-olog-n-index-b-tree-descent)
  - [5.1 The core insight: index B-tree is sorted](#51-the-core-insight-index-b-tree-is-sorted)
  - [5.2 How interior node keys work in the index B-tree](#52-how-interior-node-keys-work-in-the-index-b-tree)
  - [5.3 The descent algorithm](#53-the-descent-algorithm)
  - [5.4 What if there are many 'eritrea'](#54-after-landing-on-the-first-matching-leaf-scan-forward)
  - [5.5 Go implementation](#55-go-implementation)
  - [5.6 Comparison: full index scan vs true descent](#56-comparison-full-index-scan-vs-true-descent)

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

> _"The rootpage field in this table is the page at which the root of the table can be located."_

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

> _"To navigate to page 6 of the database, we need to multiply the page size
> by the page number -1 (as SQLite pages start numbering at 1)."_

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

> _"The two-byte integer at offset 3 gives the number of cells on the page."_

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

| Serial type | Meaning     | Bytes                 |
| ----------- | ----------- | --------------------- |
| 0           | NULL        | 0                     |
| 1–6         | integer     | 1 / 2 / 3 / 4 / 6 / 8 |
| 7           | float       | 8                     |
| 8, 9        | literal 0/1 | 0                     |
| ≥12 even    | BLOB        | `(N-12)/2`            |
| ≥13 odd     | TEXT        | `(N-13)/2`            |

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

> _"We now have the offsets to all of the records within this page."_

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

It tells you _what's inside_, not _where the page is_ (that's already known from
arithmetic). Think of the header as the label on a drawer, not a map to find it.

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "building blocks" in section "Examining the database":

> _"Pages are the building blocks of the SQLite database and the entire database
> is made up of pages."_
> _"It is worth noting that SQLite pages start numbering from one and not zero."_

→ search "database page size" in section "Examining the database header"
(the header table row at offset 16):

> _"The database page size in bytes. Must be a power of two between 512 and 32768
> inclusive, or the value 1 representing a page size of 65536."_

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

|                              | Leaf (`0x0D`)                                  | Interior (`0x05`)                    |
| ---------------------------- | ---------------------------------------------- | ------------------------------------ |
| Header size                  | 8 bytes                                        | 12 bytes (extra 4 = rightmost child) |
| Cell pointer array starts at | offset 8                                       | offset 12                            |
| Cell content                 | `[payload_size varint][rowid varint][record…]` | `[uint32 left_child][varint key]`    |
| Contains row data            | ✅                                             | ❌ routing only                      |

If you read an interior page as if it were a leaf, `page[3:5]` gives the number
of routing cells (e.g. 3), not row count — and the "cell data" you read is child
page pointers, not records. The output is garbage. ❌

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "interior page or a leaf page" in section "SQLite and b-tree":

> _"A b-tree page is either an interior page or a leaf page. A leaf page contains
> keys and in the case of a table b-tree each key has associated data. An interior
> page contains K keys together with K+1 pointers to child b-tree pages. A
> 'pointer' in an interior b-tree page is just the 32-bit unsigned integer page
> number of the child page."_

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

> _"The four-byte page number at offset 8 is the right-most pointer. This value
> appears in the header of interior b-tree pages only and is omitted from all
> other pages."_

→ search "4 byte big endian integer that is the page number of the left child"
in the same section:

> _"So at each cell offset we are expecting a 4 byte big endian integer that is
> the page number of the left child and a single varint immediately after."_

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

> _"In order to decode the data for the 'albums' table, we would go to page 2
> (offset 1024) and decode the page header there. If it was an interior page,
> we would have to map out the other pages associated with it, if it was a leaf
> page then we could extract the records from this page."_

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

---

## 4. SELECT WHERE on an indexed column — index scan

Goal: given `SELECT id, name FROM companies WHERE country = 'eritrea'`, return
only matching rows — fast, even on a ~1 GB database.

### 4.1 Why a full table scan is too slow

The stage tester uses a ~1 GB database and expects results in **< 3 seconds**.
A full table scan (`gatherLeafPages` → read every row → filter) must read
hundreds of thousands of cells. On a 1 GB file that's seconds of I/O.

### 4.1.1 Three levels of "index scan" — and which one we implement

There are three levels of search strategy. It's important to understand the
difference:

| Level                  | What is scanned                         | Cost            | Notes                                                        |
| ---------------------- | --------------------------------------- | --------------- | ------------------------------------------------------------ |
| **Full table scan**    | Every table data page                   | `O(table size)` | ~1 GB reads — too slow                                       |
| **Full index scan**    | Every index page                        | `O(index size)` | Index is much smaller — see 4.1.2. Passes the stage.         |
| **True B-tree search** | Only pages on the path to matching keys | `O(log N)`      | Descend like binary search — never visits unrelated branches |

**What 4.5 implements: a full index scan** — it BFS-walks every index page,
which is still `O(index size)`. This passes the stage because the index is much
smaller than the table. Going from reading 1 GB → reading ~100 MB is enough to
finish in < 3 seconds.

**True B-tree search** (Level 3) is how production databases work: at each
index interior node, compare the search value to cell keys and follow only the
branch that could contain the target value — exactly like `lookupByRowid` does
for rowids. You'd only read `O(log N)` pages. This is not required for the
stage, but is documented in 4.5.1 for understanding.

### 4.1.2 Why is the index smaller than the table? How much smaller?

**The source claim:** askclees.com says:

> _"Index b-trees use arbitrary keys and **stores no data at all**."_
> → search "Index b-trees use arbitrary keys" in https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/

"No data" means: the index B-tree does **not** copy the full row into its cells.
An index leaf cell stores only `(indexed_col_value, rowid)` — just two values,
not the entire row. The full row stays in the table B-tree.

**The math for companies table:**

```
Each table row stores (10 columns):
  id, name, domain, year_founded, industry, size range,
  locality, country, current_employees, total_employees
  → ~100–200 bytes per row average

Each index entry stores (2 values):
  country (TEXT, ~10 bytes avg) + rowid (INT, ~4 bytes)
  → ~15 bytes per entry
```

So the index is roughly `15 / 150 = 1/10th` the size of the table.

**Verified with the sample companies.db (7.6 MB, same schema, smaller dataset):**

```sh
sqlite3 companies.db "SELECT name, count(*) as pages FROM dbstat GROUP BY name ORDER BY pages DESC;"
```

```
companies              | 1664 pages  ← table data
idx_companies_country  |  244 pages  ← index
sqlite_sequence        |    1 page
sqlite_schema          |    1 page
```

The index uses **244 / 1664 = ~15%** of the table's page count — about 1/7th.
For the 1 GB production database (same schema, more rows), the ratio is similar:
table ~1 GB, index ~150 MB.

**There is no fixed "max size" for an index relative to a table** — it depends
entirely on how wide the indexed column is vs. the average row size. Country
names are short; if you indexed a 500-byte text column on a table with short
rows, the index could theoretically approach the table size. In practice,
indexes on short columns (IDs, short strings, numbers) are always much smaller.

**Verify yourself:**

```sh
sqlite3 companies.db "PRAGMA page_size;"    # bytes per page
sqlite3 companies.db "SELECT name, count(*) as pages, count(*) * 4096 as bytes
  FROM dbstat GROUP BY name ORDER BY pages DESC;"
```

### 4.2 What an index B-tree is and how it differs from a table B-tree

SQLite stores two completely separate B-trees in the same file: one for the
table data, one for each index. Both live as pages in the same `.db` file.
Their rootpages are stored in `sqlite_schema`.

| Property                      | Table B-tree                             | Index B-tree                               |
| ----------------------------- | ---------------------------------------- | ------------------------------------------ |
| `sqlite_schema.type`          | `"table"`                                | `"index"`                                  |
| Leaf page type byte           | `0x0D`                                   | `0x0A`                                     |
| Interior page type byte       | `0x05`                                   | `0x02`                                     |
| Leaf cell has rowid prefix?   | ✅ yes — `[payload_size][rowid][record]` | ❌ no — `[payload_size][record]`           |
| What is stored in the record? | all row columns                          | `[indexed_col_value, rowid]`               |
| Sorted by                     | rowid                                    | indexed column value (then rowid for ties) |
| Purpose                       | fetch row data by rowid                  | map a column value → rowid(s)              |

The critical difference: **index leaf cells do not have a rowid varint prefix**.
The rowid is stored _inside_ the record, as the last column.

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "index b-tree" and "index leaf cell"

### 4.3 Index B-tree cell structure

Index leaf cell (`0x0A` page):

```
[varint: payload_size]          ← total bytes of the record that follows
[record]
  [varint: headerLen]           ← header length including itself
  [varint: serial_type[0]]      ← type of indexed_col_value (TEXT for country)
  [varint: serial_type[1]]      ← type of rowid (integer serial type 1–6)
  [indexed_col_value bytes]     ← e.g. "eritrea" (7 bytes)
  [rowid bytes]                 ← e.g. 121311 as 3-byte big-endian int
```

Notice: **no rowid varint before the record**. Compare to table leaf cells
which start `[payload_size][rowid varint][record]`.

Index interior cell (`0x02` page) — same structure as table interior (`0x05`):

```
[uint32 big-endian: left child page number]
[varint: key]    ← here the key is the indexed value + rowid (used for routing)
```

Cell pointer array offset:

- Index leaf (`0x0A`): 8-byte header → pointers start at offset **8**
- Index interior (`0x02`): 12-byte header → pointers start at offset **12**

This is the same rule as table pages (`0x0D` = 8, `0x05` = 12) — the page
type byte determines the header size.

### 4.4 Step 1 — find the index rootpage in sqlite_schema

`sqlite_schema` has one row per index:

```
col 0  type     = "index"
col 1  name     = "idx_companies_country"
col 2  tbl_name = "companies"              ← which table this index covers
col 3  rootpage = 7                        ← root of the index B-tree
col 4  sql      = "CREATE INDEX idx_companies_country ON companies (country)"
```

To find the right index:

1. Look for rows where `type = "index"` and `tbl_name = q.table`
2. Parse the `sql` to extract the indexed column name (the text inside `(...)`)
3. If that column matches `q.where.col` → use this index's `rootpage`

> This requires `schemaRow` to store `tblName` (col 2), which was previously
> skipped. Add it to the struct and read it in `parseSchemaRow`.

```go
// Find index for the WHERE column
var indexRootPage int
for _, row := range schemaRows {
    if row.typ == "index" && strings.EqualFold(row.tblName, q.table) {
        // extract column from "CREATE INDEX ... ON table (col)"
        idxCol := indexedColumn(row.sql)
        if strings.EqualFold(idxCol, q.where.col) {
            indexRootPage = row.rootpage
            break
        }
    }
}
```

```go
// indexedColumn extracts the column name from a CREATE INDEX statement.
// e.g. "CREATE INDEX idx_companies_country ON companies (country)" → "country"
func indexedColumn(createIndexSQL string) string {
    start := strings.LastIndex(createIndexSQL, "(")
    end   := strings.LastIndex(createIndexSQL, ")")
    if start == -1 || end == -1 { return "" }
    return strings.TrimSpace(createIndexSQL[start+1 : end])
}
```

### 4.5 Step 2 — search the index B-tree to collect matching rowids

**This is a full index scan** — it BFS-walks every index leaf page and compares
each cell's value. The index is much smaller than the table (it only stores
`(country, rowid)` pairs, not all 10 columns), so this is fast enough to pass.

Walk every leaf page of the index B-tree (using the same BFS as `gatherLeafPages`
but for index page types `0x0A` / `0x02`), then for each leaf cell:

- Read `indexed_col_value` (TEXT, col 0 of the index record)
- If it matches `q.where.val`, read `rowid` (col 1 of the index record)

```go
func searchIndex(f *os.File, pageSize, indexRoot int, searchVal string) ([]int64, error) {
    var rowids []int64
    queue := []int{indexRoot}
    for len(queue) > 0 {
        pageNum := queue[0]
        queue = queue[1:]
        page, _ := readPageN(f, pageSize, pageNum)

        switch page[0] {
        case 0x0A: // index leaf — contains (indexed_value, rowid) pairs
            cellCount := int(binary.BigEndian.Uint16(page[3:5]))
            for i := range cellCount {
                ptrOffset  := 8 + i*2
                cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
                val, rowid := readIndexCell(page, cellOffset)
                if strings.EqualFold(val, searchVal) {
                    rowids = append(rowids, rowid)
                }
            }
        case 0x02: // index interior — enqueue children (same layout as 0x05)
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
    return rowids, nil
}
```

```go
// readIndexCell reads (indexed_value, rowid) from one index leaf cell.
// Index leaf cell layout: [payload_size varint][record]
// Record: [headerLen][serialType[0]][serialType[1]][value0_bytes][rowid_bytes]
// No rowid prefix before the record (unlike table leaf cells).
func readIndexCell(page []byte, cellOffset int) (string, int64) {
    pos := cellOffset
    _, n := readVarint(page, pos)
    pos += n // skip payload size — no rowid varint here

    recordStart := pos
    headerLen, n := readVarint(page, pos)
    pos += n

    serialType0, n := readVarint(page, pos); pos += n // indexed col type
    serialType1, _  := readVarint(page, pos)           // rowid type

    valuesStart := recordStart + int(headerLen)

    // col 0: the indexed value (TEXT)
    val := readTextValue(page, valuesStart, serialType0)

    // col 1: the rowid (integer)
    rowidOffset := valuesStart + serialByteLen(serialType0)
    rowid := readIntValue(page, rowidOffset, serialType1)

    return val, rowid
}
```

#### 4.5.1 How a true B-tree search would work (not required for this stage)

Because the index B-tree is **sorted by indexed column value**, you can descend
it like a binary search — never visiting branches that can't contain the target.

At each index interior node (`0x02`), cells contain `[left_child | key_record]`
where `key_record` holds an `(indexed_value, rowid)` pair. Compare
`searchVal` against each cell's key value:

- `searchVal <= key_value` → follow `left_child`
- `searchVal > all keys` → follow `rightmost` (from header bytes 8–11)

This is identical in structure to `lookupByRowid` — just comparing text strings
instead of integer rowids. You'd descend directly to the leaf page range
containing `'eritrea'`, then scan forward while `val == searchVal`.

This skips all pages for `'a*'`, `'b*'`, ... `'d*'` countries entirely.

### 4.6 Step 3 — look up each rowid in the table B-tree

Given a `rowid`, descend the **table** B-tree from `tableRootPage` to find
the exact cell. This is a directed descent — no full scan needed.

```
Interior page: each cell has [left_child_page | key_rowid]
  → if target_rowid <= key_rowid: follow left_child
  → if target_rowid > all keys: follow rightmost child
Leaf page: scan cells, read rowid varint, compare
```

```go
func lookupByRowid(f *os.File, pageSize, tableRoot int, rowid int64) ([]byte, int, error) {
    pageNum := tableRoot
    for {
        page, _ := readPageN(f, pageSize, pageNum)

        switch page[0] {
        case 0x0D: // leaf — scan cells for matching rowid
            cellCount := int(binary.BigEndian.Uint16(page[3:5]))
            for i := range cellCount {
                ptrOffset  := 8 + i*2
                cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
                cellRowid  := readCellRowid(page, cellOffset)
                if cellRowid == rowid {
                    return page, cellOffset, nil
                }
            }
            return nil, 0, fmt.Errorf("rowid %d not found", rowid)

        case 0x05: // interior — follow the right child branch
            cellCount := int(binary.BigEndian.Uint16(page[3:5]))
            nextPage  := int(binary.BigEndian.Uint32(page[8:12])) // default: rightmost
            for i := range cellCount {
                ptrOffset  := 12 + i*2
                cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
                leftChild  := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
                key, _     := readVarint(page, cellOffset+4)
                if rowid <= key {
                    nextPage = leftChild
                    break
                }
            }
            pageNum = nextPage
        }
    }
}
```

```go
// readCellRowid reads the rowid varint from a table leaf cell.
// Table leaf cell layout: [payload_size varint][rowid varint][record...]
func readCellRowid(page []byte, cellOffset int) int64 {
    pos := cellOffset
    _, n   := readVarint(page, pos); pos += n // skip payload size
    rowid, _ := readVarint(page, pos)
    return rowid
}
```

### 4.7 Step 4 — wire it all up in executeSelect

```go
// In executeSelect, after finding rootpage and colIndices:
if q.where != nil && indexRootPage != 0 {
    // Index scan path
    rowids, _ := searchIndex(f, pageSize, indexRootPage, q.where.val)
    for _, rowid := range rowids {
        page, cellOffset, err := lookupByRowid(f, pageSize, tableRootPage, rowid)
        if err != nil { continue }
        values := readCellColumns(page, cellOffset, colIndices)
        fmt.Println(strings.Join(values, "|"))
    }
} else {
    // Full table scan (existing gatherLeafPages path)
    ...
}
```

### 4.8 Special case: `id` is the rowid itself

In the companies table:

```sql
CREATE TABLE companies (id integer primary key autoincrement, name text, ...)
```

`id integer primary key` means the `id` column IS the rowid — it is not stored
as a separate byte in the record. Instead, serial type 0 (NULL) is stored for
`id` in the record header, and the actual value is the cell's rowid varint.

So in `readCellColumns`, when you encounter `serialType == 0` for the `id`
column, `readTextValue` returns `""` — which is wrong.

You need a special case: **if the column is an INTEGER PRIMARY KEY, return the
rowid varint as a string** instead of reading from the record values.

```go
// In readCellColumns — after reading rowid:
_, n = readVarint(page, pos)  // currently: skip rowid
// Change to:
rowid, n := readVarint(page, pos)  // save it

// Then when reading a requested column and serialTypes[colIdx] == 0:
if serialTypes[colIdx] == 0 {
    result[r] = fmt.Sprintf("%d", rowid)  // rowid is the value
}
```

**How to detect it:** serial type `0` means NULL in general, but for `INTEGER
PRIMARY KEY` columns SQLite specifically stores type `0` to signal "value is
the rowid". In practice: if serial type is `0`, use the rowid.

### 4.9 Verification commands

```sh
# Confirm the index exists and its rootpage
sqlite3 companies.db "SELECT type, name, tbl_name, rootpage FROM sqlite_schema;"

# Check index rootpage is an index interior page (0x02)
# Page size default 4096, index rootpage = N → offset (N-1)*4096, byte 0
sqlite3 companies.db "PRAGMA page_size;"
# e.g. rootpage=3 → offset 8192 → xxd -s 8192 -l 1 companies.db
# expect: 02 (index interior)

# Expected output for eritrea query
sqlite3 companies.db "SELECT id, name FROM companies WHERE country = 'eritrea';"
# 121311|unilink s.c.
# 2102438|orange asmara it solutions
# 5729848|zara mining share company
# 6634629|asmara rental

# Time your implementation vs full scan
time sqlite3 companies.db "SELECT id, name FROM companies WHERE country = 'eritrea';"
time ./your_program.sh companies.db "SELECT id, name FROM companies WHERE country = 'eritrea'"
```

---

## 5. Real O(log N) index B-tree descent

This section explains how a **true B-tree search** works — the Level 3 from
section 4.1.1. Not required for the CodeCrafters stage, but this is how
production databases actually use indexes.

### 5.1 The core insight: index B-tree is sorted

The index B-tree stores entries sorted **alphabetically by the indexed column
value** (then by rowid for ties). This means entries for `'eritrea'` are all
clustered together on consecutive leaf pages — none are scattered randomly.

```
Index B-tree sorted order (conceptual):
  ... 'egypt' ... 'el salvador' ... 'eritrea' ... 'eritrea' ... 'estonia' ...
                                    ↑ first match              ↑ last match
```

Because it's sorted, you can **binary-search** the tree: at each interior node,
look at the dividing keys and follow only the one branch that could contain
`'eritrea'`. All other branches are guaranteed to not contain it — skip them.

This is exactly how `lookupByRowid` works in section 4.6 — same algorithm, but
comparing text strings instead of integer rowids.

**Doc source:** [askclees.com](https://askclees.com/2020/11/20/sqlite-databases-at-hex-level/)
→ search "All records that are less than or equal to 7 are stored in the left
hand page" — same principle, just with text keys instead of integers.

### 5.2 How interior node keys work in the index B-tree

Recall interior page (`0x02`) cell layout (same as table interior `0x05`):

```
[uint32: left_child_page_number]
[varint: payload_size]
[record: (indexed_value, rowid)]   ← the dividing key
```

Each cell in an interior page is a **dividing key**: everything in the
`left_child` subtree has a value `≤` this key; everything to the right has
a value `>` this key.

```
Interior page example (country index):
  cell 0: left_child=page5, key=('ecuador', rowid_X)
  cell 1: left_child=page8, key=('ethiopia', rowid_Y)
  rightmost child: page12

Meaning:
  page5 subtree: all entries ≤ ('ecuador', rowid_X)
  page8 subtree: ('ecuador', rowid_X) < entries ≤ ('ethiopia', rowid_Y)
  page12 subtree: entries > ('ethiopia', rowid_Y)

Searching for 'eritrea':
  'eritrea' > 'ecuador' → skip page5
  'eritrea' <= 'ethiopia' → follow page8
  page12 → never visited
```

Note the comparison is on the **full key `(value, rowid)` pair**, not just the
value. Two entries with the same country are ordered by rowid. When searching
for a value (not a specific rowid), compare only the text part at interior nodes.

### 5.3 The descent algorithm

```
descend(indexRoot, searchVal):
  pageNum = indexRoot
  loop:
    page = readPageN(pageNum)

    if page[0] == 0x0A:        // reached a leaf — stop descending
      return page

    if page[0] == 0x02:        // interior — pick the right child
      nextPage = rightmost_child(page)    // default: go right
      for each cell in page (left to right):
        keyVal = read text from cell's record
        if searchVal <= keyVal:
          nextPage = cell's left_child
          break
      pageNum = nextPage
```

This descends one level per iteration. For `N` rows, tree height is `log_b(N)`
where `b` is the branching factor (typically 100–500 keys per interior page).
A 1 GB database with 10 million rows → tree height ≈ 3–4 levels → 3–4 page reads
to reach the first matching leaf.

### 5.4 What if there are many 'eritrea' — B-tree BFS vs B-tree descent vs B+ tree

Suppose `'eritrea'` has 300 matching companies, spread across 3 consecutive
leaf pages. This is the scenario that exposes the fundamental design difference
between the three approaches.

The root interior page has cells that are **dividing keys** — each key is the
**maximum value stored in that left child's entire subtree**:

```
root interior page:
  cell 0: [child→interior_A | key=('france', rowid=X)]    ← max of interior_A subtree
  cell 1: [child→interior_B | key=('mozambique', rowid=Y)] ← max of interior_B subtree
  rightmost: interior_C

  interior_A subtree: everything <= 'france'    → covers 'a'–'f'
  interior_B subtree: 'france' < val <= 'mozambique' → covers 'g'–'m'
  interior_C subtree: everything > 'mozambique' → covers 'n'–'z'

  Note: key=('france', rowid=X) is stored IN this interior cell — not in any leaf.
  It is the actual data record for that entry (SQLite B-tree stores records at
  interior nodes, unlike B+ tree which only stores routing copies).
```

Zooming into interior_A's subtree (since 'eritrea' < 'france'):

```
interior_A:
  cell 0: [child→interior_6 | key=('denmark', rowid=A)]   ← max of interior_6
  cell 1: [child→interior_7 | key=('france',  rowid=B)]   ← max of interior_7
  rightmost: interior_8

interior_7 (covers 'egypt'–'france' range):
  cell 0: [child→leaf40 | key=('egypt',   rowid=50000)]   ← max of leaf40
  cell 1: [child→leaf41 | key=('eritrea', rowid=100)]     ← max of leaf41
  cell 2: [child→leaf42 | key=('eritrea', rowid=200)]     ← max of leaf42
  cell 3: [child→leaf43 | key=('eritrea', rowid=300)]     ← max of leaf43
  rightmost: leaf44   (starts with 'estonia')
```

#### 5.4.1 Approach 1: B-tree full index scan (simple BFS)

BFS visits every index page — leaves AND interior nodes — regardless of value.
It also checks **interior node cells** for matches, because in SQLite's B-tree
the dividing keys (like `('france', rowid=X)`) are stored in interior cells,
not in leaf pages.

```
Queue: [root]

Pop root (interior 0x02):
  scan root's own cells for 'eritrea' → no match (keys are 'france', 'mozambique')
  enqueue all children → [interior_A, interior_B, interior_C]

Pop interior_A (interior 0x02):
  scan interior_A's own cells for 'eritrea' → no match ('denmark', 'france')
  enqueue children → [interior_6, interior_7, interior_8]

Pop interior_B → scan cells → no 'eritrea' → enqueue its children
Pop interior_C → scan cells → no 'eritrea' → enqueue its children

Pop interior_6 → scan cells → no 'eritrea' → enqueue its leaf children
Pop interior_7 (interior 0x02):
  scan interior_7's own cells → key=('eritrea', rowid=100) ← 1 match ✅ (stored here!)
  enqueue children → [leaf40, leaf41, leaf42, leaf43, leaf44]
Pop interior_8 → scan cells → no 'eritrea' → enqueue children

Pop leaf40 → scan cells → no 'eritrea' (only 'egypt') → discard
Pop leaf41 → scan cells → 100 matches ✅  (eritrea rowid 1–99, eritrea rowid=100 is in interior_7)
Pop leaf42 → scan cells → 100 matches ✅
Pop leaf43 → scan cells → 100 matches ✅
Pop leaf44 → scan cells → no 'eritrea' (only 'estonia') → discard
... (all remaining pages)
```

**Result:** finds all 301 matches (300 in leaves + 1 in interior_7) ✅
**Cost:** reads ALL 244 index pages — 'angola', 'brazil', 'china'... pages too
**Sibling problem:** doesn't exist — BFS naturally visits leaf 41, 42, 43 in queue order
**Why it works:** BFS doesn't care about sorted order, just exhausts everything

#### 5.4.2 Approach 2: B-tree true descent (the sibling problem)

Descent follows the sorted structure to land directly on leaf 41 in O(log N).

```
root → interior_X → interior_7 → leaf 41   (3–4 page reads)
  scan leaf 41 → 100 matches ✅
```

Now: **how to get to leaf 42?**

Leaf 41's 8-byte header:

```
byte 0:    0x0A  (index leaf)
bytes 1-2: first freeblock
bytes 3-4: cell count
bytes 5-6: cell content start
byte 7:    fragmented free bytes
           ← NO pointer to leaf 42 here. Dead end.
```

The only entity that knows leaf 42's page number is **interior_7** (the parent).
But you already descended past interior_7 and have no reference to it anymore.

**Three escape options:**

```
Option A — Re-descend from root:
  Take last seen key ('eritrea', rowid=100)
  Re-descend looking for first entry > that key
  → lands on leaf 42    (another O(log N) reads)
  Take last seen key ('eritrea', rowid=200)
  Re-descend → lands on leaf 43
  Total: 3 × O(log N) = O(K log N)

Option B — Stack-based: save the path during descent
  descent stack: [{root, child=1}, {interior_7, child=0}]
  finish leaf 41 → pop → back at interior_7, advance childIndex to 1
    → follow child → descend left-most → leaf 42    (O(1) extra)
  finish leaf 42 → pop → back at interior_7, advance childIndex to 2
    → follow child → leaf 43
  Total: O(log N) initial + O(1) per sibling = O(log N + K)

Option C — Stop (only works when results fit on one page):
  'eritrea' has 300 results → they DON'T fit on one leaf → misses 200 results ❌
```

**Result:** finds all 300 matches only with Option A or B ✅ (Option C misses)
**Cost:** O(K log N) or O(log N + K) depending on approach
**Sibling problem:** the core issue — must go back UP to interior_7 to get leaf 42

#### 5.4.3 Approach 3: B+ tree true descent (no sibling problem)

B+ trees (PostgreSQL, MySQL InnoDB) store a `next_page` pointer in every leaf:

```
Leaf 41 header:
  byte 0:    page type
  ...
  bytes 4-7: next_sibling = 42   ← SQLite doesn't have this

Leaf 42 header:
  bytes 4-7: next_sibling = 43

Leaf 43 header:
  bytes 4-7: next_sibling = 44   ← 'estonia', stop here
```

Descent + forward scan:

```
root → interior → leaf 41    (O(log N))
  scan leaf 41 → 100 matches ✅ → follow next=42
  scan leaf 42 → 100 matches ✅ → follow next=43
  scan leaf 43 → 100 matches ✅ → follow next=44
  scan leaf 44 → first entry 'estonia' > 'eritrea' → stop ✅
```

**Result:** finds all 300 matches ✅
**Cost:** O(log N) descent + O(K) forward scan = O(log N + K) — optimal
**Sibling problem:** does not exist — leaf 41 directly points to leaf 42
**Why SQLite doesn't do this:** B+ tree sibling pointers add write overhead
(every leaf insert/split must update neighbour pointers). SQLite optimises for
simplicity and write performance over range scan performance.

#### 5.4.4 Head-to-head comparison

```
Setup: 300 'eritrea' rows on 3 leaf pages, index tree height = 4 levels
       4096-byte pages, ~100 index entries per leaf
```

|                           | B-tree BFS (full scan) | B-tree descent                | B+ tree descent        |
| ------------------------- | ---------------------- | ----------------------------- | ---------------------- |
| Pages to find first match | 244 (all)              | 4 (descent)                   | 4 (descent)            |
| Pages to get leaf 42      | already visited        | +4 (re-descend) or +1 (stack) | +1 (next ptr)          |
| Pages to get leaf 43      | already visited        | +4 or +1                      | +1                     |
| **Total page reads**      | **244**                | **12 or 6**                   | **6**                  |
| Misses any 'eritrea'?     | ❌ never               | ❌ never (A or B)             | ❌ never               |
| Implementation            | simple                 | medium (stack)                | simple (follow ptr)    |
| SQLite uses?              | ✅ yes (what we impl)  | ✅ possible                   | ❌ no (B-tree, not B+) |

**Bottom line:**

- BFS: dumb but always correct. Reads pages you don't need.
- B-tree descent: smart routing, but sibling navigation costs extra.
- B+ tree descent: both smart AND cheap sibling access — but SQLite isn't a B+ tree.

For this stage: **full index scan (BFS)** is the right choice. Eritrea only has
4 results in the test database — they fit on one leaf. Even for the 1 GB
production database, the index is only ~150 MB, and BFS over 150 MB is fast
enough under 3 seconds.

### 5.5 Go implementation

```go
// searchIndexDescent finds matching rowids using O(log N) B-tree descent.
// Returns rowids for all index entries where indexed value == searchVal.
func searchIndexDescent(f *os.File, pageSize, indexRoot int, searchVal string) ([]int64, error) {
    // Phase 1: descend to the first leaf that may contain searchVal
    pageNum := indexRoot
    for {
        page, err := readPageN(f, pageSize, pageNum)
        if err != nil {
            return nil, err
        }
        if page[0] == 0x0A { // landed on a leaf
            // Phase 2: collect all matches on this leaf (and continue to next
            // leaves while values still match — simplified: just this leaf)
            return collectMatchesOnLeaf(page, searchVal), nil
        }
        if page[0] != 0x02 {
            return nil, fmt.Errorf("unexpected index page type 0x%02x", page[0])
        }

        // Interior page: find the right child to follow
        cellCount := int(binary.BigEndian.Uint16(page[3:5]))
        nextPage  := int(binary.BigEndian.Uint32(page[8:12])) // default: rightmost
        for i := range cellCount {
            ptrOffset  := 12 + i*2
            cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
            // Interior index cell: [uint32 left_child][varint payload_size][record]
            leftChild  := int(binary.BigEndian.Uint32(page[cellOffset : cellOffset+4]))
            keyVal, _  := readIndexCell(page, cellOffset+4) // skip left_child uint32
            if strings.Compare(strings.ToLower(searchVal), strings.ToLower(keyVal)) <= 0 {
                nextPage = leftChild
                break
            }
        }
        pageNum = nextPage
    }
}

// collectMatchesOnLeaf scans one index leaf page and returns rowids where
// the indexed value equals searchVal (case-insensitive).
func collectMatchesOnLeaf(page []byte, searchVal string) []int64 {
    var rowids []int64
    cellCount := int(binary.BigEndian.Uint16(page[3:5]))
    for i := range cellCount {
        ptrOffset  := 8 + i*2
        cellOffset := int(binary.BigEndian.Uint16(page[ptrOffset : ptrOffset+2]))
        val, rowid := readIndexCell(page, cellOffset)
        if strings.EqualFold(val, searchVal) {
            rowids = append(rowids, rowid)
        }
    }
    return rowids
}
```

Note: `readIndexCell` in section 4.5 reads cells from index **leaf** pages
(`0x0A`). Interior index page (`0x02`) cells have the same structure but are
**prefixed with a 4-byte left_child uint32** before the payload — that's why
the call above passes `cellOffset+4`.

### 5.6 Comparison: full index scan vs true descent

|                                            | Full index scan (section 4.5) | True descent (section 5.5)        |
| ------------------------------------------ | ----------------------------- | --------------------------------- |
| Pages read                                 | All index pages               | ~tree height + matched leaf pages |
| For companies.db (244 index pages)         | 244 page reads                | ~4–5 page reads                   |
| For 1 GB production DB (~8000 index pages) | 8000 page reads               | ~5–6 page reads                   |
| Handles multi-page result spans?           | ✅ always                     | ⚠️ needs sibling scan             |
| Implementation complexity                  | simple BFS                    | descent + forward scan            |
| Passes the stage?                          | ✅ yes                        | ✅ yes (faster)                   |

**Verify the tree height of companies.db index:**

```sh
# Count levels by checking rootpage type, then its children, etc.
sqlite3 companies.db "SELECT rootpage FROM sqlite_schema WHERE name='idx_companies_country';"
# e.g. rootpage = 4 → offset 3*4096 = 12288

xxd -s 12288 -l 1 companies.db
# 02 = index interior → at least 2 levels (interior + leaf)

# Check depth more easily:
sqlite3 companies.db "SELECT count(*) FROM dbstat WHERE name='idx_companies_country';"
# 244 leaf+interior pages total

# With branching factor ~200, height = ceil(log_200(N_rows)):
sqlite3 companies.db "SELECT count(*) FROM companies;"
# ~100k rows → height = ceil(log_200(100000)) ≈ 3 levels
```
