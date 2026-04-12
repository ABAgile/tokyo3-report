package report

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/xuri/excelize/v2"
)

type lookupTranslator struct{}

func (lookupTranslator) T(key string) string { return key }
func (lookupTranslator) Label(_ string, code any) string {
	switch code {
	case 1:
		return "Active"
	}
	return ""
}

func TestGenerateScript_InvalidScript(t *testing.T) {
	conf := &ReportConf{
		OutputPath: "/tmp/test_invalid_script.xlsx",
		SheetName:  "TestSheet",
		Script:     "x = (",
	}
	defer os.Remove(conf.OutputPath)
	assert.Error(t, Generate(conf, nil))
}

func TestGenerateScript(t *testing.T) {
	type TestGroupStruct struct {
		Group string
		Value int
	}

	type TestStruct struct {
		Name   string
		Age    int
		Member bool
		Joined time.Time
	}

	testCases := []struct {
		name             string
		conf             *ReportConf
		rowReader        RowsReader
		expectedRows     [][]string
		customValidation func(t *testing.T, f *excelize.File)
	}{
		{
			name: "With Grouping",
			conf: &ReportConf{
				OutputPath: "/tmp/test_group.xlsx",
				SheetName:  "TestSheet",
				Script:     "groupFields = [\"Group\"]",
			},
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
			name: "With Script",
			conf: &ReportConf{
				OutputPath: "/tmp/test_script.xlsx",
				SheetName:  "TestSheet",
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
			name: "Row style",
			conf: &ReportConf{
				OutputPath: "/tmp/test_row_style.xlsx",
				SheetName:  "TestSheet",
				Script:     `style = {"1": "{\"font\":{\"bold\":true}}"}`,
			},
			rowReader: NewStructRows([]any{
				struct{ Name string }{"Alice"},
			}),
			expectedRows: [][]string{{"Name"}, {"Alice"}},
		},
		{
			name: "Cell style",
			conf: &ReportConf{
				OutputPath: "/tmp/test_cell_style.xlsx",
				SheetName:  "TestSheet",
				Script:     `style = {"A1": "{\"font\":{\"bold\":true}}"}`,
			},
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
			name: "Column range width",
			conf: &ReportConf{
				OutputPath: "/tmp/test_range_width.xlsx",
				SheetName:  "TestSheet",
				Script:     `width = {"A:B": 25}`,
			},
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
			name: "Width via col meta",
			conf: &ReportConf{
				OutputPath: "/tmp/test_col_meta_width.xlsx",
				SheetName:  "TestSheet",
				Script:     `col = {"Name": {"width": 30}}`,
			},
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
			name: "With lookup",
			conf: &ReportConf{
				OutputPath: "/tmp/test_lookup.xlsx",
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, Generate(tc.conf, tc.rowReader))

			f, err := excelize.OpenFile(tc.conf.OutputPath)
			assert.NoError(t, err)

			rows, err := f.GetRows(tc.conf.SheetName)
			assert.NoError(t, err)

			assert.Equal(t, len(tc.expectedRows), len(rows))
			for i, expectedRow := range tc.expectedRows {
				assert.Equal(t, expectedRow, rows[i])
			}

			if tc.customValidation != nil {
				tc.customValidation(t, f)
			}

			os.Remove(tc.conf.OutputPath)
		})
	}
}
