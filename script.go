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

func getStarlarkDict(globals starlark.StringDict, key string) *starlark.Dict {
	dict, _ := globals[key].(*starlark.Dict)
	return dict
}

func parseColGlobal(globals starlark.StringDict, meta *scriptMeta) {
	dict := getStarlarkDict(globals, "col")
	if dict == nil {
		return
	}
	for _, item := range dict.Items() {
		colName, ok := starlark.AsString(item[0])
		if !ok {
			continue
		}
		entryDict, ok := item[1].(*starlark.Dict)
		if !ok {
			continue
		}
		meta.col[colName] = starlarkDictToMap(entryDict)
	}
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
	dict := getStarlarkDict(globals, "style")
	if dict == nil {
		return
	}
	for _, item := range dict.Items() {
		k, ok := starlark.AsString(item[0])
		if !ok {
			continue
		}
		v, ok := starlark.AsString(item[1])
		if !ok {
			continue
		}
		meta.styles = append(meta.styles, scriptStyle{Target: k, StyleJSON: v})
	}
}

func parseWidthGlobal(globals starlark.StringDict, meta *scriptMeta) {
	dict := getStarlarkDict(globals, "width")
	if dict == nil {
		return
	}
	for _, item := range dict.Items() {
		k, ok := starlark.AsString(item[0])
		if !ok {
			continue
		}
		if f, ok := starlark.AsFloat(item[1]); ok {
			meta.widths = append(meta.widths, widthRule{Target: k, Width: f})
		}
	}
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
	for i := 0; i < list.Len(); i++ {
		if s, ok := starlark.AsString(list.Index(i)); ok {
			meta.groupFields = append(meta.groupFields, s)
		}
	}
}
