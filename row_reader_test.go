package report

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
)

type TestStruct struct {
	Name   string
	Age    int
	Member bool
	Joined time.Time
}

func TestStructRows(t *testing.T) {
	testCases := []struct {
		name            string
		input           []any
		expectedErr     error
		expectedHdrs    []string
		expectedHdrsErr error
		expectedVals    [][]any
	}{
		{
			name: "Simple case",
			input: []any{
				TestStruct{Name: "John", Age: 30, Member: true, Joined: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)},
				TestStruct{Name: "Jane", Age: 25, Member: false, Joined: time.Date(2022, 2, 1, 0, 0, 0, 0, time.UTC)},
			},
			expectedErr:     nil,
			expectedHdrs:    []string{"Name", "Age", "Member", "Joined"},
			expectedHdrsErr: nil,
			expectedVals: [][]any{
				{"John", 30, true, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)},
				{"Jane", 25, false, time.Date(2022, 2, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
		{
			name:            "Empty case",
			input:           []any{},
			expectedErr:     sql.ErrNoRows,
			expectedHdrs:    nil,
			expectedHdrsErr: sql.ErrNoRows,
			expectedVals:    nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rowReader := NewStructRows(tc.input)

			assert.NoError(t, rowReader.Read())
			assert.Equal(t, tc.expectedErr, rowReader.Err())

			hdrs, err := rowReader.Headers()
			assert.Equal(t, tc.expectedHdrsErr, err)
			assert.Equal(t, tc.expectedHdrs, hdrs)

			for _, expectedVal := range tc.expectedVals {
				assert.True(t, rowReader.Next())
				vals, err := rowReader.Values()
				assert.NoError(t, err)
				assert.Equal(t, expectedVal, vals)
			}

			assert.False(t, rowReader.Next())
		})
	}
}

func TestSqlxRows(t *testing.T) {
	testCases := []struct {
		name            string
		input           []TestStruct
		expectedErr     error
		expectedHdrs    []string
		expectedHdrsErr error
		expectedVals    [][]any
	}{
		{
			name: "Simple case",
			input: []TestStruct{
				{Name: "John", Age: 30, Member: true, Joined: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Name: "Jane", Age: 25, Member: false, Joined: time.Date(2022, 2, 1, 0, 0, 0, 0, time.UTC)},
			},
			expectedErr:     nil,
			expectedHdrs:    []string{"Name", "Age", "Member", "Joined"},
			expectedHdrsErr: nil,
			expectedVals: [][]any{
				{"John", int64(30), true, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)},
				{"Jane", int64(25), false, time.Date(2022, 2, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
		{
			name:            "Empty case",
			input:           []TestStruct{},
			expectedErr:     nil, // SqlxRows doesn't return sql.ErrNoRows on empty result set from Read(), just empty iterator
			expectedHdrs:    []string{"Name", "Age", "Member", "Joined"},
			expectedHdrsErr: nil,
			expectedVals:    nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			assert.NoError(t, err)
			defer db.Close()

			sqlxDB := sqlx.NewDb(db, "sqlmock")

			rows := sqlmock.NewRows([]string{"Name", "Age", "Member", "Joined"})
			for _, v := range tc.input {
				rows.AddRow(v.Name, v.Age, v.Member, v.Joined)
			}

			mock.ExpectQuery("SELECT .*").WillReturnRows(rows)

			rowReader := NewSqlxRows("SELECT * FROM users", sqlxDB)

			assert.NoError(t, rowReader.Read())
			assert.NoError(t, rowReader.Err()) // Err() checks rows.Err() which should be nil initially

			hdrs, err := rowReader.Headers()
			assert.Equal(t, tc.expectedHdrsErr, err)
			assert.Equal(t, tc.expectedHdrs, hdrs)

			for _, expectedVal := range tc.expectedVals {
				assert.True(t, rowReader.Next())
				vals, err := rowReader.Values()
				assert.NoError(t, err)
				// Sqlx/Sqlmock behavior on int might differ slightly (int64 vs int), asserting loosely or casting expected
				assert.Equal(t, expectedVal, vals)
			}

			assert.False(t, rowReader.Next())
			assert.NoError(t, rowReader.Err())
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
