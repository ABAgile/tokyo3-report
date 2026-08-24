package report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Header(key string) string
	Lookup(key string, code any) string
}

// Parser converts a raw field value before any configured lookup is applied.
type Parser func(any) (any, error)

type WorksheetConf struct {
	SheetName  string
	Rows       RowsReader
	Script     string
	Translator Translator
	Parsers    map[string]Parser
}

type worksheetState struct {
	WorksheetConf
	meta        *scriptMeta
	fieldWidths []int
	styleIDs    map[string]int
}

type excelReport struct {
	hasDefaultSheet bool
	excel           *excelize.File

	filePath,
	sheetName string

	meta       *scriptMeta
	translator Translator
	parsers    map[string]Parser
}

func Generate(outputPath string, worksheets ...WorksheetConf) (err error) {
	if err = validateWorksheets(outputPath, worksheets); err != nil {
		return
	}

	report := excelReport{filePath: outputPath}
	if err = report.openWorkbook(); err != nil {
		return
	}
	defer func() {
		if closeErr := report.excel.Close(); err == nil {
			err = closeErr
		}
	}()

	states := make([]worksheetState, len(worksheets))
	for i, worksheet := range worksheets {
		state := &states[i]
		state.WorksheetConf = worksheet
		report.selectWorksheet(state)
		if err = report.runScript(state.Script); err != nil {
			return
		}
		if err = report.prepareWorksheet(state.Rows != nil); err != nil {
			return
		}
		state.meta = report.meta
		if state.Rows == nil {
			if state.styleIDs, err = report.processStyleMeta(); err != nil {
				return
			}
		}
	}

	for i := range states {
		state := &states[i]
		report.selectWorksheet(state)
		if state.Rows != nil {
			if state.fieldWidths, state.styleIDs, err = report.populateSheet(state.Rows); err != nil {
				return
			}
		}
	}

	return report.saveWorkbook(states)
}

func validateWorksheets(outputPath string, worksheets []WorksheetConf) error {
	if outputPath == "" {
		return fmt.Errorf("output path is empty")
	}
	if len(worksheets) == 0 {
		return fmt.Errorf("at least one worksheet is required")
	}

	seen := make(map[string]int, len(worksheets))
	for i, worksheet := range worksheets {
		if worksheet.SheetName == "" {
			return fmt.Errorf("worksheet %d has an empty name", i+1)
		}
		key := strings.ToLower(worksheet.SheetName)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("worksheet %d %q duplicates worksheet %d", i+1, worksheet.SheetName, previous)
		}
		seen[key] = i + 1
	}
	return nil
}

func (r *excelReport) selectWorksheet(state *worksheetState) {
	r.sheetName = state.SheetName
	r.translator = state.Translator
	r.parsers = state.Parsers
	r.meta = state.meta
}

func (r *excelReport) openWorkbook() error {
	info, err := os.Stat(r.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		r.hasDefaultSheet = true
	} else if info.Size() == 0 {
		r.hasDefaultSheet = true
	}
	if r.hasDefaultSheet {
		r.excel = excelize.NewFile()
	} else {
		r.excel, err = excelize.OpenFile(r.filePath)
		if err != nil {
			return err
		}
	}
	return nil
}

// writeWorkbookTemp writes the workbook to a sibling temporary file and
// returns its path without changing the output path.
func (r *excelReport) writeWorkbookTemp() (tmpPath string, err error) {
	targetPath, err := resolveOutputPath(r.filePath)
	if err != nil {
		return "", err
	}

	mode, replacing := os.FileMode(0o666), false
	if info, statErr := os.Stat(targetPath); statErr == nil {
		mode, replacing = info.Mode().Perm(), true
	}

	tmpPath = filepath.Join(filepath.Dir(targetPath), "."+filepath.Base(targetPath)+".tmp-"+shortuuid.New()+".xlsx")
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	// Write uses Path to determine the workbook content type. The temporary
	// filename is only a staging path, so retain the requested output path.
	r.excel.Path = r.filePath
	if err = r.excel.Write(tmp); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	if replacing {
		if err = os.Chmod(tmpPath, mode); err != nil {
			return "", err
		}
	}
	return tmpPath, nil
}

// saveWorkbook writes the completed workbook to a sibling temporary file,
// patches styles and column widths directly in its worksheet XML, and then
// atomically replaces the output. The staged file is never read back into
// Excelize, so worksheet data is never materialized in memory.
func (r *excelReport) saveWorkbook(states []worksheetState) (err error) {
	stagePath, err := r.writeWorkbookTemp()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(stagePath)
		}
	}()

	if err = r.applyWorksheetMeta(stagePath, states); err != nil {
		return err
	}
	targetPath, err := resolveOutputPath(r.filePath)
	if err != nil {
		return err
	}
	return os.Rename(stagePath, targetPath)
}

// resolveOutputPath follows existing symlinks so atomic replacement updates
// their target instead of replacing the symlink itself. A new path is kept as
// requested because there is no link target to resolve.
func resolveOutputPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return path, nil
	}
	return "", err
}

func (r *excelReport) prepareWorksheet(replaceExisting bool) error {
	index, err := r.excel.GetSheetIndex(r.sheetName)
	exists := err == nil && index >= 0
	if exists && !replaceExisting {
		if r.hasDefaultSheet && r.sheetName == DefaultSheetName {
			r.hasDefaultSheet = false
		}
		return nil
	}

	sheetName := shortuuid.New()
	if _, err := r.excel.NewSheet(sheetName); err != nil {
		return err
	}
	if r.hasDefaultSheet {
		if err := r.excel.DeleteSheet(DefaultSheetName); err != nil {
			return err
		}
		r.hasDefaultSheet = false
	} else if exists {
		if err := r.excel.DeleteSheet(r.sheetName); err != nil {
			return err
		}
	}
	return r.excel.SetSheetName(sheetName, r.sheetName)
}

func (r *excelReport) populateSheet(rowReader RowsReader) ([]int, map[string]int, error) {
	if err := rowReader.Read(); err != nil {
		return nil, nil, err
	}
	if rowReader.Err() == sql.ErrNoRows {
		styleIDs, err := r.processStyleMeta()
		return nil, styleIDs, err
	}

	headers, headerIndices, fieldWidths, err := r.prepareHeaders(rowReader)
	if err != nil {
		return nil, nil, err
	}
	if err := r.processColumnMeta(headerIndices); err != nil {
		return nil, nil, err
	}
	styleIDs, err := r.processStyleMeta()
	if err != nil {
		return nil, nil, err
	}
	stream, err := r.excel.NewStreamWriter(r.sheetName)
	if err != nil {
		return nil, nil, err
	}
	if err := stream.SetRow("A1", headers); err != nil {
		return nil, nil, err
	}
	if err := r.writeDataRows(stream, rowReader, headerIndices, fieldWidths); err != nil {
		return nil, nil, err
	}
	if err := rowReader.Err(); err != nil {
		return nil, nil, err
	}
	if err := stream.Flush(); err != nil {
		return nil, nil, err
	}
	return fieldWidths, styleIDs, nil
}

func (r *excelReport) prepareHeaders(rowReader RowsReader) ([]any, map[string]int, []int, error) {
	rawHeaders, err := rowReader.Headers()
	if err != nil {
		return nil, nil, nil, err
	}

	headers := make([]any, len(rawHeaders))
	headerIndices := make(map[string]int, len(rawHeaders))
	fieldWidths := make([]int, len(rawHeaders))
	for i, hdr := range rawHeaders {
		if r.translator != nil {
			headers[i] = r.translator.Header(hdr)
		} else {
			headers[i] = flect.Titleize(hdr)
		}
		headerIndices[hdr] = i
		fieldWidths[i] = calcCellWidth(fmt.Sprint(headers[i]))
	}
	return headers, headerIndices, fieldWidths, nil
}

func (r *excelReport) writeDataRows(stream *excelize.StreamWriter, rowReader RowsReader, headerIndices map[string]int, fieldWidths []int) error {
	dateStyleID, err := r.excel.NewStyle(&excelize.Style{NumFmt: OpenXMLShortDateFmtDateId})
	if err != nil {
		return err
	}

	var lastRow []any
	rowIdx := 2
	for rowReader.Next() {
		fields, err := rowReader.Values()
		if err != nil {
			return err
		}

		for colIdx, fn := range r.meta.parser {
			result, err := fn(fields[colIdx])
			if err != nil {
				return fmt.Errorf("row %d: %w", rowIdx, err)
			}
			fields[colIdx] = result
		}

		if lastRow != nil {
			if rowIdx, err = r.insertGroupBreaks(stream, rowIdx, fields, lastRow, headerIndices); err != nil {
				return err
			}
		}

		values := make([]any, len(fields))
		for col, field := range fields {
			if _, ok := field.(time.Time); ok {
				values[col] = excelize.Cell{StyleID: dateStyleID, Value: field}
			} else {
				values[col] = field
			}
		}
		if err := stream.SetRow(fmt.Sprintf("A%d", rowIdx), values); err != nil {
			return err
		}
		trackFieldWidths(fields, fieldWidths)
		lastRow = fields
		rowIdx++
	}
	return nil
}

func (r *excelReport) insertGroupBreaks(stream *excelize.StreamWriter, rowIdx int, fields, lastRow []any, headerIndices map[string]int) (int, error) {
	for _, field := range r.meta.groupFields {
		if colIdx, ok := headerIndices[field]; ok && fields[colIdx] != lastRow[colIdx] {
			if err := stream.SetRow(fmt.Sprintf("A%d", rowIdx), []any{}); err != nil {
				return rowIdx, err
			}
			return rowIdx + 1, nil
		}
	}
	return rowIdx, nil
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

func (r *excelReport) processColumnMeta(headerIndices map[string]int) error {
	for k, m := range r.meta.col {
		colIdx, ok := headerIndices[k]
		if !ok {
			continue
		}
		col, err := excelize.ColumnNumberToName(colIdx + 1)
		if err != nil {
			return err
		}
		if width, ok := m["width"].(float64); ok {
			r.meta.width[col] = width
		}
		if styleJSON, ok := m["style"].(string); ok {
			r.meta.style[col] = styleJSON
		}
		parserName, hasParser := m["parser"].(string)
		parser := r.parsers[parserName]
		if hasParser && parser == nil {
			return fmt.Errorf("parser %q for column %q is not registered", parserName, k)
		}
		lookupKey, hasLookup := m["lookup"].(string)
		if hasParser || hasLookup {
			r.meta.parser[colIdx] = func(arg any) (any, error) {
				if hasParser {
					parsed, err := parser(arg)
					if err != nil {
						return nil, fmt.Errorf("parse column %q with %q: %w", k, parserName, err)
					}
					arg = parsed
				}
				if hasLookup && r.translator != nil {
					arg = r.translator.Lookup(lookupKey, arg)
				}
				return arg, nil
			}
		}
	}
	return nil
}

func styleTargetKind(target string) int {
	if colRe.MatchString(target) {
		return styleTargetCol
	}
	if _, err := strconv.Atoi(target); err == nil {
		return styleTargetRow
	}
	return styleTargetCell
}

// processStyleMeta parses script styles and creates workbook-wide IDs. XML
// targets are applied later by applyWorksheetMeta.
func (r *excelReport) processStyleMeta() (map[string]int, error) {
	styleIDs := make(map[string]int, len(r.meta.style))
	for target, styleJSON := range r.meta.style {
		target = normalizeTarget(target)
		var style excelize.Style
		if err := json.Unmarshal([]byte(styleJSON), &style); err != nil {
			return nil, err
		}
		styleID, err := r.excel.NewStyle(&style)
		if err != nil {
			return nil, err
		}
		styleIDs[target] = styleID
	}
	return styleIDs, nil
}

func worksheetPatchRulesForState(state worksheetState) (patchRules, error) {
	rules := patchRules{
		Cells: make(map[string]int),
	}
	for target, styleID := range state.styleIDs {
		target = normalizeTarget(target)
		switch styleTargetKind(target) {
		case styleTargetCol:
			fromCol, toCol, err := columnSpan(target)
			if err != nil {
				return patchRules{}, err
			}
			rules.Cols = append(rules.Cols, styleSpan{Min: fromCol, Max: toCol, StyleID: styleID})
		case styleTargetRow:
			row, err := strconv.Atoi(target)
			if err != nil || row < 1 || row > excelize.TotalRows {
				return patchRules{}, fmt.Errorf("invalid style row %q", target)
			}
			rules.Rows = append(rules.Rows, styleSpan{Min: row, Max: row, StyleID: styleID})
		case styleTargetCell:
			from, to := colRange(target)
			fromCol, fromRow, err := excelize.CellNameToCoordinates(from)
			if err != nil {
				return patchRules{}, err
			}
			toCol, toRow, err := excelize.CellNameToCoordinates(to)
			if err != nil {
				return patchRules{}, err
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
	for colIdx, width := range state.fieldWidths {
		if _, err := excelize.ColumnNumberToName(colIdx + 1); err != nil {
			return patchRules{}, err
		}
		rules.Widths = append(rules.Widths, widthSpan{
			Min:   colIdx + 1,
			Max:   colIdx + 1,
			Width: boundedCellWidth(float64(width) * 1.123),
		})
	}
	if state.meta != nil {
		for target, width := range state.meta.width {
			target = normalizeTarget(target)
			if !colRe.MatchString(target) {
				continue
			}
			fromCol, toCol, err := columnSpan(target)
			if err != nil {
				return patchRules{}, err
			}
			rules.Widths = append(rules.Widths, widthSpan{
				Min:   fromCol,
				Max:   toCol,
				Width: boundedCellWidth(width),
			})
		}
	}
	return rules, nil
}

// applyWorksheetMeta rewrites worksheet XML directly, so applying styles and
// widths does not decode the full sheet into Excelize's normal-mode structures.
func (r *excelReport) applyWorksheetMeta(stagePath string, states []worksheetState) error {
	paths, err := worksheetPaths(stagePath)
	if err != nil {
		return err
	}

	rules := make(map[string]patchRules)
	for _, state := range states {
		worksheetRules, err := worksheetPatchRulesForState(state)
		if err != nil {
			return err
		}
		if len(worksheetRules.Cols) == 0 && len(worksheetRules.Rows) == 0 &&
			len(worksheetRules.Cells) == 0 && len(worksheetRules.CellRanges) == 0 &&
			len(worksheetRules.Widths) == 0 {
			continue
		}
		worksheetPath, ok := paths[state.SheetName]
		if !ok {
			return fmt.Errorf("worksheet %q not found in staged workbook", state.SheetName)
		}
		rules[worksheetPath] = worksheetRules
	}
	if len(rules) == 0 {
		return nil
	}

	patchedPath := stagePath + ".styles"
	if err := transformWorkbook(stagePath, patchedPath, rules); err != nil {
		return err
	}
	if err := os.Rename(patchedPath, stagePath); err != nil {
		os.Remove(patchedPath)
		return err
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

func normalizeTarget(target string) string {
	return strings.TrimSpace(strings.ToUpper(target))
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
