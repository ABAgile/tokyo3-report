package report

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/xuri/excelize/v2"
)

type lookupTranslator struct{}

func (lookupTranslator) Header(key string) string { return key }
func (lookupTranslator) Lookup(_ string, code any) string {
	switch code {
	case 1:
		return "Active"
	}
	return ""
}

func TestGenerateScript_InvalidScript(t *testing.T) {
	outputPath := "/tmp/test_invalid_script.xlsx"
	worksheet := WorksheetConf{SheetName: "TestSheet", Script: "x = ("}
	defer os.Remove(outputPath)
	assert.Error(t, generate(outputPath, worksheet, nil))
}

func TestGenerateScript(t *testing.T) {
	type TestGroupStruct struct {
		Group string
		Value int
	}

	type TestMultiGroupStruct struct {
		GroupA string
		GroupB string
		Value  int
	}

	type TestStruct struct {
		Name   string
		Age    int
		Member bool
		Joined time.Time
	}

	testCases := []struct {
		name             string
		outputPath       string
		worksheet        WorksheetConf
		rowReader        RowsReader
		expectedRows     [][]string
		customValidation func(t *testing.T, f *excelize.File)
	}{
		{
			name:       "With Grouping",
			outputPath: "/tmp/test_group.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: "groupFields = [\"Group\"]"},
			rowReader: NewStructRows([]any{
				TestGroupStruct{Group: "A", Value: 1},
				TestGroupStruct{Group: "A", Value: 2},
				TestGroupStruct{Group: "B", Value: 3},
			}),
			expectedRows: [][]string{
				{"Group", "Value"},
				{"A", "1"},
				{"A", "2"},
				nil,
				{"B", "3"},
			},
		},
		{
			// Regression test: insertGroupBreaks must insert at most one blank
			// row per data-row transition, even when several group fields differ
			// at once (e.g. row 2 -> row 3 differs in both GroupA and GroupB).
			name:       "With Multi-field Grouping",
			outputPath: "/tmp/test_multi_group.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: `groupFields = ["GroupA", "GroupB"]`},
			rowReader: NewStructRows([]any{
				TestMultiGroupStruct{GroupA: "A", GroupB: "X", Value: 1},
				TestMultiGroupStruct{GroupA: "A", GroupB: "Y", Value: 2},
				TestMultiGroupStruct{GroupA: "B", GroupB: "Z", Value: 3},
			}),
			expectedRows: [][]string{
				{"Group A", "Group B", "Value"},
				{"A", "X", "1"},
				nil,
				{"A", "Y", "2"},
				nil,
				{"B", "Z", "3"},
			},
		},
		{
			name:       "With Script",
			outputPath: "/tmp/test_script.xlsx",
			worksheet: WorksheetConf{
				SheetName: "TestSheet",
				Script: `
width = {"A": 50}
style = {"C": "{\"font\":{\"bold\":true}}"}
col = {"Age": {"style": "{\"font\":{\"italic\":true}}"}}
`,
			},
			rowReader: NewStructRows([]any{
				TestStruct{Name: "John", Age: 30, Member: true, Joined: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)},
			}),
			expectedRows: [][]string{
				{"Name", "Age", "Member", "Joined"},
				{"John", "30", "TRUE", "01-01-22"},
			},
			customValidation: func(t *testing.T, f *excelize.File) {
				width, err := f.GetColWidth("TestSheet", "A")
				assert.NoError(t, err)
				assert.Equal(t, 50.0, width)

				styleID, err := f.GetCellStyle("TestSheet", "C1")
				assert.NoError(t, err)
				style, err := f.GetStyle(styleID)
				assert.NoError(t, err)
				assert.True(t, style.Font.Bold)

				styleID, err = f.GetCellStyle("TestSheet", "B2")
				assert.NoError(t, err)
				style, err = f.GetStyle(styleID)
				assert.NoError(t, err)
				assert.True(t, style.Font.Italic)
			},
		},
		{
			name:       "Row style",
			outputPath: "/tmp/test_row_style.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: `style = {"1": "{\"font\":{\"bold\":true}}"}`},
			rowReader: NewStructRows([]any{
				struct{ Name string }{"Alice"},
			}),
			expectedRows: [][]string{{"Name"}, {"Alice"}},
		},
		{
			name:       "Cell style",
			outputPath: "/tmp/test_cell_style.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: `style = {"A1": "{\"font\":{\"bold\":true}}"}`},
			rowReader: NewStructRows([]any{
				struct{ Name string }{"Alice"},
			}),
			expectedRows: [][]string{{"Name"}, {"Alice"}},
			customValidation: func(t *testing.T, f *excelize.File) {
				styleID, err := f.GetCellStyle("TestSheet", "A1")
				assert.NoError(t, err)
				style, err := f.GetStyle(styleID)
				assert.NoError(t, err)
				assert.True(t, style.Font.Bold)
			},
		},
		{
			name:       "Column range width",
			outputPath: "/tmp/test_range_width.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: `width = {"A:B": 25}`},
			rowReader: NewStructRows([]any{
				struct{ Name, Role string }{"Alice", "Dev"},
			}),
			expectedRows: [][]string{{"Name", "Role"}, {"Alice", "Dev"}},
			customValidation: func(t *testing.T, f *excelize.File) {
				for _, col := range []string{"A", "B"} {
					w, err := f.GetColWidth("TestSheet", col)
					assert.NoError(t, err)
					assert.Equal(t, 25.0, w)
				}
			},
		},
		{
			name:       "Width via col meta",
			outputPath: "/tmp/test_col_meta_width.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: `col = {"Name": {"width": 30}}`},
			rowReader: NewStructRows([]any{
				struct{ Name string }{"Alice"},
			}),
			expectedRows: [][]string{{"Name"}, {"Alice"}},
			customValidation: func(t *testing.T, f *excelize.File) {
				w, err := f.GetColWidth("TestSheet", "A")
				assert.NoError(t, err)
				assert.Equal(t, 30.0, w)
			},
		},
		{
			// Regression test: processColumnMeta must ignore col meta entries
			// that reference a column name absent from the header row instead
			// of falling back to index 0 and corrupting column A's width.
			name:       "Col meta with unknown column",
			outputPath: "/tmp/test_col_meta_unknown.xlsx",
			worksheet:  WorksheetConf{SheetName: "TestSheet", Script: `col = {"DoesNotExist": {"width": 30}}`},
			rowReader: NewStructRows([]any{
				struct{ Name string }{"Alice"},
			}),
			expectedRows: [][]string{{"Name"}, {"Alice"}},
			customValidation: func(t *testing.T, f *excelize.File) {
				w, err := f.GetColWidth("TestSheet", "A")
				assert.NoError(t, err)
				assert.NotEqual(t, 30.0, w)
			},
		},
		{
			name:       "With parser",
			outputPath: "/tmp/test_parser.xlsx",
			worksheet: WorksheetConf{
				SheetName: "TestSheet",
				Script:    `col = {"Name": {"parser": "uppercase"}}`,
				Parsers: map[string]Parser{
					"uppercase": func(value any) (any, error) {
						return strings.ToUpper(value.(string)), nil
					},
				},
			},
			rowReader:    NewStructRows([]any{struct{ Name string }{"Alice"}}),
			expectedRows: [][]string{{"Name"}, {"ALICE"}},
		},
		{
			name:       "With lookup",
			outputPath: "/tmp/test_lookup.xlsx",
			worksheet: WorksheetConf{
				SheetName:  "TestSheet",
				Script:     `col = {"Status": {"lookup": "status"}}`,
				Translator: lookupTranslator{},
			},
			rowReader: NewStructRows([]any{
				struct {
					Name   string
					Status int
				}{"Alice", 1},
			}),
			expectedRows: [][]string{
				{"Name", "Status"},
				{"Alice", "Active"},
			},
		},
		{
			name:       "Parser before lookup",
			outputPath: "/tmp/test_parser_lookup.xlsx",
			worksheet: WorksheetConf{
				SheetName:  "TestSheet",
				Script:     `col = {"Status": {"parser": "status_code", "lookup": "status"}}`,
				Translator: lookupTranslator{},
				Parsers: map[string]Parser{
					"status_code": func(value any) (any, error) {
						return strconv.Atoi(strings.TrimSpace(value.(string)))
					},
				},
			},
			rowReader:    NewStructRows([]any{struct{ Status string }{" 1 "}}),
			expectedRows: [][]string{{"Status"}, {"Active"}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, generate(tc.outputPath, tc.worksheet, tc.rowReader))

			f, err := excelize.OpenFile(tc.outputPath)
			assert.NoError(t, err)

			rows, err := f.GetRows(tc.worksheet.SheetName)
			assert.NoError(t, err)

			assert.Equal(t, len(tc.expectedRows), len(rows))
			for i, expectedRow := range tc.expectedRows {
				assert.Equal(t, expectedRow, rows[i])
			}

			if tc.customValidation != nil {
				tc.customValidation(t, f)
			}

			os.Remove(tc.outputPath)
		})
	}
}

func TestGenerateScriptUnknownParser(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "unknown-parser.xlsx")
	worksheet := WorksheetConf{SheetName: "TestSheet", Script: `col = {"Name": {"parser": "missing"}}`}

	err := generate(outputPath, worksheet, NewStructRows([]any{struct{ Name string }{"Alice"}}))
	assert.EqualError(t, err, `parser "missing" for column "Name" is not registered`)
}

func TestGenerateScriptParserError(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "parser-error.xlsx")
	worksheet := WorksheetConf{
		SheetName: "TestSheet",
		Script:    `col = {"Name": {"parser": "fail"}}`,
		Parsers: map[string]Parser{
			"fail": func(any) (any, error) { return nil, errors.New("invalid value") },
		},
	}

	err := generate(outputPath, worksheet, NewStructRows([]any{struct{ Name string }{"Alice"}}))
	assert.EqualError(t, err, `row 2: parse column "Name" with "fail": invalid value`)
}

func TestGenerate_PreservesExistingWorkbookOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	worksheet := WorksheetConf{SheetName: "TestSheet"}
	assert.NoError(t, generate(path, worksheet, NewStructRows([]any{
		struct{ Name string }{"Original"},
	})))

	worksheet.Script = `col = {"Name": {"parser": "fail"}}`
	worksheet.Parsers = map[string]Parser{
		"fail": func(any) (any, error) { return nil, errors.New("invalid value") },
	}
	assert.Error(t, generate(path, worksheet, NewStructRows([]any{
		struct{ Name string }{"Replacement"},
	})))

	f, err := excelize.OpenFile(path)
	assert.NoError(t, err)
	defer f.Close()
	rows, err := f.GetRows("TestSheet")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Original"}}, rows)
}
