package infra

import "testing"

func TestValidateReadOnlySQLAllowsReads(t *testing.T) {
	ok := []string{
		"SHOW DATABASES",
		"show processlist",
		"SELECT * FROM hive.METASTORE WHERE id = 1",
		"SELECT name FROM t WHERE status = 'delete'",
		"EXPLAIN SELECT * FROM t",
		"DESCRIBE metastore.TBLS",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT count(*) FROM `order`",
	}
	for _, s := range ok {
		if err := ValidateReadOnlySQL(s); err != nil {
			t.Errorf("%q should be read-only, got %v", s, err)
		}
	}
}

func TestValidateReadOnlySQLRejectsWrites(t *testing.T) {
	bad := []string{
		"",
		"DROP TABLE t",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"SET GLOBAL read_only = 0",
		"TRUNCATE TABLE t",
		"GRANT ALL ON *.* TO 'x'@'%'",
		"CALL do_something()",
		"SELECT * FROM t; DROP TABLE t",
		"SELECT * FROM t -- trailing comment",
		"SELECT * FROM t /* c */",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"SELECT LOAD_FILE('/etc/passwd')",
		"LOAD DATA INFILE '/tmp/x' INTO TABLE t",
		"COMMIT",
	}
	for _, s := range bad {
		if err := ValidateReadOnlySQL(s); err == nil {
			t.Errorf("%q must be rejected", s)
		}
	}
}

func TestValidateMySQLCommandExtractsInlineSQL(t *testing.T) {
	ok := []string{
		`mysql -e "SHOW DATABASES"`,
		`mysql -h 127.0.0.1 -P 3306 -u root -e "SELECT count(*) FROM metastore.TBLS"`,
		`mysql --execute="SHOW PROCESSLIST"`,
		`mysql -e 'SELECT * FROM t WHERE a > 1'`,
	}
	for _, c := range ok {
		if err := ValidateMySQLCommand(c); err != nil {
			t.Errorf("%q should be allowed, got %v", c, err)
		}
	}
}

func TestValidateMySQLCommandRejectsUnsafeForms(t *testing.T) {
	bad := []string{
		`mysql -e "DROP TABLE t"`,
		`mysql -e "SELECT 1; UPDATE t SET a = 1"`,
		`mysql --init-command="SET SESSION TRANSACTION READ WRITE" -e "SHOW DATABASES"`,
		`mysql --init-file=/tmp/x -e "SHOW DATABASES"`,
		`mysql -e "SELECT LOAD_FILE('/etc/passwd')"`,
		`mysql`,
	}
	for _, c := range bad {
		if err := ValidateMySQLCommand(c); err == nil {
			t.Errorf("%q must be rejected", c)
		}
	}
}

// The shared command validator must admit read-only mysql invocations and keep
// refusing mysql writes, so `mysql` on the allowlist does not widen the surface.
func TestValidateReadOnlyCommandAllowsReadOnlyMySQL(t *testing.T) {
	ok := []string{
		`mysql -e "SHOW DATABASES"`,
		`mysql -h 127.0.0.1 -e "SELECT * FROM t WHERE a > 1"`,
		`/home/hadoop/.versions/mysql/bin/mysql -e "SHOW PROCESSLIST"`,
		`mysql -e "SHOW PROCESSLIST" 2>&1 | head -20`,
	}
	for _, c := range ok {
		if err := ValidateReadOnlyCommand(c); err != nil {
			t.Errorf("%q should be allowed, got %v", c, err)
		}
	}
}

func TestValidateReadOnlyCommandRejectsMySQLWrites(t *testing.T) {
	bad := []string{
		`mysql -e "DROP TABLE t"`,
		`mysql -e "UPDATE t SET a = 1"`,
		`mysql -e "SHOW DATABASES"; mysql -e "DROP TABLE t"`,
		`mysql --init-command="SET GLOBAL x=1" -e "SHOW DATABASES"`,
	}
	for _, c := range bad {
		if err := ValidateReadOnlyCommand(c); err == nil {
			t.Errorf("%q must be rejected", c)
		}
	}
}
