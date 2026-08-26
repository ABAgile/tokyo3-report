package report

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xuri/excelize/v2"
)

type simpleTranslator struct{}

func generate(outputPath string, worksheet WorksheetConf, rows RowsReader) error {
	worksheet.Rows = rows
	return Generate(outputPath, worksheet)
}

func (simpleTranslator) Header(key string) string {
	if key == "Name" {
		return "Full Name"
	}
	return key
}
func (simpleTranslator) Lookup(_ string, code any) string { return "" }

func TestGenerate(t *testing.T) {
	testCases := []struct {
		name         string
		outputPath   string
		worksheet    WorksheetConf
		rowReader    RowsReader
		expectedRows [][]string
	}{
		{
			name:         "Empty",
			outputPath:   "/tmp/test_empty.xlsx",
			worksheet:    WorksheetConf{SheetName: "TestSheet"},
			rowReader:    NewStructRows([]any{}),
			expectedRows: [][]string{},
		},
		{
			name:         "Nil row reader",
			outputPath:   "/tmp/test_nil_reader.xlsx",
			worksheet:    WorksheetConf{SheetName: "TestSheet"},
			rowReader:    nil,
			expectedRows: [][]string{},
		},
		{
			name:       "With translator",
			outputPath: "/tmp/test_translator.xlsx",
			worksheet: WorksheetConf{
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
			assert.NoError(t, generate(tc.outputPath, tc.worksheet, tc.rowReader))

			f, err := excelize.OpenFile(tc.outputPath)
			assert.NoError(t, err)

			rows, err := f.GetRows(tc.worksheet.SheetName)
			assert.NoError(t, err)

			assert.Equal(t, len(tc.expectedRows), len(rows))
			for i, expectedRow := range tc.expectedRows {
				assert.Equal(t, expectedRow, rows[i])
			}

			os.Remove(tc.outputPath)
		})
	}
}

func TestGenerate_PreservesOutputSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional Windows privileges")
	}

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "versioned.xlsx")
	linkPath := filepath.Join(dir, "current.xlsx")
	worksheet := WorksheetConf{
		SheetName: "Sheet",
		Rows:      NewStructRows([]any{struct{ Value string }{"old"}}),
	}
	assert.NoError(t, Generate(targetPath, worksheet))
	assert.NoError(t, os.Symlink(filepath.Base(targetPath), linkPath))

	worksheet.Rows = NewStructRows([]any{struct{ Value string }{"new"}})
	assert.NoError(t, Generate(linkPath, worksheet))

	info, err := os.Lstat(linkPath)
	assert.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	f, err := excelize.OpenFile(targetPath)
	assert.NoError(t, err)
	defer f.Close()
	rows, err := f.GetRows("Sheet")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Value"}, {"new"}}, rows)
}

func TestGenerate_MultipleWorksheets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	worksheets := []WorksheetConf{
		{
			SheetName: "First",
			Rows:      NewStructRows([]any{struct{ Name string }{"Alice"}}),
			Script:    `style = {"A1": "{\"font\":{\"bold\":true}}"}`,
		},
		{
			SheetName: "Second",
			Rows:      NewStructRows([]any{struct{ Value string }{"Ready"}}),
			Script:    `width = {"A": 25}`,
		},
	}

	assert.NoError(t, Generate(path, worksheets...))
	f, err := excelize.OpenFile(path)
	assert.NoError(t, err)
	defer f.Close()
	assert.Equal(t, []string{"First", "Second"}, f.GetSheetList())

	rows, err := f.GetRows("First")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Alice"}}, rows)
	rows, err = f.GetRows("Second")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Value"}, {"Ready"}}, rows)

	styleID, err := f.GetCellStyle("First", "A1")
	assert.NoError(t, err)
	style, err := f.GetStyle(styleID)
	assert.NoError(t, err)
	assert.True(t, style.Font.Bold)
	width, err := f.GetColWidth("Second", "A")
	assert.NoError(t, err)
	assert.Equal(t, 25.0, width)
}

func TestGenerate_MultipleWorksheetsPreservesOutputOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	valid := func(first, second string) []WorksheetConf {
		return []WorksheetConf{
			{SheetName: "First", Rows: NewStructRows([]any{struct{ Name string }{first}})},
			{SheetName: "Second", Rows: NewStructRows([]any{struct{ Name string }{second}})},
		}
	}
	assert.NoError(t, Generate(path, valid("Original first", "Original second")...))

	failed := []WorksheetConf{
		{SheetName: "First", Rows: NewStructRows([]any{struct{ Name string }{"Replacement first"}})},
		{
			SheetName: "Second",
			Rows:      NewStructRows([]any{struct{ Name string }{"Replacement second"}}),
			Script:    `col = {"Name": {"parser": "fail"}}`,
			Parsers: map[string]Parser{
				"fail": func(any) (any, error) { return nil, errors.New("invalid value") },
			},
		},
	}
	assert.Error(t, Generate(path, failed...))

	f, err := excelize.OpenFile(path)
	assert.NoError(t, err)
	defer f.Close()
	rows, err := f.GetRows("First")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Original first"}}, rows)
	rows, err = f.GetRows("Second")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Original second"}}, rows)
}

// closableRows reports whether Generate released the reader.
type closableRows struct {
	*StructRows
	closed int
}

func (r *closableRows) Close() error {
	r.closed++
	return nil
}

type readErrorRows struct {
	*StructRows
	closed int
}

func (r *readErrorRows) Read() error { return errors.New("read failed") }

func (r *readErrorRows) Close() error {
	r.closed++
	return nil
}

func TestGenerate_ClosesRowsReader(t *testing.T) {
	rows := func() *closableRows {
		return &closableRows{StructRows: NewStructRows([]any{struct{ Name string }{"Alice"}})}
	}

	success := rows()
	assert.NoError(t, Generate(filepath.Join(t.TempDir(), "report.xlsx"), WorksheetConf{
		SheetName: "Sheet1",
		Rows:      success,
	}))
	assert.Equal(t, 1, success.closed)

	// An error partway through the rows must release the reader too.
	failed := rows()
	assert.Error(t, Generate(filepath.Join(t.TempDir(), "report.xlsx"), WorksheetConf{
		SheetName: "Sheet1",
		Rows:      failed,
		Script:    `col = {"Name": {"parser": "fail"}}`,
		Parsers: map[string]Parser{
			"fail": func(any) (any, error) { return nil, errors.New("invalid value") },
		},
	}))
	assert.Equal(t, 1, failed.closed)
}

func TestGenerate_ClosesRowsReaderWhenReadFails(t *testing.T) {
	rows := &readErrorRows{StructRows: NewStructRows([]any{struct{ Name string }{"Alice"}})}

	err := Generate(filepath.Join(t.TempDir(), "report.xlsx"), WorksheetConf{
		SheetName: "Sheet1",
		Rows:      rows,
	})
	assert.EqualError(t, err, "read failed")
	assert.Equal(t, 1, rows.closed)
}

func TestGenerate_ReopenOverridesExistingSheet(t *testing.T) {
	path := "/tmp/test_reopen.xlsx"
	defer os.Remove(path)
	worksheet := WorksheetConf{SheetName: "TestSheet"}

	assert.NoError(t, generate(path, worksheet, NewStructRows([]any{
		struct{ Name string }{"Original"},
	})))
	assert.NoError(t, generate(path, worksheet, NewStructRows([]any{
		struct{ Name string }{"Overridden"},
	})))

	f, err := excelize.OpenFile(path)
	assert.NoError(t, err)
	assert.Equal(t, []string{"TestSheet"}, f.GetSheetList())

	rows, err := f.GetRows("TestSheet")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Overridden"}}, rows)
}

func TestGenerate_NilRowsPreservesExistingSheet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	worksheet := WorksheetConf{SheetName: "TestSheet"}
	assert.NoError(t, generate(path, worksheet, NewStructRows([]any{
		struct{ Name string }{"Original"},
	})))
	assert.NoError(t, Generate(path, worksheet))

	f, err := excelize.OpenFile(path)
	assert.NoError(t, err)
	defer f.Close()
	rows, err := f.GetRows("TestSheet")
	assert.NoError(t, err)
	assert.Equal(t, [][]string{{"Name"}, {"Original"}}, rows)
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
