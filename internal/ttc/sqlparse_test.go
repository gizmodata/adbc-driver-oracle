package ttc

import (
	"reflect"
	"testing"
)

func TestParseSQL(t *testing.T) {
	cases := []struct {
		sql   string
		kind  StatementKind
		binds []string
	}{
		{"select 1 from dual", KindQuery, nil},
		{"SELECT :a, :b, :a FROM dual", KindQuery, []string{"A", "B", "A"}},
		{"insert into t (x) values (:1)", KindDML, []string{"1"}},
		{"insert into t values (:x, :y)", KindDML, []string{"X", "Y"}},
		{"update t set x = 'a:b' where y = :y", KindDML, []string{"Y"}},
		{"select q'[don't :x]' from dual where z = :z", KindQuery, []string{"Z"}},
		{"-- comment :nope\nselect :yes from dual", KindQuery, []string{"YES"}},
		{"/* :nope */ select 1 from dual", KindQuery, nil},
		{"begin :a := :b; :a := 1; end;", KindPLSQL, []string{"A", "B"}},
		{"create table t (x number)", KindDDL, nil},
		{"with q as (select 1 x from dual) select x from q", KindQuery, nil},
		{`select "quoted col" from t where a = :"weird name"`, KindQuery, []string{"weird name"}},
		{"select json_value('{}', '$.a' returning varchar2) from dual", KindQuery, nil},
	}
	for _, c := range cases {
		p := parseSQL(c.sql)
		if p.kind != c.kind {
			t.Errorf("%q: kind %v want %v", c.sql, p.kind, c.kind)
		}
		if !reflect.DeepEqual(p.bindNames, c.binds) && !(len(p.bindNames) == 0 && len(c.binds) == 0) {
			t.Errorf("%q: binds %v want %v", c.sql, p.bindNames, c.binds)
		}
	}
}
