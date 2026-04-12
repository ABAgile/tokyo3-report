package report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	"github.com/lithammer/shortuuid/v4"
	"github.com/xuri/excelize/v2"
)

const (
	DefaultSheetName          = "Sheet1"
	OpenXMLShortDateFmtDateId = 14
)

const (
	styleTargetCol = iota
	styleTargetRow
	styleTargetCell
)

var colRe = regexp.MustCompile(`^[A-Z]+(:[A-Z]+)?$`)

// Translator supplies display strings for header names and enum-mapped values.
// A nil Translator is valid; headers fall back to flect.Titleize and values pass through unchanged.
type Translator interface {
	T(key string) string
	Label(key string, code any) string
}

type ReportConf struct {
	OutputPath,
	Script,
	SheetName string
	Translator Translator
}

type excelReport struct {
	isNew bool
	excel *excelize.File

	filePath,
	sheetName string

	meta       *scriptMeta
	translator Translator
}

func Generate(conf *ReportConf, rowReader RowsReader) (err error) {
	report := excelReport{
		filePath:   conf.OutputPath,
		sheetName:  conf.SheetName,
		translator: conf.Translator,
	}

	if err = report.openWorkbook(); err != nil {
		return
	}
	defer func() {
		if saveErr := report.excel.SaveAs(report.filePath); err == nil {
			err = saveErr
		}
		if closeErr := report.excel.Close(); err == nil {
			err = closeErr
		}
	}()

	if err = report.runScript(conf.Script); err != nil {
		return
	}

	if err = report.prepareWorksheet(); err != nil {
		return
	}
	if rowReader != nil {
		if err = report.populateSheet(rowReader); err != nil {
			return
		}
	}

	if err = report.processStyleMeta(); err != nil {
		return
	}
	return report.processWidthMeta()
}

func (r *excelReport) openWorkbook() error {
	info, err := os.Stat(r.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		r.isNew = true
	} else {
		r.isNew = info.Size() == 0
	}
	if r.isNew {
		r.excel = excelize.NewFile()
	} else {
		r.excel, err = excelize.OpenFile(r.filePath)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *excelReport) prepareWorksheet() error {
	sheetName := shortuuid.New()
	if _, err := r.excel.NewSheet(sheetName); err != nil {
		return err
	}
	if r.isNew {
		if err := r.excel.DeleteSheet(DefaultSheetName); err != nil {
			return err
		}
	} else if _, err := r.excel.GetSheetIndex(r.sheetName); err == nil {
		if err := r.excel.DeleteSheet(r.sheetName); err != nil {
			return err
		}
	}
	return r.excel.SetSheetName(sheetName, r.sheetName)
}

func (r *excelReport) populateSheet(rowReader RowsReader) error {
	if err := rowReader.Read(); err != nil {
		return err
	}
	if rowReader.Err() == sql.ErrNoRows {
		return nil
	}

	headerIndices, fieldWidths, err := r.writeHeaderRow(rowReader)
	if err != nil {
		return err
	}
	if err := r.processColumnMeta(headerIndices); err != nil {
		return err
	}
	if err := r.writeDataRows(rowReader, headerIndices, fieldWidths); err != nil {
		return err
	}
	return r.applyColumnWidths(fieldWidths)
}

func (r *excelReport) writeHeaderRow(rowReader RowsReader) (map[string]int, []int, error) {
	headers, err := rowReader.Headers()
	if err != nil {
		return nil, nil, err
	}

	headerIndices := make(map[string]int)
	fieldWidths := make([]int, len(headers))
	for i, hdr := range headers {
		if r.translator != nil {
			headers[i] = r.translator.T(hdr)
		} else {
			headers[i] = flect.Titleize(hdr)
		}
		headerIndices[hdr] = i
		if w := calcCellWidth(headers[i]); w > fieldWidths[i] {
			fieldWidths[i] = w
		}
	}

	if err := r.excel.SetSheetRow(r.sheetName, "A1", &headers); err != nil {
		return nil, nil, err
	}
	return headerIndices, fieldWidths, nil
}

func (r *excelReport) writeDataRows(rowReader RowsReader, headerIndices map[string]int, fieldWidths []int) error {
	var lastRow []any
	for i := 2; rowReader.Next(); i++ {
		fields, err := rowReader.Values()
		if err != nil {
			return err
		}

		for i, fn := range r.meta.parser {
			if result, err := fn(fields[i]); err == nil {
				fields[i] = result
			}
		}

		if lastRow != nil {
			if i, err = r.insertGroupBreaks(i, fields, lastRow, headerIndices); err != nil {
				return err
			}
		}
		lastRow = fields

		cell := fmt.Sprintf("A%d", i)
		if err := r.excel.SetSheetRow(r.sheetName, cell, &fields); err != nil {
			return err
		}

		if err := r.applyDateStyles(fields, i); err != nil {
			return err
		}
		trackFieldWidths(fields, fieldWidths)
	}
	return nil
}

func (r *excelReport) insertGroupBreaks(rowIdx int, fields, lastRow []any, headerIndices map[string]int) (int, error) {
	for _, field := range r.meta.groupFields {
		if colIdx, ok := headerIndices[field]; ok {
			if fields[colIdx] != lastRow[colIdx] {
				cell := fmt.Sprintf("A%d", rowIdx)
				if err := r.excel.SetSheetRow(r.sheetName, cell, &[]any{}); err != nil {
					return rowIdx, err
				}
				rowIdx++
			}
		}
	}
	return rowIdx, nil
}

// applyDateStyles sets the date number format on cells whose value is a time.Time,
// preventing Excel from auto-converting them to MMM-YY on open.
func (r *excelReport) applyDateStyles(fields []any, rowIdx int) error {
	dateStyleID, err := r.excel.NewStyle(&excelize.Style{NumFmt: OpenXMLShortDateFmtDateId})
	if err != nil {
		return err
	}
	for col, field := range fields {
		if _, ok := field.(time.Time); !ok {
			continue
		}
		cellName, err := excelize.CoordinatesToCellName(col+1, rowIdx)
		if err != nil {
			return err
		}
		if err := r.excel.SetCellStyle(r.sheetName, cellName, cellName, dateStyleID); err != nil {
			return err
		}
	}
	return nil
}

func trackFieldWidths(fields []any, fieldWidths []int) {
	for col, width := range fieldWidths {
		var str string
		if t, ok := fields[col].(time.Time); ok {
			str = t.Format(time.RFC3339)
		} else {
			str = fmt.Sprintf("%v", fields[col])
		}
		if w := calcCellWidth(str); w > width {
			fieldWidths[col] = w
		}
	}
}

func (r *excelReport) applyColumnWidths(fieldWidths []int) error {
	for colIdx, width := range fieldWidths {
		col, err := excelize.ColumnNumberToName(colIdx + 1)
		if err != nil {
			return err
		}
		if err := r.excel.SetColWidth(r.sheetName, col, col, boundedCellWidth(float64(width)*1.123)); err != nil {
			return err
		}
	}
	return nil
}

func (r *excelReport) processColumnMeta(headerIndices map[string]int) error {
	for k, m := range r.meta.col {
		colIdx := headerIndices[k]
		col, err := excelize.ColumnNumberToName(colIdx + 1)
		if err != nil {
			return err
		}
		if width, ok := m["width"]; ok {
			if val, ok := width.(float64); ok {
				r.meta.width[col] = val
			}
		}
		if styleString, ok := m["style"]; ok {
			if styleJSON, ok := styleString.(string); ok {
				r.meta.style[col] = styleJSON
			}
		}
		if lookup, ok := m["lookup"]; ok {
			if key, ok := lookup.(string); ok {
				t := r.translator
				r.meta.parser[colIdx] = func(arg any) (any, error) {
					if t == nil {
						return arg, nil
					}
					return t.Label(key, arg), nil
				}
			}
		}
	}
	return nil
}

func (r *excelReport) processStyleMeta() error {
	// Apply styles in specificity order (columns → rows → cells) so that
	// more-specific targets win at intersections (e.g. a row-bold style beats
	// a column-numfmt style at their shared header cell).
	classify := func(target string) int {
		if colRe.MatchString(target) {
			return styleTargetCol
		}
		if _, err := strconv.Atoi(target); err == nil {
			return styleTargetRow
		}
		return styleTargetCell
	}

	for _, bucket := range []int{styleTargetCol, styleTargetRow, styleTargetCell} {
		for target, styleJSON := range r.meta.style {
			target = strings.TrimSpace(strings.ToUpper(target))
			if classify(target) != bucket {
				continue
			}

			var style excelize.Style
			if err := json.Unmarshal([]byte(styleJSON), &style); err != nil {
				return err
			}
			styleID, err := r.excel.NewStyle(&style)
			if err != nil {
				return err
			}
			if err := r.applyStyleTarget(bucket, target, styleID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *excelReport) applyStyleTarget(bucket int, target string, styleID int) error {
	switch bucket {
	case styleTargetCol:
		return r.excel.SetColStyle(r.sheetName, target, styleID)
	case styleTargetRow:
		rowNum, _ := strconv.Atoi(target)
		return r.excel.SetRowStyle(r.sheetName, rowNum, rowNum, styleID)
	default:
		from, to := colRange(target)
		return r.excel.SetCellStyle(r.sheetName, from, to, styleID)
	}
}

func (r *excelReport) processWidthMeta() error {
	for target, width := range r.meta.width {
		target = strings.TrimSpace(strings.ToUpper(target))
		if colRe.MatchString(target) {
			from, to := colRange(target)
			if err := r.excel.SetColWidth(r.sheetName, from, to, boundedCellWidth(width)); err != nil {
				return err
			}
		}
	}
	return nil
}

func calcCellWidth(str string) int {
	// ASCII (SBCS) counts as 1, non-ASCII (MBCS e.g. CJK) counts as 2.
	w := 0
	for _, ch := range str {
		if ch < 0x80 {
			w++
		} else {
			w += 2
		}
	}
	return w
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
