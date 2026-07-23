package report

import (
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type parserFn func(any) (any, error)

type scriptMeta struct {
	col         map[string]map[string]any
	parser      map[int]parserFn
	style       map[string]string
	width       map[string]float64
	groupFields []string
}

func (r *excelReport) runScript(script string) error {
	meta := scriptMeta{
		col:         make(map[string]map[string]any),
		parser:      make(map[int]parserFn),
		style:       make(map[string]string),
		width:       make(map[string]float64),
		groupFields: []string{},
	}

	if script != "" {
		thread := &starlark.Thread{}
		globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, "script.star", script, nil)
		if err != nil {
			return err
		}
		parseColGlobal(globals, &meta)
		parseStyleGlobal(globals, &meta)
		parseWidthGlobal(globals, &meta)
		parseGroupFieldsGlobal(globals, &meta)
	}

	r.meta = &meta
	return nil
}

func getStarlarkDict(globals starlark.StringDict, key string) *starlark.Dict {
	val, ok := globals[key]
	if !ok {
		return nil
	}
	dict, ok := val.(*starlark.Dict)
	if !ok {
		return nil
	}
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
		meta.style[k] = v
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
			meta.width[k] = f
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
