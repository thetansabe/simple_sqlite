# Bit Manipulation in `readVarint()`

## What problem does varint solve?

If you stored every integer as a fixed 8-byte int64, a value like `2` would
waste 7 bytes. SQLite databases have millions of integers (row IDs, offsets,
lengths), so this adds up fast.

Variable-length integers (varints) shrink small numbers to 1–2 bytes while
still being able to represent huge numbers when needed.

---

## How SQLite varint works — one rule only

Every byte is split like this:

```
byte = [FLAG | D D D D D D D]
        bit7   bits 6..0 (7 data bits)

FLAG = 1  →  more bytes coming, keep reading
FLAG = 0  →  this is the last byte, stop
```

You collect 7 data bits from each byte and glue them together until FLAG = 0.

---

## Why the loop is `i < 9` (not 8 or 10)

From the **official SQLite file format docs** ([sqlite.org/fileformat.html](https://www.sqlite.org/fileformat.html)):

> *"The lower seven bits of each of the first **eight bytes** and all **8 bits
> of the ninth byte** are used to reconstruct the 64-bit twos-complement integer."*

So the math is fixed:

```
bytes 1–8:  8 × 7 bits = 56 bits
byte 9:     1 × 8 bits =  8 bits  (no FLAG, all data)
                         ────────
total:                    64 bits  = exactly int64
```

9 bytes is the **minimum** needed to cover all possible int64 values. Not 8
(8×7=56, not enough), not 10 (unnecessary). The 9th byte also has no FLAG bit
because there is no 10th byte — it just uses all 8 bits directly.

---

## Concrete example 1 — single byte (value = 42)

42 in binary = `010 1010` → fits in 7 bits → only 1 byte needed

```
byte[0] = 0_010 1010
          ↑ FLAG=0 (last byte)
            ↑↑↑↑↑↑↑ data = 42
```

Loop i=0:
```
b        = 0010_1010

b & 0x7f = 0010_1010  (0x7f = 0111_1111, strips the flag bit)
         = 42

result   = (0 << 7) | 42
         = 0 | 42
         = 42

b & 0x80 = 0000_0000  (0x80 = 1000_0000, isolates flag bit)
         = 0  → FLAG=0 → STOP
```

Returns `(42, 1)` — value=42, consumed 1 byte. ✓

---

## Concrete example 2 — two bytes (value = 1000)

1000 in binary = `000 0111 110 1000` → needs 10 bits → split into two 7-bit chunks:

```
1000 = 000_0111  110_1000
       chunk A   chunk B
       (high)    (low)
```

Encode: chunk A gets FLAG=1 (more follows), chunk B gets FLAG=0 (last):
```
byte[0] = 1_000 0111  =  0x87   ← chunk A, FLAG=1
byte[1] = 0_110 1000  =  0x68   ← chunk B, FLAG=0
```

**Step by step through the loop:**

```
result starts = 0

━━━━━━━━━━━━━━━━━━━━━━━━━━━━
i=0: b = 0x87 = 1000_0111
━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Goal: strip the flag, grab the 7 data bits, glue them onto result

  b & 0x7f:
    1000_0111   ← b
  & 0111_1111   ← 0x7f (mask: zeroes out bit7, keeps bits 6..0)
  -----------
    0000_0111   = 7   ← chunk A's data bits

  result << 7:
    0 << 7 = 0          ← make room for 7 new bits (nothing yet)

  result = 0 | 7 = 7    ← glued chunk A in

  b & 0x80:
    1000_0111
  & 1000_0000   ← 0x80 (mask: only keeps bit7)
  -----------
    1000_0000   ≠ 0  → FLAG=1 → keep reading

━━━━━━━━━━━━━━━━━━━━━━━━━━━━
i=1: b = 0x68 = 0110_1000
━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  b & 0x7f:
    0110_1000
  & 0111_1111
  -----------
    0110_1000   = 104   ← chunk B's data bits

  result << 7:
    7 << 7 = 896
    in bits: 0000_0111  →  0011_1000_0000
                           ↑↑↑↑↑↑↑         7 blank slots on the right

  result = 896 | 104 = 1000  ✓

  b & 0x80:
    0110_1000
  & 1000_0000
  -----------
    0000_0000   = 0  → FLAG=0 → STOP
```

Returns `(1000, 2)` — value=1000, consumed 2 bytes. ✓

---

## What each operator actually does

| Operator | What it does | Why |
|----------|-------------|-----|
| `b & 0x7f` | Zeroes out bit 7 (the FLAG), keeps bits 6..0 | Extract the 7 data bits |
| `b & 0x80` | Zeroes out bits 6..0, keeps bit 7 (the FLAG) | Check if more bytes follow |
| `result << 7` | Slide existing bits left by 7 | Make 7 empty slots for next chunk |
| `result \| chunk` | Write chunk into those empty slots | Glue the chunks together |

`<<` makes the gap. `|` fills it. Every iteration appends 7 more bits.

---

## 9th byte edge case

After 8 bytes you've collected 56 bits. The 9th byte has no FLAG bit — all 8 bits are data (because 56 + 8 = 64 = int64 max). So instead of `<< 7` you do `<< 8`:

```go
result = (result << 8) | int64(b)
//                ↑ make room for 8 bits, not 7
```

---

## How is this different from LEB128 (used in Kafka / Protobuf)?

Both use the same FLAG bit trick (`bit7=1` → more bytes, `bit7=0` → last byte),
but differ in **byte order** and **how signed integers work**.

See your Kafka notes for a full LEB128 walkthrough:
`/Users/ntdien/workplace/codecrafters-kafka-go/questions/leb123.md`

### 1. Byte order — LSB first vs MSB first

Same value `1024`, encoded both ways:

```
1024 in binary = 100_0000000  (10 bits)

Split into two 7-bit chunks:
  chunk A = 000_1000  (high bits)
  chunk B = 000_0000  (low bits)
```

**LEB128 — low chunk first:**
```
byte[0] = chunk B + FLAG=1  →  1_000_0000  =  0x80   ← small bits first
byte[1] = chunk A + FLAG=0  →  0_000_1000  =  0x08   ← big bits second

on disk:  0x80  0x08

decode:
  i=0: result |= 0x00 << 0  =  0
  i=1: result |= 0x08 << 7  =  1024  ✓
```

**SQLite varint — high chunk first:**
```
byte[0] = chunk A + FLAG=1  →  1_000_1000  =  0x88   ← big bits first
byte[1] = chunk B + FLAG=0  →  0_000_0000  =  0x00   ← small bits second

on disk:  0x88  0x00

decode:
  i=0: result = (0 << 7) | 8  =  8
  i=1: result = (8 << 7) | 0  =  1024  ✓
```

Side by side — same two bytes, swapped order:
```
Value 1024    byte[0]    byte[1]
─────────────────────────────────
LEB128         0x80       0x08    ← small bits first, big bits second
SQLite         0x88       0x00    ← big bits first,   small bits second
```

| Format | Byte order | Decode pattern |
|--------|-----------|----------------|
| **LEB128** | LSB first (little-endian) | `result \|= chunk << (7*i)` |
| **SQLite varint** | MSB first (big-endian) | `result = (result << 7) \| chunk` |

SQLite is big-endian throughout its file format — varints follow the same convention.

### 2. Signed integers

| Format | How |
|--------|-----|
| **LEB128 unsigned** | raw bits |
| **LEB128 signed** | zigzag encoding: `(n << 1) ^ (n >> 31)` |
| **SQLite varint** | raw int64 bit pattern |

Kafka uses zigzag so that `-1` encodes to `1`, `-2` to `3` (small absolute values
stay small). SQLite varints store the raw two's complement, so negative numbers
always use all 9 bytes.

### 3. Max size

Both cap at 9 bytes:
- LEB128:        9 × 7 = 63 bits + sign bit = 64-bit signed
- SQLite varint: 8 × 7 + 8 = 64 bits exactly

---

## Summary cheat sheet

```
SQLite varint byte:  [FLAG | 7 data bits]   (MSB first, 9 bytes max)
LEB128 byte:         [FLAG | 7 data bits]   (LSB first, 9 bytes max)

Both use:  FLAG=1 → more bytes,  FLAG=0 → last byte

Decode SQLite:  result = (result << 7) | (b & 0x7f)
Decode LEB128:  result = result | ((b & 0x7f) << (7 * i))
```
