package main

import (
	"fmt"
	"log"
	"time"

	report "github.com/abagile/tokyo3-report"
)

// SalesRecord is the row type for the report.
type SalesRecord struct {
	Region   string
	Product  string
	Qty      int
	Revenue  float64
	SaleDate time.Time
}

// salesTranslator localises headers and enum values.
type salesTranslator struct{}

func (salesTranslator) T(key string) string {
	headers := map[string]string{
		"Region":   "Region",
		"Product":  "Product",
		"Qty":      "Qty",
		"Revenue":  "Revenue (USD)",
		"SaleDate": "Date",
	}
	if v, ok := headers[key]; ok {
		return v
	}
	return key
}

func (salesTranslator) Label(key string, code any) string {
	return fmt.Sprintf("%v", code)
}

// script customises layout via Starlark.
//
// groupFields:  inserts a blank separator row when Region changes.
// width:        explicit column widths (overrides auto-sizing).
// style:        makes the header row bold.
// col:          formats the Revenue column as #,##0.00.
const script = `
groupFields = ["Region"]

width = {
    "A": 14,
    "B": 22,
    "C": 8,
    "D": 18,
    "E": 13,
}

style = {
    "1": '{"font":{"bold":true}}',
}

col = {
    "Revenue": {
        "style": '{"num_fmt":4}',
    },
}
`

func main() {
	rows := []any{
		SalesRecord{"North", "Widget A", 10, 2500.00, time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
		SalesRecord{"North", "Widget B", 5, 1250.00, time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)},
		SalesRecord{"South", "Widget A", 8, 2000.00, time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
		SalesRecord{"South", "Widget C", 12, 3600.00, time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC)},
		SalesRecord{"West", "Widget B", 20, 5000.00, time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)},
	}

	conf := &report.ReportConf{
		OutputPath: "sales_report.xlsx",
		SheetName:  "Q1 Sales",
		Script:     script,
		Translator: salesTranslator{},
	}

	if err := report.Generate(conf, report.NewStructRows(rows)); err != nil {
		log.Fatal(err)
	}

	log.Println("wrote sales_report.xlsx")
}
