package report

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

type benchRows struct {
	n, i, cols int
}

func (g *benchRows) Read() error { return nil }
func (g *benchRows) Headers() ([]string, error) {
	h := make([]string, g.cols)
	for i := range h {
		h[i] = fmt.Sprintf("col_%d", i)
	}
	return h, nil
}
func (g *benchRows) Values() ([]any, error) {
	v := make([]any, g.cols)
	for c := range v {
		switch c % 4 {
		case 0:
			v[c] = int64(g.i*c + 1)
		case 1:
			v[c] = fmt.Sprintf("value-%d-%d", g.i, c)
		case 2:
			v[c] = float64(g.i) * 1.5
		default:
			v[c] = time.Unix(int64(1600000000+g.i), 0).UTC()
		}
	}
	return v, nil
}
func (g *benchRows) Next() bool { g.i++; return g.i <= g.n }
func (g *benchRows) Err() error { return nil }

func benchGenerate(b *testing.B, script string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "o.xlsx")
		if err := Generate(out, WorksheetConf{SheetName: "S", Rows: &benchRows{n: 20000, cols: 12}, Script: script}); err != nil {
			b.Fatal(err)
		}
	}
}

// widths only: no row/cell rules -> sheetData needs no edits
func BenchmarkGenerateWidthsOnly(b *testing.B) {
	benchGenerate(b, `width = {"A": 20.0}`)
}

// column style + width: still no per-row edits
func BenchmarkGenerateColStyle(b *testing.B) {
	benchGenerate(b, `
style = {"A:C": "{\"alignment\":{\"horizontal\":\"center\"}}"}
width = {"A": 20.0}
`)
}

// row style: sheetData must be rewritten
func BenchmarkGenerateRowStyle(b *testing.B) {
	benchGenerate(b, `
style = {"1": "{\"font\":{\"bold\":true}}", "B2": "{\"font\":{\"italic\":true}}"}
width = {"A": 20.0}
`)
}
