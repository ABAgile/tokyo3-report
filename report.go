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
	"unicode/utf8"

	"github.com/gobuffalo/flect"
	"github.com/lithammer/shortuuid/v4"
	"github.com/xuri/excelize/v2"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

const (
	DefaultSheetName          = "Sheet1"
	OpenXMLShortDateFmtDateId = 14
)

// Translator supplies display strings for header names and enum-mapped values.
// A nil Translator is valid; headers fall back to flect.Titleize and values pass through unchanged.
type Translator interface {
	T(key string) string
	Label(key string, code any) string
}

type ReportConf struct {
	OutputPath,
	Script,
	SheetName,
	QueryPath string
	Translator Translator
}

type parserFn func(any) (any, error)

type scriptMeta struct {
	col         map[string]map[string]any
	parser      map[int]parserFn
	style       map[string]string
	width       map[string]float64
	groupFields []string
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
	excel := report.excel
	defer func() {
		if saveErr := report.saveWorkbook(); err == nil {
			err = saveErr
		}
	}()

	defaultDateStyleID, err := excel.NewStyle(&excelize.Style{NumFmt: OpenXMLShortDateFmtDateId})
	if err != nil {
		return
	}

	if err = report.runScript(conf.Script); err != nil {
		return
	}

	if rowReader != nil {
		if err = report.prepareWorksheet(); err != nil {
			return
		}

		if err = rowReader.Read(); err != nil {
			return
		}
		if rowReader.Err() != sql.ErrNoRows {
			headerIndices := make(map[string]int)
			headers, err := rowReader.Headers()
			if err != nil {
				return err
			}
			fieldWidths := make([]int, len(headers))
			for i, hdr := range headers {
				if conf.Translator != nil {
					headers[i] = conf.Translator.T(hdr)
				} else {
					headers[i] = flect.Titleize(hdr)
				}
				headerIndices[hdr] = i
				cellWidth := calcCellWidth(headers[i])
				if cellWidth > fieldWidths[i] {
					fieldWidths[i] = cellWidth
				}
			}
			if err := excel.SetSheetRow(report.sheetName, "A1", &headers); err != nil {
				return err
			}

			if err := report.processColunmMeta(headerIndices); err != nil {
				return err
			}

			var lastRow []any
			for i := 2; rowReader.Next(); i++ {
				fields, err := rowReader.Values()
				if err != nil {
					return err
				}

				for i, fn := range report.meta.parser {
					if result, err := fn(fields[i]); err == nil {
						fields[i] = result
					}
				}

				if lastRow != nil {
					for _, field := range report.meta.groupFields {
						if colIdx, ok := headerIndices[field]; ok {
							if fields[colIdx] != lastRow[colIdx] {
								cell := fmt.Sprintf("A%d", i)
								if err := excel.SetSheetRow(report.sheetName, cell, &[]any{}); err != nil {
									return err
								}
								i++
							}
						}
					}
				}
				lastRow = fields

				cell := fmt.Sprintf("A%d", i)
				if err := excel.SetSheetRow(report.sheetName, cell, &fields); err != nil {
					return err
				}

				for col, width := range fieldWidths {
					var str string
					if t, ok := fields[col].(time.Time); ok {
						// force using default date-style to avoid Excel sometime automatic convert to MMM-YY when open the file
						cellName, err := excelize.CoordinatesToCellName(col+1, i)
						if err != nil {
							return err
						}
						if err := excel.SetCellStyle(report.sheetName, cellName, cellName, defaultDateStyleID); err != nil {
							return err
						}
						str = t.Format(time.RFC3339)
					} else {
						str = fmt.Sprintf("%v", fields[col])
					}
					cellWidth := calcCellWidth(str)
					if cellWidth > width {
						fieldWidths[col] = cellWidth
					}
				}
			}

			for colIdx, width := range fieldWidths {
				col, err := excelize.ColumnNumberToName(colIdx + 1)
				if err != nil {
					return err
				}
				if err := excel.SetColWidth(report.sheetName, col, col, boundedCellWidth(float64(width)*1.123)); err != nil {
					return err
				}
			}
		}
	} else {
		if _, err = excel.NewSheet(report.sheetName); err != nil {
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

func (r *excelReport) saveWorkbook() error {
	if err := r.excel.SaveAs(r.filePath); err != nil {
		return err
	}
	return r.excel.Close()
}

func (r *excelReport) runScript(script string) error {
	meta := scriptMeta{
		col:         make(map[string]map[string]any),
		parser:      make(map[int]parserFn),
		style:       make(map[string]string),
		width:       make(map[string]float64),
		groupFields: []string{},
	}

	if script != "" {
		thread := &starlark.Thread{}
		globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, "script.star", script, nil)
		if err != nil {
			return err
		}

		if val, ok := globals["col"]; ok {
			if dict, ok := val.(*starlark.Dict); ok {
				for _, item := range dict.Items() {
					colName, ok := starlark.AsString(item[0])
					if !ok {
						continue
					}
					entryDict, ok := item[1].(*starlark.Dict)
					if !ok {
						continue
					}
					entry := make(map[string]any)
					for _, kv := range entryDict.Items() {
						k, ok := starlark.AsString(kv[0])
						if !ok {
							continue
						}
						switch v := kv[1].(type) {
						case starlark.String:
							entry[k] = string(v)
						case starlark.Bool:
							entry[k] = bool(v)
						default:
							if f, ok := starlark.AsFloat(kv[1]); ok {
								entry[k] = f
							}
						}
					}
					meta.col[colName] = entry
				}
			}
		}

		if val, ok := globals["style"]; ok {
			if dict, ok := val.(*starlark.Dict); ok {
				for _, item := range dict.Items() {
					k, ok := starlark.AsString(item[0])
					if !ok {
						continue
					}
					v, ok := starlark.AsString(item[1])
					if !ok {
						continue
					}
					meta.style[k] = v
				}
			}
		}

		if val, ok := globals["width"]; ok {
			if dict, ok := val.(*starlark.Dict); ok {
				for _, item := range dict.Items() {
					k, ok := starlark.AsString(item[0])
					if !ok {
						continue
					}
					if f, ok := starlark.AsFloat(item[1]); ok {
						meta.width[k] = f
					}
				}
			}
		}

		if val, ok := globals["groupFields"]; ok {
			if list, ok := val.(*starlark.List); ok {
				for i := 0; i < list.Len(); i++ {
					if s, ok := starlark.AsString(list.Index(i)); ok {
						meta.groupFields = append(meta.groupFields, s)
					}
				}
			}
		}
	}

	r.meta = &meta
	return nil
}

func (r *excelReport) processColunmMeta(headerIndices map[string]int) error {
	for k, m := range r.meta.col {
		colIdx := headerIndices[k]
		if width, ok := m["width"]; ok {
			if val, ok := width.(float64); ok {
				col, err := excelize.ColumnNumberToName(colIdx + 1)
				if err != nil {
					return err
				}
				r.meta.width[col] = val
			}
		}
		if styleString, ok := m["style"]; ok {
			if styleJSON, ok := styleString.(string); ok {
				col, err := excelize.ColumnNumberToName(colIdx + 1)
				if err != nil {
					return err
				}
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
	colRe := regexp.MustCompile(`^[A-Z]+(:[A-Z]+)?$`)

	// Apply styles in specificity order (columns → rows → cells) so that
	// more-specific targets win at intersections (e.g. a row-bold style beats
	// a column-numfmt style at their shared header cell).
	const (
		bucketCol = iota
		bucketRow
		bucketCell
	)
	classify := func(target string) int {
		if colRe.MatchString(target) {
			return bucketCol
		}
		if _, err := strconv.Atoi(target); err == nil {
			return bucketRow
		}
		return bucketCell
	}

	for _, bucket := range []int{bucketCol, bucketRow, bucketCell} {
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

			switch bucket {
			case bucketCol:
				if err := r.excel.SetColStyle(r.sheetName, target, styleID); err != nil {
					return err
				}
			case bucketRow:
				rowNum, _ := strconv.Atoi(target)
				if err := r.excel.SetRowStyle(r.sheetName, rowNum, rowNum, styleID); err != nil {
					return err
				}
			default:
				parts := strings.Split(target, ":")
				if len(parts) == 1 {
					if err := r.excel.SetCellStyle(r.sheetName, target, target, styleID); err != nil {
						return err
					}
				} else if len(parts) == 2 {
					if err := r.excel.SetCellStyle(r.sheetName, parts[0], parts[1], styleID); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (r *excelReport) processWidthMeta() error {
	for target, width := range r.meta.width {
		target = strings.TrimSpace(strings.ToUpper(target))
		if regexp.MustCompile(`^[A-Z]+(:[A-Z]+)?$`).MatchString(target) {
			parts := strings.Split(target, ":")
			if len(parts) == 1 {
				if err := r.excel.SetColWidth(r.sheetName, target, target, boundedCellWidth(width)); err != nil {
					return err
				}
			} else if len(parts) == 2 {
				if err := r.excel.SetColWidth(r.sheetName, parts[0], parts[1], boundedCellWidth(width)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func calcCellWidth(str string) int {
	// Single Byte Character Set(SBCS) is 1 holds
	// Multi-Byte Character System(MBCS) is 2 holds
	var cellLenSBCS, cellLenMBCS int

	for _, ch := range str {
		if ch < 0x80 {
			cellLenSBCS++
		}
	}

	runeLen := utf8.RuneCountInString(str)
	cellLenMBCS = runeLen - cellLenSBCS

	return cellLenSBCS + cellLenMBCS*2
}

func boundedCellWidth(width float64) float64 {
	return max(min(width, 100.0), 10.0)
}
