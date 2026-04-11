package report

import (
	"database/sql"
	"reflect"
	"strconv"

	"github.com/jmoiron/sqlx"
)

type RowsReader interface {
	Read() error
	Headers() ([]string, error)
	Values() ([]any, error)
	Next() bool
	Err() error
}

type StructRows struct {
	rows  []any
	index int
}

func NewStructRows(rows []any) *StructRows {
	return &StructRows{
		rows:  rows,
		index: -1,
	}
}

func (r *StructRows) Read() error { return nil }

func (r *StructRows) Headers() ([]string, error) {
	if len(r.rows) == 0 {
		return nil, sql.ErrNoRows
	}
	result := []string{}
	t := reflect.TypeOf(r.rows[0])
	for field := range t.Fields() {
		result = append(result, field.Name)
	}
	return result, nil
}

func (r *StructRows) Values() ([]any, error) {
	if r.index < 0 || r.index >= len(r.rows) {
		return nil, sql.ErrNoRows
	}
	result := []any{}
	v := reflect.ValueOf(r.rows[r.index])
	for _, val := range v.Fields() {
		if val.Kind() == reflect.Pointer {
			if !val.IsNil() {
				val = val.Elem()
			} else {
				val = reflect.ValueOf("")
			}
		}
		result = append(result, val.Interface())
	}
	return result, nil
}

func (r *StructRows) Next() bool {
	r.index++
	return r.index < len(r.rows)
}

func (r *StructRows) Err() error {
	if len(r.rows) == 0 {
		return sql.ErrNoRows
	}
	return nil
}

var _ RowsReader = (*StructRows)(nil) // interface implementation check

type SqlxRows struct {
	query       string
	db          *sqlx.DB
	rows        *sqlx.Rows
	columnTypes []*sql.ColumnType
}

func NewSqlxRows(query string, db *sqlx.DB) *SqlxRows {
	return &SqlxRows{
		query: query,
		db:    db,
	}
}

func (r *SqlxRows) Read() error {
	if r.db == nil {
		return sql.ErrNoRows
	}
	var err error
	if r.rows, err = r.db.Queryx(r.query); err != nil {
		return err
	}
	if r.columnTypes, err = r.rows.ColumnTypes(); err != nil {
		return err
	}
	return nil
}

func (r *SqlxRows) Values() ([]any, error) {
	if r.rows == nil {
		return nil, sql.ErrNoRows
	}
	if vals, err := r.rows.SliceScan(); err != nil {
		return nil, err
	} else {
		for i, val := range vals {
			if val != nil {
				if r.columnTypes[i].DatabaseTypeName() == "NUMERIC" {
					if f, err := strconv.ParseFloat(val.(string), 64); err == nil {
						vals[i] = f
					}
				}
			}
		}
		return vals, nil
	}
}

func (r *SqlxRows) Headers() ([]string, error) {
	if r.rows == nil {
		return nil, sql.ErrNoRows
	}
	return r.rows.Columns()
}

func (r *SqlxRows) Next() bool {
	if r.rows == nil {
		return false
	}
	return r.rows.Next()
}

func (r *SqlxRows) Err() error {
	if r.rows == nil {
		return sql.ErrNoRows
	}
	return r.rows.Err()
}

var _ RowsReader = (*SqlxRows)(nil) // interface implementation check
