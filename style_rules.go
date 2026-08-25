package report

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// This file models the style and width rules of one worksheet: how a script
// target is compiled into numeric ranges, and how those ranges answer what a
// row or a cell needs. Both appliers, the streamed write and the worksheet XML
// patch, share these rules.

// Style IDs are zero-based indexes into xl/styles.xml/cellXfs/xf.
// The IDs must already exist in the workbook's styles.xml.
type styleSpan struct {
	Min, Max int
	StyleID  int
}

type widthSpan struct {
	Min, Max int
	Width    float64
}

type cellSpan struct {
	FromCol, ToCol int
	FromRow, ToRow int
	StyleID        int
}

type styledCell struct {
	col     int
	styleID int
}

// patchRules mirrors the worksheet-level style and width metadata after
// workbook styles have been assigned, but before targets are compiled into
// numeric XML ranges.
type patchRules struct {
	Styles      map[string]int     // target -> workbook style ID
	Widths      map[string]float64 // column target -> explicit width
	FieldWidths []int              // auto-sized content widths by zero-based column
	// SheetDataStyled marks worksheets whose cells were already styled while
	// their rows were streamed, leaving only <cols> to patch.
	SheetDataStyled bool
}

func (r patchRules) empty() bool {
	return len(r.Styles) == 0 && len(r.Widths) == 0 && len(r.FieldWidths) == 0
}

// compiledPatchRules contains the numeric ranges consumed by the worksheet
// XML transformer.
type compiledPatchRules struct {
	// Exact cell rules always win.
	Cells      map[string]int // e.g. "B4": 7
	CellRanges []cellSpan
	Rows       []styleSpan // Excel row numbers, inclusive
	Cols       []styleSpan // Excel column numbers, inclusive; A=1
	Widths     []widthSpan // Excel column widths, inclusive; A=1

	SheetDataStyled bool
}

const (
	styleTargetCol = iota
	styleTargetRow
	styleTargetCell
)

var colRe = regexp.MustCompile(`^[A-Z]+(:[A-Z]+)?$`)

func styleTargetKind(target string) int {
	if colRe.MatchString(target) {
		return styleTargetCol
	}
	if _, err := strconv.Atoi(target); err == nil {
		return styleTargetRow
	}
	return styleTargetCell
}

func normalizeTarget(target string) string {
	return strings.TrimSpace(strings.ToUpper(target))
}

func normalCellRef(ref string) string {
	return strings.ToUpper(strings.ReplaceAll(ref, "$", ""))
}

func boundedCellWidth(width float64) float64 {
	return max(min(width, 100.0), 10.0)
}

// colRange splits a colon-separated column range (e.g. "A:C") into its two
// endpoints. A single column name is returned as both endpoints unchanged.
func colRange(target string) (from, to string) {
	if f, t, ok := strings.Cut(target, ":"); ok {
		return f, t
	}
	return target, target
}

func columnSpan(target string) (from, to int, err error) {
	fromName, toName := colRange(target)
	from, err = excelize.ColumnNameToNumber(fromName)
	if err != nil {
		return 0, 0, err
	}
	to, err = excelize.ColumnNameToNumber(toName)
	if err != nil {
		return 0, 0, err
	}
	if from > to {
		from, to = to, from
	}
	return from, to, nil
}

func compilePatchRules(spec patchRules) (compiledPatchRules, error) {
	rules := compiledPatchRules{
		Cells:           make(map[string]int),
		SheetDataStyled: spec.SheetDataStyled,
	}
	for target, styleID := range spec.Styles {
		target = normalizeTarget(target)
		switch styleTargetKind(target) {
		case styleTargetCol:
			fromCol, toCol, err := columnSpan(target)
			if err != nil {
				return compiledPatchRules{}, err
			}
			rules.Cols = append(rules.Cols, styleSpan{Min: fromCol, Max: toCol, StyleID: styleID})
		case styleTargetRow:
			row, err := strconv.Atoi(target)
			if err != nil || row < 1 || row > excelize.TotalRows {
				return compiledPatchRules{}, fmt.Errorf("invalid style row %q", target)
			}
			rules.Rows = append(rules.Rows, styleSpan{Min: row, Max: row, StyleID: styleID})
		case styleTargetCell:
			from, to := colRange(target)
			fromCol, fromRow, err := excelize.CellNameToCoordinates(from)
			if err != nil {
				return compiledPatchRules{}, err
			}
			toCol, toRow, err := excelize.CellNameToCoordinates(to)
			if err != nil {
				return compiledPatchRules{}, err
			}
			if fromCol > toCol {
				fromCol, toCol = toCol, fromCol
			}
			if fromRow > toRow {
				fromRow, toRow = toRow, fromRow
			}
			if fromCol == toCol && fromRow == toRow {
				rules.Cells[normalCellRef(from)] = styleID
			} else {
				rules.CellRanges = append(rules.CellRanges, cellSpan{
					FromCol: fromCol,
					ToCol:   toCol,
					FromRow: fromRow,
					ToRow:   toRow,
					StyleID: styleID,
				})
			}
		}
	}

	// Auto-sized widths are the baseline; explicit width metadata is appended
	// afterward so it overrides the automatic value.
	for colIdx, width := range spec.FieldWidths {
		if _, err := excelize.ColumnNumberToName(colIdx + 1); err != nil {
			return compiledPatchRules{}, err
		}
		rules.Widths = append(rules.Widths, widthSpan{
			Min:   colIdx + 1,
			Max:   colIdx + 1,
			Width: boundedCellWidth(float64(width) * 1.123),
		})
	}
	for target, width := range spec.Widths {
		target = normalizeTarget(target)
		if !colRe.MatchString(target) {
			continue
		}
		fromCol, toCol, err := columnSpan(target)
		if err != nil {
			return compiledPatchRules{}, err
		}
		rules.Widths = append(rules.Widths, widthSpan{
			Min:   fromCol,
			Max:   toCol,
			Width: boundedCellWidth(width),
		})
	}
	return rules, nil
}

// preparedRules indexes the rules of one worksheet so the streaming pass never
// parses a cell reference or scans the exact-cell rules once per row.
type preparedRules struct {
	compiledPatchRules
	cellRows  map[int][]styledCell // exact cell styles per row, ascending column
	cellRowNo []int                // sorted rows present in cellRows
}

func prepareRules(rules compiledPatchRules) (preparedRules, error) {
	prepared := preparedRules{compiledPatchRules: rules}
	if len(rules.Cells) == 0 {
		return prepared, nil
	}
	prepared.cellRows = make(map[int][]styledCell, len(rules.Cells))
	for ref, styleID := range rules.Cells {
		col, row, err := excelize.CellNameToCoordinates(normalCellRef(ref))
		if err != nil {
			return preparedRules{}, fmt.Errorf("invalid cell reference %q: %w", ref, err)
		}
		prepared.cellRows[row] = append(prepared.cellRows[row], styledCell{col: col, styleID: styleID})
	}
	for row, cells := range prepared.cellRows {
		slices.SortFunc(cells, func(a, b styledCell) int { return a.col - b.col })
		prepared.cellRowNo = append(prepared.cellRowNo, row)
	}
	sort.Ints(prepared.cellRowNo)
	return prepared, nil
}

// sheetDataUntouched reports whether the rules change nothing inside
// <sheetData>. Column widths and column styles live in <cols>, which precedes
// <sheetData>, so such worksheets can be copied verbatim from that point on.
// Column styles also fall back to <cols> alone once the cells carry their
// styles from the streamed write.
func (p preparedRules) sheetDataUntouched() bool {
	if len(p.Rows) != 0 || len(p.Cells) != 0 || len(p.CellRanges) != 0 {
		return false
	}
	return p.SheetDataStyled || len(p.Cols) == 0
}

// nextStyledRow finds the first row after the given one that must be
// materialized because a row or cell style targets it.
func (p preparedRules) nextStyledRow(after int) (int, bool) {
	if after >= excelize.TotalRows {
		return 0, false
	}
	next := excelize.TotalRows + 1
	consider := func(row int) {
		if row > after && row < next && row <= excelize.TotalRows {
			next = row
		}
	}
	for _, span := range p.Rows {
		row := max(span.Min, after+1)
		if row <= span.Max {
			consider(row)
		}
	}
	for _, span := range p.CellRanges {
		row := max(span.FromRow, after+1)
		if row <= span.ToRow {
			consider(row)
		}
	}
	if i := sort.SearchInts(p.cellRowNo, after+1); i < len(p.cellRowNo) {
		consider(p.cellRowNo[i])
	}
	if next > excelize.TotalRows {
		return 0, false
	}
	return next, true
}

// styledCells lists the styles a row needs from exact cell and cell-range
// rules, in ascending column order. It is the single place that resolves their
// precedence: exact cells win over ranges, and later ranges win over earlier
// ones.
func (p preparedRules) styledCells(row int) []styledCell {
	exact := p.cellRows[row]
	ranged := false
	for _, rule := range p.CellRanges {
		if rule.FromRow <= row && row <= rule.ToRow {
			ranged = true
			break
		}
	}
	if !ranged {
		return exact
	}

	styles := make(map[int]int)
	for _, rule := range p.CellRanges {
		if rule.FromRow > row || row > rule.ToRow {
			continue
		}
		for col := rule.FromCol; col <= rule.ToCol; col++ {
			styles[col] = rule.StyleID
		}
	}
	for _, cell := range exact {
		styles[cell.col] = cell.styleID
	}
	cells := make([]styledCell, 0, len(styles))
	for _, col := range slices.Sorted(maps.Keys(styles)) {
		cells = append(cells, styledCell{col: col, styleID: styles[col]})
	}
	return cells
}

// rowOrColStyle is the fallback for cells that no exact cell or cell-range
// rule targets. Row rules win over column rules, as in Excel.
func rowOrColStyle(rules compiledPatchRules, row, col int) (int, bool) {
	if id, ok := spanStyle(rules.Rows, row); ok {
		return id, true
	}
	return spanStyle(rules.Cols, col)
}

func spanStyle(spans []styleSpan, value int) (int, bool) {
	for _, span := range slices.Backward(spans) { // last rule wins
		if span.Min <= value && value <= span.Max {
			return span.StyleID, true
		}
	}
	return 0, false
}

func spanWidth(spans []widthSpan, value int) (float64, bool) {
	for _, span := range slices.Backward(spans) {
		if span.Min <= value && value <= span.Max {
			return span.Width, true
		}
	}
	return 0, false
}
