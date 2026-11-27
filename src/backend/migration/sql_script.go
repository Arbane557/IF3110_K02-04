package migration

import (
	"strings"
)

type migrationStatement struct {
	sql string
	log string
}

// splitMigrationSQL recognizes PostgreSQL quoting and comments so scripts can
// be sent as separate extended-protocol statements in one target transaction.
func splitMigrationSQL(query string) ([]migrationStatement, error) {
	var statements []migrationStatement
	var logText strings.Builder
	start := 0
	appendStatement := func(end int) {
		if logged := strings.TrimSpace(logText.String()); logged != "" {
			statements = append(statements, migrationStatement{strings.TrimSpace(query[start:end]), logged})
		}
		logText.Reset()
		start = end + 1
	}
	for i := 0; i < len(query); {
		switch {
		case strings.HasPrefix(query[i:], "--"):
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				i = len(query)
			} else {
				i += end + 1
			}
			logText.WriteByte(' ')
		case strings.HasPrefix(query[i:], "/*"):
			i += 2
			depth := 1
			for i < len(query) && depth > 0 {
				switch {
				case strings.HasPrefix(query[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(query[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth > 0 {
				return nil, errJobInput
			}
			logText.WriteByte(' ')
		case query[i] == '\'' || query[i] == '"':
			quote, begin := query[i], i
			escapes := quote == '\'' && i > 0 && (query[i-1] == 'E' || query[i-1] == 'e') && (i < 2 || !sqlIdentifierByte(query[i-2]))
			i++
			closed := false
			for i < len(query) {
				if escapes && query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] != quote {
					i++
					continue
				}
				i++
				if i < len(query) && query[i] == quote {
					i++
					continue
				}
				closed = true
				break
			}
			if !closed {
				return nil, errJobInput
			}
			if quote == '\'' {
				logText.WriteString("'[redacted]'")
			} else {
				logText.WriteString(query[begin:i])
			}
		case query[i] == '$' && (i == 0 || !sqlIdentifierByte(query[i-1])):
			delimiter := sqlDollarDelimiter(query[i:])
			if delimiter == "" {
				logText.WriteByte(query[i])
				i++
				continue
			}
			i += len(delimiter)
			end := strings.Index(query[i:], delimiter)
			if end < 0 {
				return nil, errJobInput
			}
			i += end + len(delimiter)
			logText.WriteString("$redacted$[redacted]$redacted$")
		case query[i] == ';':
			appendStatement(i)
			i++
		default:
			logText.WriteByte(query[i])
			i++
		}
	}
	appendStatement(len(query))
	return statements, nil
}

func sqlIdentifierByte(b byte) bool {
	return b > 127 || b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func sqlDollarDelimiter(query string) string {
	for i := 1; i < len(query); i++ {
		if query[i] == '$' {
			return query[:i+1]
		}
		if !sqlIdentifierByte(query[i]) || (i == 1 && query[i] >= '0' && query[i] <= '9') {
			break
		}
	}
	return ""
}

func sqlForLog(query string) string {
	statements, err := splitMigrationSQL(query)
	if err != nil {
		return "[SQL withheld]"
	}
	logs := make([]string, 0, len(statements))
	for _, step := range statements {
		logs = append(logs, step.log)
	}
	return strings.Join(logs, "; ")
}
