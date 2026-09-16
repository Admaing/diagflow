// Read-only MySQL access for cluster nodes.
//
// Operators frequently need to inspect the cluster's databases (the Hive
// metastore, for example) with the mysql client. DiagFlow exposes that as a
// plain command (`mysql -e "..."`) but only ever read-only: a command that
// opens a mysql session must carry a single SELECT/SHOW/DESC/EXPLAIN-style
// statement, and any write verb, multi-statement separator, comment, or
// file-writing clause is refused before the command reaches a node.
package infra

import (
	"fmt"
	"regexp"
	"strings"
)

// readOnlySQLHeads are the only statement heads a read-only mysql invocation
// may use. USE selects a default schema and mutates nothing; TABLE (MySQL 8)
// lists rows like SELECT.
var readOnlySQLHeads = map[string]bool{
	"select": true, "show": true, "desc": true, "describe": true,
	"explain": true, "with": true, "table": true, "help": true, "use": true,
}

// writeSQLVerbs are write / DDL / transaction-control / session-control verbs.
// They are matched on word boundaries against a copy of the statement with all
// string literals and quoted identifiers removed, so a value like 'delete' or
// a column named `update_time` does not trip the filter.
var writeSQLVerbs = []string{
	"insert", "update", "delete", "replace", "upsert", "merge",
	"create", "alter", "drop", "truncate", "rename",
	"grant", "revoke", "flush", "reset", "purge",
	"call", "handler", "load", "outfile", "dumpfile", "infile",
	"lock", "unlock", "kill", "shutdown", "set", "commit", "rollback", "savepoint",
	"analyze", "optimize", "repair", "checksum", "install", "uninstall",
	"start", "begin", "xa", "change", "stop", "slave", "replicate",
}

var writeSQLRe = regexp.MustCompile(`\b(` + strings.Join(writeSQLVerbs, "|") + `)\b`)

// writeSQLFragments are non-read constructs that word-boundary matching would
// miss because they contain an underscore (LOAD_FILE) or need their exact
// spelling (INTO OUTFILE). They are matched on the structure-only copy.
var writeSQLFragments = []string{"load_file(", "into outfile", "into dumpfile"}

// invalidMySQLOptions are mysql client flags that could run SQL we never
// validated or read/write local files.
var invalidMySQLOptions = []string{
	"--init-command", "--init-file", "--local-infile",
}

// ValidateReadOnlySQL reports whether sql is a single read-only MySQL
// statement. It is deliberately conservative: anything it does not recognise as
// a read is rejected.
func ValidateReadOnlySQL(sql string) error {
	stmt := strings.TrimSpace(sql)
	if stmt == "" {
		return fmt.Errorf("empty SQL statement")
	}
	if len(stmt) > 8000 {
		return fmt.Errorf("SQL statement too long (%d bytes)", len(stmt))
	}
	// Remove string literals and quoted identifiers so keyword checks only see
	// SQL structure, never data or names.
	structural := stripSQLQuoted(stmt)

	// A comment can hide part of the statement from a naive reader, and it is
	// never needed for a read; refuse it outright.
	if strings.Contains(structural, "--") || strings.Contains(structural, "#") ||
		strings.Contains(structural, "/*") {
		return fmt.Errorf("SQL comments are not allowed")
	}
	// Multi-statement input could smuggle a write past the head check.
	if strings.Contains(strings.TrimRight(structural, "; \t\r\n"), ";") {
		return fmt.Errorf("only a single SQL statement is allowed")
	}

	lower := strings.ToLower(strings.TrimSpace(strings.TrimRight(structural, "; \t\r\n")))
	if lower == "" {
		return fmt.Errorf("empty SQL statement")
	}
	head := lower
	if i := strings.IndexAny(head, " \t\r\n("); i >= 0 {
		head = head[:i]
	}
	if !readOnlySQLHeads[head] {
		return fmt.Errorf("SQL statement %q is not read-only (allowed: SELECT, SHOW, DESC, EXPLAIN, WITH)", head)
	}
	if m := writeSQLRe.FindString(lower); m != "" {
		return fmt.Errorf("SQL contains a non-read-only keyword %q", m)
	}
	for _, frag := range writeSQLFragments {
		if strings.Contains(lower, frag) {
			return fmt.Errorf("SQL contains a non-read-only construct %q", frag)
		}
	}
	return nil
}

// stripSQLQuoted removes '...', "..." and `...` spans, honouring backslash and
// doubled-quote escapes. The remaining text is the statement's structure.
func stripSQLQuoted(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c != '\'' && c != '"' && c != '`' {
			b.WriteRune(c)
			continue
		}
		quote := c
		for i++; i < len(runes); i++ {
			if runes[i] == '\\' {
				i++
				continue
			}
			if runes[i] == quote {
				if i+1 < len(runes) && runes[i+1] == quote {
					i++
					continue
				}
				break
			}
		}
		b.WriteRune(' ')
	}
	return b.String()
}

// ValidateMySQLCommand validates a `mysql ...` command segment (as it appears
// in a shell command line) and checks that its -e/--execute SQL is read-only.
// The segment must carry the SQL inline; other input paths (`< file`, a
// heredoc, source/\.) are already blocked by ValidateReadOnlyCommand.
func ValidateMySQLCommand(segment string) error {
	tokens, err := shellFields(segment)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("empty mysql command")
	}
	sql, ok := "", false
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		low := strings.ToLower(tok)
		for _, bad := range invalidMySQLOptions {
			if low == bad || strings.HasPrefix(low, bad+"=") {
				return fmt.Errorf("mysql option %q is not allowed", bad)
			}
		}
		switch {
		case low == "-e" || low == "--execute":
			if i+1 >= len(tokens) {
				return fmt.Errorf("mysql %s requires an inline SQL string", tok)
			}
			sql, ok = tokens[i+1], true
			i++
		case strings.HasPrefix(low, "--execute="):
			sql, ok = tok[len("--execute="):], true
		}
	}
	if !ok {
		return fmt.Errorf("mysql must be called with an inline read-only query, e.g. mysql -e \"SHOW DATABASES\"")
	}
	return ValidateReadOnlySQL(sql)
}

// shellFields splits a shell command fragment into fields, honouring single and
// double quotes and backslash escapes. It is intentionally small: the fragment
// has already passed ValidateReadOnlyCommand, so it contains no operators.
func shellFields(s string) ([]string, error) {
	var (
		fields  []string
		cur     strings.Builder
		quote   rune
		started bool
	)
	flush := func() {
		if started {
			fields = append(fields, cur.String())
			cur.Reset()
			started = false
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteRune(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case '\\':
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				started = true
			}
		case ' ', '\t':
			flush()
		default:
			cur.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in mysql command")
	}
	flush()
	return fields, nil
}
