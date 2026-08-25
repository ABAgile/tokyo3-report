package report

import (
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

// Styles of a sheet with rows are written by the stream writer, including the
// rules that reach past the data and the rows added for group breaks.
func TestStreamStyleAppliesRulesBeyondData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "styled.xlsx")
	if err := Generate(path, WorksheetConf{
		SheetName: "Data",
		Rows: NewStructRows([]any{
			struct {
				Name string
				Qty  int
			}{"a", 1},
			struct {
				Name string
				Qty  int
			}{"b", 2},
		}),
		Script: "groupFields = [\"Name\"]\nstyle = {\"A5:B6\": \"{\\\"font\\\":{\\\"size\\\":9}}\"}",
	}); err != nil {
		t.Fatal(err)
	}

	book, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()

	// Row 1 holds the headers, row 2 the first record, row 3 the group break,
	// row 4 the second record. Rows 5 and 6 hold no value, they exist only to
	// carry the style of the rule.
	rows, err := book.GetRows("Data")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("row count = %d, want 4", len(rows))
	}
	if got := rows[3][0]; got != "b" {
		t.Errorf("A4 = %q, want %q", got, "b")
	}
	want, err := book.GetCellStyle("Data", "A5")
	if err != nil {
		t.Fatal(err)
	}
	if want == 0 {
		t.Fatal("A5 has no style")
	}
	for _, cell := range []string{"A5", "B5", "A6", "B6"} {
		got, err := book.GetCellStyle("Data", cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s style = %d, want %d", cell, got, want)
		}
	}
	if got, err := book.GetCellStyle("Data", "A4"); err != nil || got != 0 {
		t.Errorf("A4 style = %d (err %v), want 0", got, err)
	}
}
