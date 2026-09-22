package database

import "testing"

func TestQueryExecType(t *testing.T) {
	type args struct {
		prefix string
		sqlstr string
	}
	tests := []struct {
		name         string
		prefix       string
		sqlstr       string
		wantPrefix   string
		wantExecType bool
	}{
		{
			name:         "select",
			prefix:       "select * from city",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "select with space",
			prefix:       "    select    * from city",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "start linebreak",
			prefix:       "\nselect * from city",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "with linebreak",
			prefix:       "select\n* from city",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "start tab",
			prefix:       "\tselect * from city",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "with tab",
			prefix:       "select\t* from city",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "explain",
			prefix:       "explain select * from city",
			sqlstr:       "",
			wantPrefix:   "EXPLAIN",
			wantExecType: true,
		},
		{
			name:         "insert",
			prefix:       "insert into city values (8181, 'Kabul', 'AFG', 'Kabol', 1780000);",
			sqlstr:       "",
			wantPrefix:   "INSERT",
			wantExecType: false,
		},
		{
			name:         "delete",
			prefix:       "delete from city where id = 8181;",
			sqlstr:       "",
			wantPrefix:   "DELETE",
			wantExecType: false,
		},
		{
			name:         "block and line comment before select",
			prefix:       "/* heading */\n-- note\nSELECT 1",
			sqlstr:       "",
			wantPrefix:   "SELECT",
			wantExecType: true,
		},
		{
			name:         "line comment before update",
			prefix:       "-- note\nUPDATE city SET name = 'x' WHERE id = 1",
			sqlstr:       "",
			wantPrefix:   "UPDATE",
			wantExecType: false,
		},
		{
			name:         "legacy select into unaffected by trivia trimming",
			prefix:       "select INTO new_city from city",
			sqlstr:       "",
			wantPrefix:   "SELECT INTO",
			wantExecType: false,
		},
		{
			name:         "legacy pragma get unaffected by trivia trimming",
			prefix:       "PRAGMA foreign_keys",
			sqlstr:       "",
			wantPrefix:   "PRAGMA",
			wantExecType: true,
		},
		{
			name:         "legacy pragma set unaffected by trivia trimming",
			prefix:       "PRAGMA foreign_keys",
			sqlstr:       "PRAGMA foreign_keys=1",
			wantPrefix:   "PRAGMA",
			wantExecType: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, execType := QueryExecType(tt.prefix, tt.sqlstr)
			if prefix != tt.wantPrefix {
				t.Errorf("QueryExecType() got = %v, want %v", prefix, tt.wantPrefix)
			}
			if execType != tt.wantExecType {
				t.Errorf("QueryExecType() got1 = %v, want %v", execType, tt.wantExecType)
			}
		})
	}
}
