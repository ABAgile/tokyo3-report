# report

[![Release](https://img.shields.io/github/v/release/abagile/tokyo3-report?sort=semver&logo=Go&color=%23007D9C)](https://github.com/abagile/tokyo3-report/releases)
[![Test](https://github.com/abagile/tokyo3-report/actions/workflows/test.yml/badge.svg)](https://github.com/abagile/tokyo3-report/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/abagile/tokyo3-report.svg)](https://pkg.go.dev/github.com/abagile/tokyo3-report)
[![Go Report Card](https://goreportcard.com/badge/github.com/abagile/tokyo3-report)](https://goreportcard.com/report/github.com/abagile/tokyo3-report)
[![codecov](https://codecov.io/gh/abagile/tokyo3-report/branch/main/graph/badge.svg)](https://codecov.io/gh/abagile/tokyo3-report)

Excel report generator for Go. Accepts any tabular data source, auto-sizes columns, and supports per-report customisation via embedded [Starlark](https://github.com/google/starlark-go) scripts.

**Requires Go 1.26+**

```
go get github.com/abagile/tokyo3-report
```

---

## Design

```
ReportConf ──► Generate(conf, rowReader) ──► .xlsx file
                    │
                    ├── RowsReader   (data source: StructRows or SqlxRows)
                    ├── Script       (optional Starlark, controls layout & style)
                    ├── Parsers      (optional named field-value parsers)
                    └── Translator   (optional, localises headers and enum values)
```

`Generate` either creates a new workbook or opens an existing one, replaces the named sheet with fresh data, applies script-defined layout, then saves. If the target file does not exist it is created from scratch; if it exists but contains other sheets those sheets are left untouched.

---

## Quick start

```go
type Order struct {
    Region  string
    Product string
    Amount  float64
}

rows := []any{
    Order{"North", "Widget A", 1200.00},
    Order{"North", "Widget B",  450.00},
    Order{"South", "Widget A",  980.00},
}

err := report.Generate(
    &report.ReportConf{
        OutputPath: "orders.xlsx",
        SheetName:  "Orders",
    },
    report.NewStructRows(rows),
)
```

Produces a sheet with auto-titled headers (`Region`, `Product`, `Amount`) and auto-sized columns.

See the [`example/`](example/) directory for a complete runnable program.

---

## ReportConf

| Field        | Type         | Description |
|--------------|--------------|-------------|
| `OutputPath` | `string`     | Path to the `.xlsx` file to create or update. |
| `SheetName`  | `string`     | Name of the worksheet to write. |
| `Script`     | `string`     | Starlark script for layout customisation (see below). |
| `Translator` | `Translator` | Optional interface for header and value localisation. |
| `Parsers`    | `map[string]Parser` | Optional named parsers referenced by column metadata. |
| `QueryPath`  | `string`     | Informational — callers may store the SQL file path here; the library does not read it. |

---

## RowsReader

`RowsReader` is the interface that decouples `Generate` from any particular data source.

```go
type RowsReader interface {
    Read()            error
    Headers()         ([]string, error)
    Values()          ([]any, error)
    Next()            bool
    Err()             error
}
```

Two implementations are provided:

### StructRows

Wraps an in-memory `[]any` of structs. Struct field names become column headers.

```go
reader := report.NewStructRows([]any{
    MyStruct{...},
    MyStruct{...},
})
```

Pointer fields are dereferenced; nil pointers render as an empty string.

### SqlxRows

Streams directly from a live `*sqlx.DB` query.

```go
reader := report.NewSqlxRows("SELECT region, product, amount FROM orders", db)
```

`NUMERIC` database columns are automatically converted to `float64`.

---

## Translator

Implement `Translator` to localise header names and enum-coded values.

```go
type Translator interface {
    T(key string) string                 // header: raw field/column name → display name
    Label(key string, code any) string   // value:  lookup key + raw code  → display label
}
```

`T` is called once per header with the raw field or SQL column name.
`Label` is called per cell for columns that set `lookup` in the script (see below).

```go
type MyTranslator struct{}

func (MyTranslator) T(key string) string {
    return map[string]string{
        "region":  "Region",
        "product": "Product Name",
        "amount":  "Amount (USD)",
    }[key]
}

func (MyTranslator) Label(key string, code any) string {
    if key == "status" {
        return map[any]string{1: "Active", 2: "Closed"}[code]
    }
    return fmt.Sprintf("%v", code)
}
```

When no `Translator` is set, headers are produced by `flect.Titleize` (`order_id` → `Order Id`).

---

## Parsers

A `Parser` converts a raw field value before it is written to Excel:

```go
type Parser func(any) (any, error)
```

Register parsers by name in `ReportConf`, then reference those names from the
script's `col` metadata:

```go
conf.Parsers = map[string]report.Parser{
    "trim": func(value any) (any, error) {
        return strings.TrimSpace(value.(string)), nil
    },
}
```

An unknown parser name or a parser error stops report generation. When a column
specifies both `parser` and `lookup`, the parser runs first and the lookup
receives the parsed value.

---

## Starlark script

The `Script` field accepts a [Starlark](https://github.com/google/starlark-go) program. Starlark is a deterministic, sandboxed Python dialect — no I/O, no imports, no side-effects outside the declared variables below.

The script may define any combination of the four top-level variables. Unrecognised variables are silently ignored.

### `groupFields`

```python
groupFields = ["Region", "Category"]
```

A list of raw column names. Whenever the value in any listed column changes compared to the previous row, a blank separator row is inserted above the new group. Multiple fields are checked in order.

### `width`

```python
width = {
    "A":   14,
    "B:D": 22,
}
```

Explicit column widths. Keys are column letters or letter ranges (`"B:D"`). Values are numbers (integer or float). Overrides auto-sizing for those columns. All other columns remain auto-sized.

Column widths are clamped to the range **[10, 100]**.

### `style`

```python
style = {
    "1":       '{"font":{"bold":true}}',    # entire row 1 (header)
    "A":       '{"font":{"color":"FF0000"}}', # entire column A
    "A:C":     '{"fill":{"type":"pattern","color":["FFFFCC"],"pattern":1}}', # columns A–C
    "B2":      '{"font":{"italic":true}}',  # single cell
    "B2:D10":  '{"font":{"italic":true}}',  # cell range
}
```

Each key selects a target; each value is the JSON serialisation of an [excelize Style](https://pkg.go.dev/github.com/xuri/excelize/v2#Style). Target type is inferred from the key format:

| Key pattern | Example | Applied with |
|---|---|---|
| Column letter(s) | `"A"`, `"A:C"` | `SetColStyle` |
| Integer | `"1"`, `"3"` | `SetRowStyle` |
| Cell reference | `"B2"` | `SetCellStyle` |
| Cell range | `"B2:D10"` | `SetCellStyle` |

### `col`

Per-column settings keyed by the **raw** field/column name (before any `Translator.T` rename).

```python
col = {
    "amount": {
        "width": 18,
        "style": '{"num_fmt":4}',        # #,##0.00
    },
    "status": {
        "parser": "normalize_status",    # resolves ReportConf.Parsers entry
        "lookup": "order_status",        # receives the parsed value
    },
}
```

| Sub-key  | Type   | Description |
|----------|--------|-------------|
| `width`  | number | Column width (same semantics as `width` dict above). |
| `style`  | string | JSON style applied to the entire column. |
| `parser` | string | Runs the named `ReportConf.Parsers` entry. Unknown names and parser errors stop generation. |
| `lookup` | string | Passes the parsed (or raw) value through `Translator.Label(key, value)`. No-op when no `Translator` is set. |

`col` style and width entries are merged into the `style` and `width` dicts after the script runs, so they can coexist with direct `style`/`width` entries.

### Style JSON reference

Style values are JSON representations of [`excelize.Style`](https://pkg.go.dev/github.com/xuri/excelize/v2#Style). Common fields:

```jsonc
{
  "font":    { "bold": true, "italic": true, "size": 11, "color": "FF0000" },
  "fill":    { "type": "pattern", "pattern": 1, "color": ["FFFFCC"] },
  "alignment": { "horizontal": "center", "wrap_text": true },
  "num_fmt": 4    // built-in number format ID (4 = #,##0.00)
}
```

Common `num_fmt` IDs: `3` = `#,##0`, `4` = `#,##0.00`, `9` = `0%`, `10` = `0.00%`.

---

## Auto column sizing

Column widths are calculated automatically from content width, where ASCII characters count as 1 unit and CJK/multibyte characters count as 2 units, multiplied by a 1.123 scale factor. The result is clamped to **[10, 100]**. Script-defined widths always override auto-sizing.

`time.Time` values receive Excel's built-in short-date format (format ID 14, e.g. `01/15/24`) to prevent Excel from auto-converting dates to `MMM-YY` display format on open.
