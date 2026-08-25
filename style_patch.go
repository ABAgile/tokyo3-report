package report

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Style IDs are zero-based indexes into xl/styles.xml/cellXfs/xf.
// The IDs must already exist in the workbook's styles.xml.
type styleSpan struct {
	Min, Max int
	StyleID  int
}

type widthSpan struct {
	Min, Max int
	Width    float64
}

type cellSpan struct {
	FromCol, ToCol int
	FromRow, ToRow int
	StyleID        int
}

// patchRules mirrors the worksheet-level style and width metadata after
// workbook styles have been assigned, but before targets are compiled into
// numeric XML ranges.
type patchRules struct {
	Styles      map[string]int     // target -> workbook style ID
	Widths      map[string]float64 // column target -> explicit width
	FieldWidths []int              // auto-sized content widths by zero-based column
}

func (r patchRules) empty() bool {
	return len(r.Styles) == 0 && len(r.Widths) == 0 && len(r.FieldWidths) == 0
}

// compiledPatchRules contains the numeric ranges consumed by the worksheet
// XML transformer.
type compiledPatchRules struct {
	// Exact cell rules always win.
	Cells      map[string]int // e.g. "B4": 7
	CellRanges []cellSpan
	Rows       []styleSpan // Excel row numbers, inclusive
	Cols       []styleSpan // Excel column numbers, inclusive; A=1
	Widths     []widthSpan // Excel column widths, inclusive; A=1
}

const (
	styleTargetCol = iota
	styleTargetRow
	styleTargetCell
)

var colRe = regexp.MustCompile(`^[A-Z]+(:[A-Z]+)?$`)

// These types are used only for the small <cols> element. The worksheet's
// <sheetData> is never accumulated in memory.
type rawCols struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Cols    []rawCol   `xml:"col"`
}

type rawCol struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
}

type styledCell struct {
	col     int
	styleID int
}

func styleTargetKind(target string) int {
	if colRe.MatchString(target) {
		return styleTargetCol
	}
	if _, err := strconv.Atoi(target); err == nil {
		return styleTargetRow
	}
	return styleTargetCell
}

func boundedCellWidth(width float64) float64 {
	return max(min(width, 100.0), 10.0)
}

func normalizeTarget(target string) string {
	return strings.TrimSpace(strings.ToUpper(target))
}

// colRange splits a colon-separated column range (e.g. "A:C") into its two
// endpoints. A single column name is returned as both endpoints unchanged.
func colRange(target string) (from, to string) {
	if f, t, ok := strings.Cut(target, ":"); ok {
		return f, t
	}
	return target, target
}

func columnSpan(target string) (from, to int, err error) {
	fromName, toName := colRange(target)
	from, err = excelize.ColumnNameToNumber(fromName)
	if err != nil {
		return 0, 0, err
	}
	to, err = excelize.ColumnNameToNumber(toName)
	if err != nil {
		return 0, 0, err
	}
	if from > to {
		from, to = to, from
	}
	return from, to, nil
}

func compilePatchRules(spec patchRules) (compiledPatchRules, error) {
	rules := compiledPatchRules{
		Cells: make(map[string]int),
	}
	for target, styleID := range spec.Styles {
		target = normalizeTarget(target)
		switch styleTargetKind(target) {
		case styleTargetCol:
			fromCol, toCol, err := columnSpan(target)
			if err != nil {
				return compiledPatchRules{}, err
			}
			rules.Cols = append(rules.Cols, styleSpan{Min: fromCol, Max: toCol, StyleID: styleID})
		case styleTargetRow:
			row, err := strconv.Atoi(target)
			if err != nil || row < 1 || row > excelize.TotalRows {
				return compiledPatchRules{}, fmt.Errorf("invalid style row %q", target)
			}
			rules.Rows = append(rules.Rows, styleSpan{Min: row, Max: row, StyleID: styleID})
		case styleTargetCell:
			from, to := colRange(target)
			fromCol, fromRow, err := excelize.CellNameToCoordinates(from)
			if err != nil {
				return compiledPatchRules{}, err
			}
			toCol, toRow, err := excelize.CellNameToCoordinates(to)
			if err != nil {
				return compiledPatchRules{}, err
			}
			if fromCol > toCol {
				fromCol, toCol = toCol, fromCol
			}
			if fromRow > toRow {
				fromRow, toRow = toRow, fromRow
			}
			if fromCol == toCol && fromRow == toRow {
				rules.Cells[normalCellRef(from)] = styleID
			} else {
				rules.CellRanges = append(rules.CellRanges, cellSpan{
					FromCol: fromCol,
					ToCol:   toCol,
					FromRow: fromRow,
					ToRow:   toRow,
					StyleID: styleID,
				})
			}
		}
	}

	// Auto-sized widths are the baseline; explicit width metadata is appended
	// afterward so it overrides the automatic value.
	for colIdx, width := range spec.FieldWidths {
		if _, err := excelize.ColumnNumberToName(colIdx + 1); err != nil {
			return compiledPatchRules{}, err
		}
		rules.Widths = append(rules.Widths, widthSpan{
			Min:   colIdx + 1,
			Max:   colIdx + 1,
			Width: boundedCellWidth(float64(width) * 1.123),
		})
	}
	for target, width := range spec.Widths {
		target = normalizeTarget(target)
		if !colRe.MatchString(target) {
			continue
		}
		fromCol, toCol, err := columnSpan(target)
		if err != nil {
			return compiledPatchRules{}, err
		}
		rules.Widths = append(rules.Widths, widthSpan{
			Min:   fromCol,
			Max:   toCol,
			Width: boundedCellWidth(width),
		})
	}
	return rules, nil
}

// preparedRules indexes the rules of one worksheet so the streaming pass never
// parses a cell reference or scans the exact-cell rules once per row.
type preparedRules struct {
	compiledPatchRules
	cellRows  map[int][]styledCell // exact cell styles per row, ascending column
	cellRowNo []int                // sorted rows present in cellRows
}

func prepareRules(rules compiledPatchRules) (preparedRules, error) {
	prepared := preparedRules{compiledPatchRules: rules}
	if len(rules.Cells) == 0 {
		return prepared, nil
	}
	prepared.cellRows = make(map[int][]styledCell, len(rules.Cells))
	for ref, styleID := range rules.Cells {
		col, row, err := excelize.CellNameToCoordinates(normalCellRef(ref))
		if err != nil {
			return preparedRules{}, fmt.Errorf("invalid cell reference %q: %w", ref, err)
		}
		prepared.cellRows[row] = append(prepared.cellRows[row], styledCell{col: col, styleID: styleID})
	}
	for row, cells := range prepared.cellRows {
		slices.SortFunc(cells, func(a, b styledCell) int { return a.col - b.col })
		prepared.cellRowNo = append(prepared.cellRowNo, row)
	}
	sort.Ints(prepared.cellRowNo)
	return prepared, nil
}

// encoding/xml resolves namespace prefixes to namespace URLs while decoding.
// The normalizer below puts prefixes back into the serialized XML. Without
// this step, a token-for-token XML round trip can produce duplicate xmlns
// declarations or invalid generated prefixes.
type namespaceState struct {
	main     string
	prefixes map[string]string
	declared map[string]bool
	next     int
}

func newNamespaceState() *namespaceState {
	return &namespaceState{
		prefixes: map[string]string{
			"http://schemas.openxmlformats.org/officeDocument/2006/relationships": "r",
			"http://schemas.openxmlformats.org/markup-compatibility/2006":         "mc",
			"http://schemas.microsoft.com/office/spreadsheetml/2009/9/ac":         "x14ac",
		},
		declared: make(map[string]bool),
	}
}

// transformWorkbook rewrites the selected worksheet parts in one ZIP pass.
// Rules are keyed by worksheet part path, e.g. xl/worksheets/sheet1.xml.
// The worksheet XML is token-streamed; it is never accumulated in memory, and
// every other part is copied still compressed. The output keeps the input's
// file mode.
func transformWorkbook(input, output string, rules map[string]patchRules) error {
	if len(rules) == 0 {
		return fmt.Errorf("no worksheet rules supplied")
	}

	targets := make(map[string]patchRules, len(rules))
	for path, rule := range rules {
		if path == "" {
			return fmt.Errorf("worksheet part path is required")
		}
		targets[cleanZipPath(path)] = rule
	}

	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()

	stat, err := in.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(in, stat.Size())
	if err != nil {
		return err
	}

	// Validate every target before writing anything, so a bad rule fails
	// before the output is built.
	present := make(map[string]bool, len(zr.File))
	for _, entry := range zr.File {
		present[cleanZipPath(entry.Name)] = true
	}
	for path := range targets {
		if !present[path] {
			return fmt.Errorf("worksheet part not found: %s", path)
		}
	}
	compiledTargets := make(map[string]compiledPatchRules, len(targets))
	for path, spec := range targets {
		compiled, err := compilePatchRules(spec)
		if err != nil {
			return err
		}
		compiledTargets[path] = compiled
	}

	tmp, err := os.CreateTemp(filepath.Dir(output), ".xlsx-style-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(tmpName)
		}
	}()

	zw := zip.NewWriter(tmp)
	for _, entry := range zr.File {
		path := cleanZipPath(entry.Name)
		rule, patched := compiledTargets[path]
		if !patched {
			// Untouched parts keep their compressed bytes: no inflate,
			// no deflate, and no buffering of the part contents.
			if err := zw.Copy(entry); err != nil {
				return err
			}
			continue
		}

		header := entry.FileHeader
		writer, err := zw.CreateHeader(&header)
		if err != nil {
			return err
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		err = transformWorksheet(rc, writer, rule)
		closeErr := rc.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}

	if err := zw.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, stat.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(tmpName, output); err != nil {
		return err
	}
	ok = true
	return nil
}

// worksheetPaths resolves workbook sheet names to their worksheet XML parts
// through workbook.xml and its relationships file.
func worksheetPaths(filename string) (map[string]string, error) {
	in, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	stat, err := in.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(in, stat.Size())
	if err != nil {
		return nil, err
	}

	var workbook struct {
		Sheets struct {
			Sheet []struct {
				Name  string `xml:"name,attr"`
				RelID string `xml:"id,attr"`
			} `xml:"sheet"`
		} `xml:"sheets"`
	}
	var rels struct {
		Relationships []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	for _, entry := range zr.File {
		switch cleanZipPath(entry.Name) {
		case "xl/workbook.xml":
			if err := decodeZipXML(entry, &workbook); err != nil {
				return nil, err
			}
		case "xl/_rels/workbook.xml.rels":
			if err := decodeZipXML(entry, &rels); err != nil {
				return nil, err
			}
		}
	}

	relTargets := make(map[string]string, len(rels.Relationships))
	for _, rel := range rels.Relationships {
		target := strings.ReplaceAll(rel.Target, "\\", "/")
		if trimmed, ok := strings.CutPrefix(target, "/"); ok {
			target = trimmed
		} else if !strings.HasPrefix(target, "xl/") {
			target = pathpkg.Join("xl", target)
		}
		relTargets[rel.ID] = cleanZipPath(target)
	}

	paths := make(map[string]string, len(workbook.Sheets.Sheet))
	for _, sheet := range workbook.Sheets.Sheet {
		if path, ok := relTargets[sheet.RelID]; ok {
			paths[sheet.Name] = path
		}
	}
	return paths, nil
}

func decodeZipXML(entry *zip.File, dst any) error {
	rc, err := entry.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return xml.NewDecoder(rc).Decode(dst)
}

func transformWorksheet(src io.Reader, dst io.Writer, patch compiledPatchRules) error {
	rules, err := prepareRules(patch)
	if err != nil {
		return err
	}

	dec := xml.NewDecoder(src)
	enc := xml.NewEncoder(dst)

	rowNumber := 0
	columnNumber := 0
	sawCols := false
	ns := newNamespaceState()
	rootSeen := false
	depth := 0
	sheetDataDepth := -1
	lastWrittenRow := 0
	rowDepth := -1
	pending := []styledCell(nil)
	pendingIndex := 0

	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		isRoot := false
		switch t := token.(type) {
		case xml.StartElement:
			isRoot = !rootSeen
			rootSeen = true
			directRowChild := rowDepth >= 0 && depth == rowDepth+1
			switch t.Name.Local {
			case "cols":
				var cols rawCols
				if err := dec.DecodeElement(&cols, &t); err != nil {
					return err
				}
				cols.Cols = rewriteCols(cols.Cols, rules.Cols, rules.Widths)
				if err := encodeCols(enc, cols, ns); err != nil {
					return err
				}
				sawCols = true
				continue

			case "sheetData":
				sheetDataDepth = depth
				lastWrittenRow = 0
				// <cols> must occur before <sheetData>. Add it when the source
				// worksheet has no column definitions at all.
				if !sawCols && (len(rules.Cols) > 0 || len(rules.Widths) > 0) {
					cols := rawCols{XMLName: xml.Name{Space: t.Name.Space, Local: "cols"}}
					cols.Cols = rewriteCols(nil, rules.Cols, rules.Widths)
					if err := encodeCols(enc, cols, ns); err != nil {
						return err
					}
				}

			case "row":
				// Cell-range styles need explicit cells for blank positions, just
				// as Excelize's SetCellStyle does in normal mode.
				rowNumber = attrInt(t.Attr, "r")
				if rowNumber == 0 {
					rowNumber++
				}
				if sheetDataDepth >= 0 && depth == sheetDataDepth+1 {
					for {
						styledRow, ok := rules.nextStyledRow(lastWrittenRow)
						if !ok || styledRow >= rowNumber {
							break
						}
						if err := writeStyledRow(enc, ns, rules, styledRow); err != nil {
							return err
						}
						lastWrittenRow = styledRow
					}
				}
				if style, ok := spanStyle(rules.Rows, rowNumber); ok {
					setRowStyleAttr(&t.Attr, style)
				}
				columnNumber = 0
				pending = rules.styledCells(rowNumber)
				pendingIndex = 0
				rowDepth = depth

			case "c":
				if directRowChild {
					cellRef := attrString(t.Attr, "r")
					if cellRef != "" {
						columnNumber = columnFromRef(cellRef)
						if columnNumber == 0 {
							columnNumber++
						}
					} else {
						columnNumber++
					}

					if err := writeCellsBefore(enc, ns, rowNumber, pending, &pendingIndex, columnNumber); err != nil {
						return err
					}
					// Exact cell and cell-range styles are already resolved
					// in pending; only the row and column fallback is left.
					if pendingIndex < len(pending) && pending[pendingIndex].col == columnNumber {
						setStyleAttr(&t.Attr, "s", pending[pendingIndex].styleID)
						pendingIndex++
					} else if style, ok := rowOrColStyle(rules.compiledPatchRules, rowNumber, columnNumber); ok {
						setStyleAttr(&t.Attr, "s", style)
					}
				}

			default:
				if directRowChild {
					if err := writeCellsBefore(enc, ns, rowNumber, pending, &pendingIndex, excelize.MaxColumns+1); err != nil {
						return err
					}
				}
			}
			token = t
			depth++

		case xml.EndElement:
			depth--
			if rowDepth >= 0 && t.Name.Local == "row" && depth == rowDepth {
				if err := writeCellsBefore(enc, ns, rowNumber, pending, &pendingIndex, excelize.MaxColumns+1); err != nil {
					return err
				}
				lastWrittenRow = rowNumber
				rowDepth = -1
			}
			if sheetDataDepth >= 0 && t.Name.Local == "sheetData" && depth == sheetDataDepth {
				for {
					styledRow, ok := rules.nextStyledRow(lastWrittenRow)
					if !ok {
						break
					}
					if err := writeStyledRow(enc, ns, rules, styledRow); err != nil {
						return err
					}
					lastWrittenRow = styledRow
				}
				sheetDataDepth = -1
			}
		}

		switch token := token.(type) {
		case xml.StartElement:
			token = normalizeStartElement(token, ns, isRoot)
			if err := enc.EncodeToken(token); err != nil {
				return err
			}
		case xml.EndElement:
			token.Name = normalizeName(token.Name, ns)
			if err := enc.EncodeToken(token); err != nil {
				return err
			}
		default:
			if err := enc.EncodeToken(token); err != nil {
				return err
			}
		}
	}
	return enc.Flush()
}

// nextStyledRow finds the first row after the given one that must be
// materialized because a row or cell style targets it.
func (p preparedRules) nextStyledRow(after int) (int, bool) {
	if after >= excelize.TotalRows {
		return 0, false
	}
	next := excelize.TotalRows + 1
	consider := func(row int) {
		if row > after && row < next && row <= excelize.TotalRows {
			next = row
		}
	}
	for _, span := range p.Rows {
		row := max(span.Min, after+1)
		if row <= span.Max {
			consider(row)
		}
	}
	for _, span := range p.CellRanges {
		row := max(span.FromRow, after+1)
		if row <= span.ToRow {
			consider(row)
		}
	}
	if i := sort.SearchInts(p.cellRowNo, after+1); i < len(p.cellRowNo) {
		consider(p.cellRowNo[i])
	}
	if next > excelize.TotalRows {
		return 0, false
	}
	return next, true
}

func writeStyledRow(enc *xml.Encoder, ns *namespaceState, rules preparedRules, row int) error {
	attrs := []xml.Attr{{Name: xml.Name{Local: "r"}, Value: strconv.Itoa(row)}}
	if style, ok := spanStyle(rules.Rows, row); ok {
		setRowStyleAttr(&attrs, style)
	}
	start := normalizeStartElement(xml.StartElement{
		Name: xml.Name{Local: "row"},
		Attr: attrs,
	}, ns, false)
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	cells := rules.styledCells(row)
	index := 0
	if err := writeCellsBefore(enc, ns, row, cells, &index, excelize.MaxColumns+1); err != nil {
		return err
	}
	return enc.EncodeToken(xml.EndElement{Name: normalizeName(start.End().Name, ns)})
}

// styledCells lists the styles a row needs from exact cell and cell-range
// rules, in ascending column order. It is the single place that resolves their
// precedence: exact cells win over ranges, and later ranges win over earlier
// ones.
func (p preparedRules) styledCells(row int) []styledCell {
	exact := p.cellRows[row]
	ranged := false
	for _, rule := range p.CellRanges {
		if rule.FromRow <= row && row <= rule.ToRow {
			ranged = true
			break
		}
	}
	if !ranged {
		return exact
	}

	styles := make(map[int]int)
	for _, rule := range p.CellRanges {
		if rule.FromRow > row || row > rule.ToRow {
			continue
		}
		for col := rule.FromCol; col <= rule.ToCol; col++ {
			styles[col] = rule.StyleID
		}
	}
	for _, cell := range exact {
		styles[cell.col] = cell.styleID
	}
	cells := make([]styledCell, 0, len(styles))
	for _, col := range slices.Sorted(maps.Keys(styles)) {
		cells = append(cells, styledCell{col: col, styleID: styles[col]})
	}
	return cells
}

func writeCellsBefore(enc *xml.Encoder, ns *namespaceState, row int, cells []styledCell, index *int, beforeCol int) error {
	for *index < len(cells) && cells[*index].col < beforeCol {
		cell := cells[*index]
		ref, err := excelize.CoordinatesToCellName(cell.col, row)
		if err != nil {
			return err
		}
		start := normalizeStartElement(xml.StartElement{
			Name: xml.Name{Local: "c"},
			Attr: []xml.Attr{
				{Name: xml.Name{Local: "r"}, Value: ref},
				{Name: xml.Name{Local: "s"}, Value: strconv.Itoa(cell.styleID)},
			},
		}, ns, false)
		if err := enc.EncodeToken(start); err != nil {
			return err
		}
		if err := enc.EncodeToken(xml.EndElement{Name: normalizeName(start.End().Name, ns)}); err != nil {
			return err
		}
		*index = *index + 1
	}
	return nil
}

func normalizeStartElement(start xml.StartElement, ns *namespaceState, root bool) xml.StartElement {
	if root && ns.main == "" {
		ns.main = start.Name.Space
		if ns.main == "" {
			ns.main = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
		}
	}

	name := normalizeName(start.Name, ns)
	attrs := make([]xml.Attr, 0, len(start.Attr)+1)
	if root {
		for _, attr := range start.Attr {
			if attr.Name.Local == "xmlns" {
				attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: attr.Value})
				ns.declared[""] = true
				continue
			}
			if attr.Name.Space == "xmlns" {
				attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns:" + attr.Name.Local}, Value: attr.Value})
				ns.prefixes[attr.Value] = attr.Name.Local
				ns.declared[attr.Name.Local] = true
			}
		}
		if !ns.declared[""] {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: ns.main})
			ns.declared[""] = true
		}
	}
	for _, attr := range start.Attr {
		if attr.Name.Local == "xmlns" || attr.Name.Space == "xmlns" || strings.HasPrefix(attr.Name.Local, "xmlns:") {
			continue
		}
		attrName := attr.Name
		if attrName.Space != "" {
			prefix := ns.prefix(attrName.Space)
			attrName = xml.Name{Local: prefix + ":" + attrName.Local}
			if !ns.declared[prefix] {
				attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: attr.Name.Space})
				ns.declared[prefix] = true
			}
		} else {
			attrName.Space = ""
		}
		attrs = append(attrs, xml.Attr{Name: attrName, Value: attr.Value})
	}
	return xml.StartElement{Name: name, Attr: attrs}
}

func normalizeName(name xml.Name, ns *namespaceState) xml.Name {
	if name.Space == "" || name.Space == ns.main {
		return xml.Name{Local: name.Local}
	}
	prefix := ns.prefix(name.Space)
	return xml.Name{Local: prefix + ":" + name.Local}
}

func (ns *namespaceState) prefix(namespace string) string {
	if prefix, ok := ns.prefixes[namespace]; ok {
		return prefix
	}
	for {
		ns.next++
		prefix := "ns" + strconv.Itoa(ns.next)
		used := false
		for _, existing := range ns.prefixes {
			if existing == prefix {
				used = true
				break
			}
		}
		if !used {
			ns.prefixes[namespace] = prefix
			return prefix
		}
	}
}

// rowOrColStyle is the fallback for cells that no exact cell or cell-range
// rule targets. Row rules win over column rules, as in Excel.
func rowOrColStyle(rules compiledPatchRules, row, col int) (int, bool) {
	if id, ok := spanStyle(rules.Rows, row); ok {
		return id, true
	}
	return spanStyle(rules.Cols, col)
}

func spanStyle(spans []styleSpan, value int) (int, bool) {
	for _, span := range slices.Backward(spans) { // last rule wins
		if span.Min <= value && value <= span.Max {
			return span.StyleID, true
		}
	}
	return 0, false
}

// rewriteCols splits the existing <col> definitions at every rule boundary so
// each emitted range carries at most one style and width, and adds definitions
// for targeted columns that had no <col> entry at all.
func rewriteCols(input []rawCol, styleRules []styleSpan, widthRules []widthSpan) []rawCol {
	if len(styleRules) == 0 && len(widthRules) == 0 {
		return input
	}

	var output, existing []rawCol
	points := make([]int, 0, 2*(len(input)+len(styleRules)+len(widthRules)))
	for _, col := range input {
		colMin, colMax := attrInt(col.Attrs, "min"), attrInt(col.Attrs, "max")
		if colMin == 0 || colMax == 0 || colMax < colMin {
			output = append(output, col) // keep unusable definitions as they are
			continue
		}
		existing = append(existing, col)
		points = append(points, colMin, colMax+1)
	}
	for _, rule := range styleRules {
		points = append(points, rule.Min, rule.Max+1)
	}
	for _, rule := range widthRules {
		points = append(points, rule.Min, rule.Max+1)
	}
	sort.Ints(points)
	points = uniqueInts(points)

	for i := 0; i+1 < len(points); i++ {
		lo, hi := points[i], points[i+1]-1
		if lo > hi {
			continue
		}
		style, hasStyle := spanStyle(styleRules, lo)
		width, hasWidth := spanWidth(widthRules, lo)
		source, covered := coveringCol(existing, lo)
		if !covered && !hasStyle && !hasWidth {
			continue
		}
		col := rawCol{XMLName: xml.Name{Local: "col"}}
		if covered {
			col = rawCol{XMLName: source.XMLName, Attrs: slices.Clone(source.Attrs)}
		}
		setAttr(&col.Attrs, "min", strconv.Itoa(lo))
		setAttr(&col.Attrs, "max", strconv.Itoa(hi))
		if hasStyle {
			setStyleAttr(&col.Attrs, "style", style)
		}
		if hasWidth {
			setAttr(&col.Attrs, "width", strconv.FormatFloat(width, 'f', -1, 64))
			setAttr(&col.Attrs, "customWidth", "1")
		}
		output = append(output, col)
	}
	return output
}

func spanWidth(spans []widthSpan, value int) (float64, bool) {
	for _, span := range slices.Backward(spans) {
		if span.Min <= value && value <= span.Max {
			return span.Width, true
		}
	}
	return 0, false
}

func coveringCol(cols []rawCol, column int) (rawCol, bool) {
	for _, col := range cols {
		if attrInt(col.Attrs, "min") <= column && column <= attrInt(col.Attrs, "max") {
			return col, true
		}
	}
	return rawCol{}, false
}

func encodeCols(enc *xml.Encoder, cols rawCols, ns *namespaceState) error {
	name := cols.XMLName
	if name.Local == "" {
		name.Local = "cols"
	}
	start := normalizeStartElement(xml.StartElement{Name: name, Attr: cols.Attrs}, ns, false)
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, col := range cols.Cols {
		colName := col.XMLName
		if colName.Local == "" {
			colName.Local = "col"
		}
		colStart := normalizeStartElement(xml.StartElement{Name: colName, Attr: col.Attrs}, ns, false)
		if err := enc.EncodeToken(colStart); err != nil {
			return err
		}
		if err := enc.EncodeToken(xml.EndElement{Name: normalizeName(colStart.End().Name, ns)}); err != nil {
			return err
		}
	}
	return enc.EncodeToken(xml.EndElement{Name: normalizeName(start.End().Name, ns)})
}

func setStyleAttr(attrs *[]xml.Attr, name string, style int) {
	if style == 0 {
		removeAttr(attrs, name)
		return
	}
	setAttr(attrs, name, strconv.Itoa(style))
}

func setRowStyleAttr(attrs *[]xml.Attr, style int) {
	setStyleAttr(attrs, "s", style)
	if style == 0 {
		removeAttr(attrs, "customFormat")
		return
	}
	setAttr(attrs, "customFormat", "1")
}

func setAttr(attrs *[]xml.Attr, name, value string) {
	for i := range *attrs {
		if (*attrs)[i].Name.Local == name {
			(*attrs)[i].Value = value
			return
		}
	}
	*attrs = append(*attrs, xml.Attr{Name: xml.Name{Local: name}, Value: value})
}

func removeAttr(attrs *[]xml.Attr, name string) {
	out := (*attrs)[:0]
	for _, attr := range *attrs {
		if attr.Name.Local != name {
			out = append(out, attr)
		}
	}
	*attrs = out
}

func attrString(attrs []xml.Attr, name string) string {
	for _, attr := range attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func attrInt(attrs []xml.Attr, name string) int {
	value, _ := strconv.Atoi(attrString(attrs, name))
	return value
}

func columnFromRef(ref string) int {
	ref = strings.TrimPrefix(strings.ToUpper(ref), "$")
	cut := strings.IndexFunc(ref, func(r rune) bool { return r >= '0' && r <= '9' })
	if cut <= 0 {
		return 0
	}
	letters := strings.Trim(ref[:cut], "$")
	result := 0
	for _, r := range letters {
		if r < 'A' || r > 'Z' {
			return 0
		}
		result = result*26 + int(r-'A'+1)
	}
	return result
}

func normalCellRef(ref string) string {
	return strings.ToUpper(strings.ReplaceAll(ref, "$", ""))
}

func cleanZipPath(name string) string {
	return pathpkg.Clean(strings.ReplaceAll(name, "\\", "/"))
}

func uniqueInts(values []int) []int {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
