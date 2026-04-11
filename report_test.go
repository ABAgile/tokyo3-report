package report_test

import (
	"os"
	"testing"
	"time"

	"github.com/abagile/tokyo3-report"
	"github.com/stretchr/testify/assert"
	"github.com/xuri/excelize/v2"
)

func TestGenerate(t *testing.T) {
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
		conf             *report.ReportConf
		rowReader        report.RowsReader
		expectedRows     [][]string
		customValidation func(t *testing.T, f *excelize.File)
	}{
		{
			name: "Empty",
			conf: &report.ReportConf{
				OutputPath: "/tmp/test_empty.xlsx",
				SheetName:  "TestSheet",
			},
			rowReader:    report.NewStructRows([]any{}),
			expectedRows: [][]string{},
		},
		{
			name: "With Grouping",
			conf: &report.ReportConf{
				OutputPath: "/tmp/test_group.xlsx",
				SheetName:  "TestSheet",
				Script:     "groupFields = [\"Group\"]",
			},
			rowReader: report.NewStructRows([]any{
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
			conf: &report.ReportConf{
				OutputPath: "/tmp/test_script.xlsx",
				SheetName:  "TestSheet",
				Script: `
width = {"A": 50}
style = {"C": "{\"font\":{\"bold\":true}}"}
col = {"Age": {"style": "{\"font\":{\"italic\":true}}"}}
`,
			},
			rowReader: report.NewStructRows([]any{
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, report.Generate(tc.conf, tc.rowReader))

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
