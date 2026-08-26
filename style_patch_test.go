package report

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestCompileRulesPreservesRuleOrder(t *testing.T) {
	compiled, err := compileRules(worksheetRules{
		StyleRules: []styleRule{
			{Target: "A:C", StyleID: 1},
			{Target: "B:D", StyleID: 2},
			{Target: "A1:B1", StyleID: 3},
			{Target: "B1:C2", StyleID: 4},
		},
		ExplicitWidths: []widthRule{
			{Target: "A:C", Width: 20},
			{Target: "B:D", Width: 30},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := spanStyle(compiled.Cols, 2); !ok || got != 2 {
		t.Errorf("column style = %d, %v; want 2, true", got, ok)
	}
	if got, ok := spanWidth(compiled.Widths, 2); !ok || got != 30 {
		t.Errorf("column width = %v, %v; want 30, true", got, ok)
	}

	prepared := prepareRules(compiled)
	got := prepared.styledCells(1)
	want := []styledCell{{col: 1, styleID: 3}, {col: 2, styleID: 4}, {col: 3, styleID: 4}}
	if len(got) != len(want) {
		t.Fatalf("styled cells = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("styled cell %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRewriteColsCombinesStyleAndWidthRules(t *testing.T) {
	input := []rawCol{{
		XMLName: xml.Name{Local: "col"},
		Attrs: []xml.Attr{
			{Name: xml.Name{Local: "min"}, Value: "2"},
			{Name: xml.Name{Local: "max"}, Value: "2"},
		},
	}}

	cols := rewriteCols(
		input,
		[]styleSpan{{Min: 1, Max: 3, StyleID: 4}},
		[]widthSpan{{Min: 1, Max: 3, Width: 25}},
	)
	if len(cols) != 3 {
		t.Fatalf("got %d column definitions, want 3", len(cols))
	}

	for _, col := range cols {
		if got := attrString(col.Attrs, "style"); got != "4" {
			t.Errorf("column %s:%s has style %q, want 4", attrString(col.Attrs, "min"), attrString(col.Attrs, "max"), got)
		}
		if got := attrString(col.Attrs, "width"); got != "25" {
			t.Errorf("column %s:%s has width %q, want 25", attrString(col.Attrs, "min"), attrString(col.Attrs, "max"), got)
		}
		if got := attrString(col.Attrs, "customWidth"); got != "1" {
			t.Errorf("column %s:%s has customWidth %q, want 1", attrString(col.Attrs, "min"), attrString(col.Attrs, "max"), got)
		}
	}
}

func TestRewriteColsAddsAndPreservesRanges(t *testing.T) {
	input := []rawCol{{
		XMLName: xml.Name{Local: "col"},
		Attrs: []xml.Attr{
			{Name: xml.Name{Local: "min"}, Value: "1"},
			{Name: xml.Name{Local: "max"}, Value: "4"},
			{Name: xml.Name{Local: "width"}, Value: "9"},
		},
	}}

	cols := rewriteCols(input, []styleSpan{{Min: 2, Max: 3, StyleID: 5}}, []widthSpan{{Min: 6, Max: 6, Width: 30}})

	type got struct{ min, max, style, width string }
	var actual []got
	for _, col := range cols {
		actual = append(actual, got{
			attrString(col.Attrs, "min"), attrString(col.Attrs, "max"),
			attrString(col.Attrs, "style"), attrString(col.Attrs, "width"),
		})
	}
	want := []got{
		{"1", "1", "", "9"},  // untouched part of the original definition
		{"2", "3", "5", "9"}, // styled split, original width kept
		{"4", "4", "", "9"},  // untouched part of the original definition
		{"6", "6", "", "30"}, // column with no previous definition
	}
	if len(actual) != len(want) {
		t.Fatalf("got %v, want %v", actual, want)
	}
	for i := range want {
		if actual[i] != want[i] {
			t.Errorf("column %d = %v, want %v", i, actual[i], want[i])
		}
	}
}

func TestTransformWorksheetStylesBlankCells(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blank.xlsx")
	if err := Generate(path, WorksheetConf{
		SheetName: "Data",
		Rows:      NewStructRows([]any{struct{ Name string }{"Alice"}}),
		Script:    `style = {"A1:B2": "{\"font\":{\"italic\":true}}"}`,
	}); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for _, cell := range []string{"A1", "B1", "A2", "B2"} {
		styleID, err := book.GetCellStyle("Data", cell)
		if err != nil {
			t.Fatal(err)
		}
		style, err := book.GetStyle(styleID)
		if err != nil {
			t.Fatal(err)
		}
		if !style.Font.Italic {
			t.Errorf("%s is not italic", cell)
		}
	}
}

func TestTransformWorksheetCreatesStyledRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.xlsx")
	if err := Generate(path, WorksheetConf{
		SheetName: "Data",
		Script:    `style = {"B2": "{\"font\":{\"bold\":true}}"}`,
	}); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	styleID, err := book.GetCellStyle("Data", "B2")
	if err != nil {
		t.Fatal(err)
	}
	style, err := book.GetStyle(styleID)
	if err != nil {
		t.Fatal(err)
	}
	if !style.Font.Bold {
		t.Fatal("B2 is not bold")
	}
}

// styleFixture writes a two-row sheet and returns its path, the worksheet part
// path, and freshly created style IDs.
func styleFixture(t *testing.T, count int) (string, string, []int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.xlsx")
	if err := Generate(path, WorksheetConf{
		SheetName: "Data",
		Rows: NewStructRows([]any{
			struct{ Name, Note string }{"a", "b"},
		}),
	}); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sizes := []float64{8, 9, 10, 11, 12, 13}
	ids := make([]int, count)
	for i := range ids {
		if ids[i], err = book.NewStyle(&excelize.Style{Font: &excelize.Font{Size: sizes[i]}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Save(); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}

	return path, worksheetPart(t, path, "Data"), ids
}

func TestTransformWorksheetStylePrecedence(t *testing.T) {
	source, part, ids := styleFixture(t, 4)
	exact, ranged, rowStyle, colStyle := ids[0], ids[1], ids[2], ids[3]

	output := filepath.Join(filepath.Dir(source), "out.xlsx")
	if err := transformWorkbook(source, output, map[string]worksheetRules{
		"Data": {
			StyleRules: []styleRule{
				{Target: "A2", StyleID: exact},
				{Target: "A2:B3", StyleID: ranged},
				{Target: "2", StyleID: rowStyle},
				{Target: "A:B", StyleID: colStyle},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	// Row 1 has no cell or row rule, so the column rule applies. Row 2 takes
	// the exact rule in A and the range rule in B. Row 3 does not exist in the
	// source and is materialized from the range rule.
	for cell, want := range map[string]int{
		"A1": colStyle, "B1": colStyle,
		"A2": exact, "B2": ranged,
		"A3": ranged, "B3": ranged,
	} {
		got, err := book.GetCellStyle("Data", cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s style = %d, want %d", cell, got, want)
		}
	}
	rowAttrs := fmt.Sprintf(`<row r="2" s="%d" customFormat="1">`, rowStyle)
	if xml := worksheetXML(t, output, part); !strings.Contains(xml, rowAttrs) {
		t.Errorf("worksheet does not contain %s", rowAttrs)
	}
	if got, err := book.GetColStyle("Data", "A"); err != nil || got != colStyle {
		t.Errorf("column A style = %d (err %v), want %d", got, err, colStyle)
	}
}

func TestTransformWorksheetRejectsInvalidCellRule(t *testing.T) {
	source, _, ids := styleFixture(t, 1)
	output := filepath.Join(filepath.Dir(source), "out.xlsx")
	err := transformWorkbook(source, output, map[string]worksheetRules{
		"Data": {StyleRules: []styleRule{{Target: "NOPE", StyleID: ids[0]}}},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid cell reference")
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Errorf("output should not exist, stat error: %v", statErr)
	}
}

// Width-only rules never touch <sheetData>, so the transformer copies it
// verbatim instead of re-encoding every cell. The copied bytes must still
// produce a readable workbook with the widths applied.
func TestTransformWorksheetCopiesUntouchedSheetData(t *testing.T) {
	type row struct {
		Name  string
		Count int
	}
	rows := make([]any, 0, 200)
	for i := range 200 {
		rows = append(rows, row{Name: fmt.Sprintf("name <%d> & more", i), Count: i})
	}

	path := filepath.Join(t.TempDir(), "widths.xlsx")
	if err := Generate(path, WorksheetConf{
		SheetName: "Data",
		Rows:      NewStructRows(rows),
		Script:    `width = {"B": 42.0}`,
	}); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	sheetRows, err := book.GetRows("Data")
	if err != nil {
		t.Fatal(err)
	}
	if len(sheetRows) != len(rows)+1 {
		t.Fatalf("row count = %d, want %d", len(sheetRows), len(rows)+1)
	}
	if got := sheetRows[200][0]; got != "name <199> & more" {
		t.Errorf("last row name = %q", got)
	}
	if got, err := book.GetColWidth("Data", "B"); err != nil || got != 42 {
		t.Errorf("column B width = %v (err %v), want 42", got, err)
	}
}

// An empty sheet leaves a self-closing <sheetData/>, whose end element is
// synthesized by the decoder and consumes no input.
func TestTransformWorksheetCopiesEmptySheetData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.xlsx")
	if err := Generate(path, WorksheetConf{
		SheetName: "Data",
		Rows:      NewStructRows(nil),
		Script:    `width = {"A": 30.0}`,
	}); err != nil {
		t.Fatal(err)
	}
	book, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if got, err := book.GetColWidth("Data", "A"); err != nil || got != 30 {
		t.Errorf("column A width = %v (err %v), want 30", got, err)
	}
}

// worksheetPart resolves the worksheet XML part of one sheet name.
func worksheetPart(t *testing.T, path, sheet string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	paths, err := sheetPartPaths(&zr.Reader)
	if err != nil {
		t.Fatal(err)
	}
	part, ok := paths[sheet]
	if !ok {
		t.Fatalf("sheet %q not found in %s", sheet, path)
	}
	return part
}

func worksheetXML(t *testing.T, path, part string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, entry := range zr.File {
		if cleanZipPath(entry.Name) != part {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	t.Fatalf("worksheet part %s not found", part)
	return ""
}
