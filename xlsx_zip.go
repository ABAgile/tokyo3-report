package report

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

// This file handles the workbook container: locating worksheet parts inside the
// staged XLSX and rewriting selected parts into a new archive while every other
// part keeps its compressed bytes.

// transformWorkbook rewrites the selected worksheet parts in one ZIP pass.
// Rules are keyed by workbook sheet name; the matching worksheet part is
// resolved from the same archive reader, so the staged file is opened once.
// The worksheet XML is token-streamed; it is never accumulated in memory, and
// every other part is copied still compressed. The output keeps the input's
// file mode.
func transformWorkbook(input, output string, rules map[string]worksheetRules) error {
	if len(rules) == 0 {
		return fmt.Errorf("no worksheet rules supplied")
	}
	for sheet := range rules {
		if sheet == "" {
			return fmt.Errorf("worksheet name is required")
		}
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
	paths, err := sheetPartPaths(zr)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(zr.File))
	for _, entry := range zr.File {
		present[cleanZipPath(entry.Name)] = true
	}
	compiledTargets := make(map[string]compiledRules, len(rules))
	for sheet, spec := range rules {
		path, ok := paths[sheet]
		if !ok || !present[path] {
			return fmt.Errorf("worksheet %q not found in workbook", sheet)
		}
		compiled, err := compileRules(spec)
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

// sheetPartPaths resolves workbook sheet names to their worksheet XML parts
// through workbook.xml and its relationships file. It reuses an open archive
// reader so the caller does not have to reopen the workbook.
func sheetPartPaths(zr *zip.Reader) (map[string]string, error) {
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

func cleanZipPath(name string) string {
	return pathpkg.Clean(strings.ReplaceAll(name, "\\", "/"))
}
