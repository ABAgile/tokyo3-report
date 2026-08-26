package report

import (
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type scriptStyle struct {
	Target    string
	StyleJSON string
}

type scriptMeta struct {
	col         map[string]map[string]any
	parsers     []colParser
	styles      []scriptStyle
	widths      []widthRule
	groupFields []string
}

type colParser struct {
	col int
	fn  Parser
}

// setParser registers the parser of one zero-based column, replacing any
// parser already registered for it.
func (m *scriptMeta) setParser(col int, fn Parser) {
	for i := range m.parsers {
		if m.parsers[i].col == col {
			m.parsers[i].fn = fn
			return
		}
	}
	m.parsers = append(m.parsers, colParser{col: col, fn: fn})
}

func (s *worksheetState) runScript() error {
	meta := scriptMeta{
		col: make(map[string]map[string]any),
	}

	if s.Script != "" {
		globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, &starlark.Thread{}, "script.star", s.Script, nil)
		if err != nil {
			return err
		}
		parseColGlobal(globals, &meta)
		parseStyleGlobal(globals, &meta)
		parseWidthGlobal(globals, &meta)
		parseGroupFieldsGlobal(globals, &meta)
	}

	s.meta = &meta
	return nil
}

// forEachStringKey visits the entries of a global dict whose keys are strings,
// skipping the global entirely when it is absent or not a dict.
func forEachStringKey(globals starlark.StringDict, key string, fn func(string, starlark.Value)) {
	dict, _ := globals[key].(*starlark.Dict)
	if dict == nil {
		return
	}
	for _, item := range dict.Items() {
		if name, ok := starlark.AsString(item[0]); ok {
			fn(name, item[1])
		}
	}
}

func parseColGlobal(globals starlark.StringDict, meta *scriptMeta) {
	forEachStringKey(globals, "col", func(colName string, value starlark.Value) {
		if entryDict, ok := value.(*starlark.Dict); ok {
			meta.col[colName] = starlarkDictToMap(entryDict)
		}
	})
}

func starlarkDictToMap(dict *starlark.Dict) map[string]any {
	entry := make(map[string]any)
	for _, kv := range dict.Items() {
		k, ok := starlark.AsString(kv[0])
		if !ok {
			continue
		}
		switch v := kv[1].(type) {
		case starlark.String:
			entry[k] = string(v)
		case starlark.Bool:
			entry[k] = bool(v)
		default:
			if f, ok := starlark.AsFloat(kv[1]); ok {
				entry[k] = f
			}
		}
	}
	return entry
}

func parseStyleGlobal(globals starlark.StringDict, meta *scriptMeta) {
	forEachStringKey(globals, "style", func(target string, value starlark.Value) {
		if styleJSON, ok := starlark.AsString(value); ok {
			meta.styles = append(meta.styles, scriptStyle{Target: target, StyleJSON: styleJSON})
		}
	})
}

func parseWidthGlobal(globals starlark.StringDict, meta *scriptMeta) {
	forEachStringKey(globals, "width", func(target string, value starlark.Value) {
		if f, ok := starlark.AsFloat(value); ok {
			meta.widths = append(meta.widths, widthRule{Target: target, Width: f})
		}
	})
}

func parseGroupFieldsGlobal(globals starlark.StringDict, meta *scriptMeta) {
	val, ok := globals["groupFields"]
	if !ok {
		return
	}
	list, ok := val.(*starlark.List)
	if !ok {
		return
	}
	for i := range list.Len() {
		if s, ok := starlark.AsString(list.Index(i)); ok {
			meta.groupFields = append(meta.groupFields, s)
		}
	}
}
