package ttc

import (
	"strings"
	"unicode"
)

// StatementKind classifies a SQL statement by its leading keyword.
type StatementKind int

// Statement kinds.
const (
	KindUnknown StatementKind = iota
	KindQuery
	KindDML
	KindDDL
	KindPLSQL
)

// parsedSQL is the result of scanning SQL text.
type parsedSQL struct {
	kind        StatementKind
	bindNames   []string // in order of appearance (duplicates repeated for SQL, deduped for PL/SQL)
	isReturning bool
}

func determineKind(keyword string) StatementKind {
	switch keyword {
	case "DECLARE", "BEGIN", "CALL":
		return KindPLSQL
	case "SELECT", "WITH":
		return KindQuery
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		return KindDML
	case "CREATE", "ALTER", "DROP", "GRANT", "REVOKE", "ANALYZE", "AUDIT", "COMMENT", "TRUNCATE":
		return KindDDL
	}
	return KindUnknown
}

// parseSQL scans the statement for its kind and bind variable names,
// mirroring python-oracledb's StatementParser (comments, quoted strings,
// q-strings and JSON-constant colons are skipped).
func parseSQL(sql string) parsedSQL {
	var p parsedSQL
	rs := []rune(sql)
	n := len(rs)
	pos := 0
	initialKeywordFound := false
	returningKeywordFound := false
	lastWasString := false
	lastWasAlpha := false
	var lastCh rune
	alphaStart := 0
	seen := map[string]bool{}

	addBind := func(name string) {
		if p.kind == KindPLSQL && seen[name] {
			return
		}
		seen[name] = true
		p.bindNames = append(p.bindNames, name)
	}

	for pos < n {
		ch := rs[pos]
		isAlpha := unicode.IsLetter(ch)
		if isAlpha && !lastWasAlpha {
			alphaStart = pos
		} else if !isAlpha && lastWasAlpha {
			word := strings.ToUpper(string(rs[alphaStart:pos]))
			if !initialKeywordFound {
				p.kind = determineKind(word)
				initialKeywordFound = true
				if p.kind == KindDDL {
					break
				}
			} else if p.kind == KindDML && !returningKeywordFound && word == "RETURNING" {
				returningKeywordFound = true
			} else if returningKeywordFound && word == "INTO" {
				p.isReturning = true
			}
		}
		switch {
		case ch == '\'':
			lastWasString = true
			if lastCh == 'q' || lastCh == 'Q' {
				pos = skipQString(rs, pos)
			} else {
				pos = skipQuoted(rs, pos, '\'')
			}
		case unicode.IsSpace(ch):
		default:
			switch ch {
			case '-':
				if pos+1 < n && rs[pos+1] == '-' {
					for pos < n && rs[pos] != '\n' {
						pos++
					}
				}
			case '/':
				if pos+1 < n && rs[pos+1] == '*' {
					end := strings.Index(string(rs[pos+2:]), "*/")
					if end < 0 {
						pos = n
					} else {
						pos += 2 + len([]rune(string(rs[pos+2:])[:end])) + 1
					}
				}
			case '"':
				pos = skipQuoted(rs, pos, '"')
			case ':':
				if !lastWasString {
					name, next := parseBindName(rs, pos)
					if name != "" {
						addBind(name)
						pos = next
						lastWasString = false
						lastWasAlpha = false
						lastCh = ':'
						pos++
						continue
					}
				}
			}
			lastWasString = false
		}
		if pos < n {
			lastCh = rs[pos]
			lastWasAlpha = unicode.IsLetter(lastCh)
		}
		pos++
	}
	if !initialKeywordFound && lastWasAlpha && alphaStart < n {
		p.kind = determineKind(strings.ToUpper(string(rs[alphaStart:])))
	}
	return p
}

// skipQuoted returns the index of the closing quote.
func skipQuoted(rs []rune, pos int, q rune) int {
	pos++
	for pos < len(rs) {
		if rs[pos] == q {
			if pos+1 < len(rs) && rs[pos+1] == q {
				pos += 2
				continue
			}
			return pos
		}
		pos++
	}
	return len(rs)
}

// skipQString handles q'[...]' style literals; pos is at the quote.
func skipQString(rs []rune, pos int) int {
	pos++
	if pos >= len(rs) {
		return len(rs)
	}
	sep := rs[pos]
	switch sep {
	case '[':
		sep = ']'
	case '{':
		sep = '}'
	case '<':
		sep = '>'
	case '(':
		sep = ')'
	}
	pos++
	for pos+1 < len(rs) {
		if rs[pos] == sep && rs[pos+1] == '\'' {
			return pos + 1
		}
		pos++
	}
	return len(rs)
}

// parseBindName parses a bind name after a colon; returns the normalized
// name and the index of the last character consumed.
func parseBindName(rs []rune, pos int) (string, int) {
	i := pos + 1
	for i < len(rs) && unicode.IsSpace(rs[i]) {
		i++
	}
	if i >= len(rs) {
		return "", pos
	}
	start := i
	switch {
	case rs[i] == '"':
		j := i + 1
		for j < len(rs) && rs[j] != '"' {
			j++
		}
		return string(rs[start+1 : j]), j
	case unicode.IsDigit(rs[i]):
		j := i
		for j < len(rs) && unicode.IsDigit(rs[j]) {
			j++
		}
		return string(rs[start:j]), j - 1
	case unicode.IsLetter(rs[i]):
		j := i
		for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_' || rs[j] == '$' || rs[j] == '#') {
			j++
		}
		return strings.ToUpper(string(rs[start:j])), j - 1
	}
	return "", pos
}
