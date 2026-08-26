package report

import (
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"
)

// streamStyler applies the script styles while rows are streamed into the
// worksheet. Doing it here keeps the later patch pass limited to the <cols>
// element, so the worksheet XML never has to be re-encoded cell by cell.
// Sheets without a RowsReader have no stream to write into and keep using the
// patch pass for their styles.
type streamStyler struct {
	rules       preparedRules
	dateStyleID int
	lastRow     int
	// values is reused across rows; SetRow writes it out and does not retain it.
	values []any
}

func newStreamStyler(book *excelize.File, styleRules []styleRule) (*streamStyler, error) {
	compiled, err := compileRules(worksheetRules{StyleRules: styleRules})
	if err != nil {
		return nil, err
	}
	rules := prepareRules(compiled)
	dateStyleID, err := book.NewStyle(&excelize.Style{NumFmt: OpenXMLShortDateFmtDateId})
	if err != nil {
		return nil, err
	}
	return &streamStyler{rules: rules, dateStyleID: dateStyleID}, nil
}

// writeRow writes one row with its styles applied. A nil fields slice writes a
// row that only carries styles, as group breaks and rules beyond the data need.
func (s *streamStyler) writeRow(stream *excelize.StreamWriter, row int, fields []any) error {
	if err := s.materializeBefore(stream, row); err != nil {
		return err
	}
	values, opts := s.styleRow(row, fields)
	if err := stream.SetRow("A"+strconv.Itoa(row), values, opts...); err != nil {
		return err
	}
	s.lastRow = row
	return nil
}

// materializeBefore writes the styled rows that precede the given row and that
// the data itself does not produce.
func (s *streamStyler) materializeBefore(stream *excelize.StreamWriter, row int) error {
	for {
		styledRow, ok := s.rules.nextStyledRow(s.lastRow)
		if !ok || styledRow >= row {
			return nil
		}
		if err := s.writeRow(stream, styledRow, nil); err != nil {
			return err
		}
	}
}

// finish writes the styled rows that follow the last data row.
func (s *streamStyler) finish(stream *excelize.StreamWriter) error {
	return s.materializeBefore(stream, excelize.TotalRows+1)
}

// styleRow resolves the style of every cell of one row. Precedence matches the
// patch pass: exact cell and cell range rules win, then the row rule, then the
// column rule, and a date format applies only where no rule does.
func (s *streamStyler) styleRow(row int, fields []any) ([]any, []excelize.RowOpts) {
	cells := s.rules.styledCells(row)
	rowStyle, hasRowStyle := spanStyle(s.rules.Rows, row)

	var opts []excelize.RowOpts
	if hasRowStyle {
		opts = append(opts, excelize.RowOpts{StyleID: rowStyle})
	}
	width := len(fields)
	if len(cells) > 0 {
		width = max(width, cells[len(cells)-1].col)
	}
	if width == 0 {
		return nil, opts
	}

	if cap(s.values) < width {
		s.values = make([]any, width)
	}
	values := s.values[:width]
	next := 0
	for col := range width {
		var field any
		if col < len(fields) {
			field = fields[col]
		}
		style := 0
		if _, ok := field.(time.Time); ok {
			style = s.dateStyleID
		}
		for next < len(cells) && cells[next].col < col+1 {
			next++
		}
		switch {
		case next < len(cells) && cells[next].col == col+1:
			style = cells[next].styleID
		case hasRowStyle:
			style = rowStyle
		default:
			if colStyle, ok := spanStyle(s.rules.Cols, col+1); ok {
				style = colStyle
			}
		}
		if style == 0 {
			values[col] = field
			continue
		}
		values[col] = excelize.Cell{StyleID: style, Value: field}
	}
	return values, opts
}

// columnStyles keeps the rules that the <cols> element carries, which is all
// that is left to patch once cell styles are written during streaming.
func columnStyles(styleRules []styleRule) []styleRule {
	cols := make([]styleRule, 0, len(styleRules))
	for _, rule := range styleRules {
		if styleTargetKind(normalizeTarget(rule.Target)) == styleTargetCol {
			cols = append(cols, rule)
		}
	}
	return cols
}
