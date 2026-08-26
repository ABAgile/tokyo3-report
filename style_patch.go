package report

import (
	"encoding/xml"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// This file patches styles and widths into an already written worksheet part by
// streaming its XML tokens. It serves worksheets that have no RowsReader, and
// the <cols> element of every worksheet. Worksheets with rows get their cell
// styles from style_stream.go instead.

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

// tailCapture records the bytes read from the worksheet part so a bail-out can
// re-emit whatever the XML decoder buffered past the last returned token.
// Capturing stops as soon as bailing out is no longer possible, so the recorded
// prefix stays bounded by the worksheet header.
type tailCapture struct {
	src     io.Reader
	buf     []byte
	stopped bool
}

func (t *tailCapture) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if !t.stopped {
		t.buf = append(t.buf, p[:n]...)
	}
	return n, err
}

func (t *tailCapture) stop() {
	t.stopped = true
	t.buf = nil
}

// selfClosed reports whether the element source text ending at the given input
// offset was written in self-closing form, i.e. its end element is synthesized
// by the decoder and consumes no input.
func (t *tailCapture) selfClosed(offset int64) bool {
	return offset >= 2 && int(offset) <= len(t.buf) && t.buf[offset-2] == '/'
}

// tail returns the captured bytes the decoder read past the given offset.
func (t *tailCapture) tail(offset int64) []byte {
	if int(offset) >= len(t.buf) {
		return nil
	}
	return t.buf[offset:]
}

func transformWorksheet(src io.Reader, dst io.Writer, compiled compiledRules) error {
	rules, err := prepareRules(compiled)
	if err != nil {
		return err
	}

	capture := &tailCapture{src: src}
	dec := xml.NewDecoder(capture)
	enc := xml.NewEncoder(dst)
	bailArmed, pendingSyntheticEnd := false, false

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
		// The encoder output matches the input byte for byte up to the current
		// offset, so the remainder of the part can be copied without parsing.
		if bailArmed && !pendingSyntheticEnd {
			if err := enc.Flush(); err != nil {
				return err
			}
			if _, err := dst.Write(capture.tail(dec.InputOffset())); err != nil {
				return err
			}
			capture.stop()
			_, err := io.Copy(dst, src)
			return err
		}

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
				bailArmed = rules.sheetDataUntouched()
				if !bailArmed {
					capture.stop() // bailing out is no longer possible
				}
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
					} else if style, ok := rowOrColumnStyle(rules.compiledRules, rowNumber, columnNumber); ok {
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
			pendingSyntheticEnd = capture.selfClosed(dec.InputOffset())

		case xml.EndElement:
			depth--
			pendingSyntheticEnd = false
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

		default:
			pendingSyntheticEnd = false
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

// plainElement reports whether an element needs no prefix or declaration
// rewriting, so it can be emitted with its attributes untouched. This is the
// case for nearly every element of a worksheet, including all cells.
func plainElement(start xml.StartElement, ns *namespaceState) bool {
	if start.Name.Space != "" && start.Name.Space != ns.main {
		return false
	}
	for _, attr := range start.Attr {
		if attr.Name.Space != "" || attr.Name.Local == "xmlns" || strings.HasPrefix(attr.Name.Local, "xmlns:") {
			return false
		}
	}
	return true
}

func normalizeStartElement(start xml.StartElement, ns *namespaceState, root bool) xml.StartElement {
	if root && ns.main == "" {
		ns.main = start.Name.Space
		if ns.main == "" {
			ns.main = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
		}
	}

	if !root && plainElement(start, ns) {
		return xml.StartElement{Name: xml.Name{Local: start.Name.Local}, Attr: start.Attr}
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
