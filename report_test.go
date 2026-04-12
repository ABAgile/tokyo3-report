package report

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xuri/excelize/v2"
)

type simpleTranslator struct{}

func (simpleTranslator) T(key string) string {
	if key == "Name" {
		return "Full Name"
	}
	return key
}
func (simpleTranslator) Label(_ string, code any) string { return "" }

func TestGenerate(t *testing.T) {
	testCases := []struct {
		name         string
		conf         *ReportConf
		rowReader    RowsReader
		expectedRows [][]string
	}{
		{
			name: "Empty",
			conf: &ReportConf{
				OutputPath: "/tmp/test_empty.xlsx",
				SheetName:  "TestSheet",
			},
			rowReader:    NewStructRows([]any{}),
			expectedRows: [][]string{},
		},
		{
			name: "Nil row reader",
			conf: &ReportConf{
				OutputPath: "/tmp/test_nil_reader.xlsx",
				SheetName:  "TestSheet",
			},
			rowReader:    nil,
			expectedRows: [][]string{},
		},
		{
			name: "With translator",
			conf: &ReportConf{
				OutputPath: "/tmp/test_translator.xlsx",
				SheetName:  "TestSheet",
				Translator: simpleTranslator{},
			},
			rowReader: NewStructRows([]any{
				struct{ Name, Code string }{"Alice", "X1"},
			}),
			expectedRows: [][]string{
				{"Full Name", "Code"},
				{"Alice", "X1"},
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

			os.Remove(tc.conf.OutputPath)
		})
	}
}

func TestGenerate_ReopenOverridesExistingSheet(t *testing.T) {
	path := "/tmp/test_reopen.xlsx"
	defer os.Remove(path)
	conf := &ReportConf{OutputPath: path, SheetName: "TestSheet"}

	assert.NoError(t, Generate(conf, NewStructRows([]any{
		struct{ Name string }{"Original"},
	})))
	assert.NoError(t, Generate(conf, NewStructRows([]any{
		struct{ Name string }{"Overridden"},
	})))

	f, err := excelize.OpenFile(path)
	assert.NoError(t, err)
	assert.Equal(t, []string{"TestSheet"}, f.GetSheetList())

	rows, err := f.GetRows("TestSheet")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Overridden"}}, rows)
}

func TestCalcCellWidth(t *testing.T) {
	testCases := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"hello", 5},
		{"你好", 4},   // 2 CJK × 2
		{"hi你好", 6}, // 2 ASCII + 2 CJK × 2
	}
	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, calcCellWidth(tc.input))
		})
	}
}

func TestBoundedCellWidth(t *testing.T) {
	testCases := []struct {
		input    float64
		expected float64
	}{
		{50.0, 50.0},
		{5.0, 10.0},    // below min → clamped to 10
		{150.0, 100.0}, // above max → clamped to 100
		{10.0, 10.0},   // at min
		{100.0, 100.0}, // at max
	}
	for _, tc := range testCases {
		assert.Equal(t, tc.expected, boundedCellWidth(tc.input))
	}
}

func TestColRange(t *testing.T) {
	testCases := []struct {
		input string
		from  string
		to    string
	}{
		{"A", "A", "A"},
		{"A:C", "A", "C"},
		{"AA:ZZ", "AA", "ZZ"},
	}
	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			from, to := colRange(tc.input)
			assert.Equal(t, tc.from, from)
			assert.Equal(t, tc.to, to)
		})
	}
}
