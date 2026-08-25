package report

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestTransformWorkbookPatchesStylesAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.xlsx")
	if err := Generate(source, WorksheetConf{
		SheetName: "Data",
		Rows:      NewStructRows([]any{struct{ Name, Note string }{"Alice", "hi"}}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o640); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	styleID, err := book.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Save(); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(dir, "output.xlsx")
	if err := transformWorkbook(source, output, map[string]patchRules{
		"Data": {
			Styles: map[string]int{"1": styleID, "B2": styleID},
			Widths: map[string]float64{"A:B": 42},
		},
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("output mode is %v, want 0640", got)
	}

	patched, err := excelize.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer patched.Close()
	if got, err := patched.GetCellStyle("Data", "B2"); err != nil || got != styleID {
		t.Errorf("B2 style = %d (err %v), want %d", got, err, styleID)
	}
	if got, err := patched.GetCellStyle("Data", "A1"); err != nil || got != styleID {
		t.Errorf("A1 style = %d (err %v), want %d", got, err, styleID)
	}
	if got, err := patched.GetColWidth("Data", "B"); err != nil || got != 42 {
		t.Errorf("column B width = %v (err %v), want 42", got, err)
	}
	if rows, err := patched.GetRows("Data"); err != nil || len(rows) != 2 {
		t.Errorf("rows = %v (err %v), want 2 rows", rows, err)
	}
}

func TestTransformWorkbookRejectsUnknownSheet(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.xlsx")
	if err := Generate(source, WorksheetConf{
		SheetName: "Data",
		Rows:      NewStructRows([]any{struct{ Name string }{"Alice"}}),
	}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output.xlsx")
	err := transformWorkbook(source, output, map[string]patchRules{
		"Missing": {Styles: map[string]int{"1": 1}},
	})
	if err == nil {
		t.Fatal("expected an error for a missing worksheet")
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Errorf("output should not exist, stat error: %v", statErr)
	}
}
