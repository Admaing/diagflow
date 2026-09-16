package infra

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildLogCommandTail(t *testing.T) {
	cmd, err := BuildLogCommand("/var/log/hadoop-hdfs/hdfs.log", "", 150)
	if err != nil {
		t.Fatal(err)
	}
	want := "if [ -f '/var/log/hadoop-hdfs/hdfs.log' ]; then tail -n 150 -- '/var/log/hadoop-hdfs/hdfs.log'; else echo '__DIAGFLOW_NOFILE__'; fi"
	if cmd != want {
		t.Fatalf("got %q want %q", cmd, want)
	}
}

func TestBuildLogCommandTailGrep(t *testing.T) {
	cmd, err := BuildLogCommand("/var/log/x.log", "ERROR|Exception", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "grep -E -i -- 'ERROR|Exception'") {
		t.Fatalf("missing grep filter: %q", cmd)
	}
	if !strings.Contains(cmd, "tail -n 100") {
		t.Fatalf("missing final tail: %q", cmd)
	}
	if !strings.Contains(cmd, NoFileMarker) {
		t.Fatalf("missing no-file marker: %q", cmd)
	}
}

func TestBuildLogCommandRejectsUnsafePaths(t *testing.T) {
	for _, bad := range []string{
		"relative/log.log", "/var/log/x; rm -rf /", "/var/log/$(id).log",
		"/var/log/`id`.log", "/var/log/a'b.log", "",
		"/var/log/../../etc/passwd", "/..\x00",
	} {
		if _, err := BuildLogCommand(bad, "", 10); err == nil {
			t.Errorf("path %q should be rejected", bad)
		}
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	if got := ShellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("got %q", got)
	}
	if got := shellQuote("plain"); got != "'plain'" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateReadOnlyCommand(t *testing.T) {
	ok := []string{"cat /etc/hosts", "grep -i error /var/log/x.log", "ps -ef", "jps -l",
		"tail -n 100 /var/log/x.log | grep ERROR", "df -h; free -m"}
	for _, c := range ok {
		if err := ValidateReadOnlyCommand(c); err != nil {
			t.Errorf("command %q should be allowed: %v", c, err)
		}
	}
	bad := []string{
		"", "rm -rf /", "sudo reboot", "echo $(id)", "cat x > /tmp/y",
		"cat /etc/passwd | sh", "curl http://evil", "python3 -c 'x'",
		"kill -9 1", "dd if=/dev/zero of=/dev/sda",
	}
	for _, c := range bad {
		if err := ValidateReadOnlyCommand(c); err == nil {
			t.Errorf("command %q should be rejected", c)
		}
	}
}

func TestValidatePattern(t *testing.T) {
	if err := ValidatePattern("ERROR|Exception,FATAL"); err != nil {
		t.Fatalf("allowed pattern rejected: %v", err)
	}
	if err := ValidatePattern("ERROR; rm -rf /"); err == nil {
		t.Fatal("shell metacharacters should be rejected")
	}
}

func TestDecodePasswd(t *testing.T) {
	// "Gaoyongfu24861." as returned by describe_cluster's passwd field.
	pw, err := DecodePasswd("R2FveW9uZ2Z1MjQ4NjEu")
	if err != nil {
		t.Fatal(err)
	}
	if pw != "Gaoyongfu24861." {
		t.Fatalf("password = %q", pw)
	}
	if _, err := DecodePasswd("not base64!"); err == nil {
		t.Fatal("invalid base64 should fail")
	}
}

func TestCredentialForDefaultsToRoot(t *testing.T) {
	cred, err := CredentialFor(NodeInfo{Passwd: "c2VjcmV0"})
	if err != nil {
		t.Fatal(err)
	}
	if cred.User != "root" || cred.Password != "secret" {
		t.Fatalf("cred = %+v", cred)
	}
}

type recordingExec struct {
	host string
	cmd  string
	out  string
	err  error
}

func (r *recordingExec) Run(_ context.Context, host string, _ Credential, cmd string) (string, error) {
	r.host, r.cmd = host, cmd
	return r.out, r.err
}

func TestLogReaderEmptyOutputIsNotAnError(t *testing.T) {
	rec := &recordingExec{out: ""}
	r := &LogReader{Exec: rec}
	out, err := r.ReadLog(context.Background(), "10.0.0.1", Credential{}, "/var/log/x.log", "", 5)
	if err != nil {
		t.Fatalf("empty output should not be an error: %v", err)
	}
	if out != "" {
		t.Fatalf("out = %q", out)
	}
}

func TestLogReaderDelegates(t *testing.T) {
	rec := &recordingExec{out: "line1\nline2"}
	r := &LogReader{Exec: rec}
	out, err := r.ReadLog(context.Background(), "10.0.0.1", Credential{User: "root"}, "/var/log/x.log", "ERROR", 5)
	if err != nil {
		t.Fatal(err)
	}
	if out != "line1\nline2" {
		t.Fatalf("out = %q", out)
	}
	if rec.host != "10.0.0.1" || !strings.Contains(rec.cmd, "/var/log/x.log") {
		t.Fatalf("host=%q cmd=%q", rec.host, rec.cmd)
	}
}

func TestHostPortBracketsIPv6(t *testing.T) {
	if got := hostPort("10.0.0.1", 22); got != "10.0.0.1:22" {
		t.Errorf("ipv4 = %q", got)
	}
	if got := hostPort("2003:da8::1", 22); got != "[2003:da8::1]:22" {
		t.Errorf("ipv6 = %q", got)
	}
}

func TestValidateLogDir(t *testing.T) {
	for _, ok := range []string{"/var/log", "/var/log/hadoop-hdfs", "/var/log/hadoop-yarn/"} {
		if err := ValidateLogDir(ok); err != nil {
			t.Errorf("dir %q should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"/etc", "/var/log/../etc", "/", "relative", "/var/lib/hadoop"} {
		if err := ValidateLogDir(bad); err == nil {
			t.Errorf("dir %q should be rejected", bad)
		}
	}
}

func TestBuildListLogsCommand(t *testing.T) {
	cmd, err := BuildListLogsCommand([]string{"/var/log/hadoop-hdfs", "/var/log/hadoop-yarn"})
	if err != nil {
		t.Fatal(err)
	}
	// Missing directories must be skipped, not treated as an error.
	if !strings.Contains(cmd, "if [ -d '/var/log/hadoop-hdfs' ]") {
		t.Fatalf("missing dir guard: %q", cmd)
	}
	if !strings.Contains(cmd, "ls -l '/var/log/hadoop-yarn/'") {
		t.Fatalf("ls must follow symlinked log dirs: %q", cmd)
	}
	if _, err := BuildListLogsCommand([]string{"/etc"}); err == nil {
		t.Fatal("unsafe dir should be rejected")
	}
	if _, err := BuildListLogsCommand(nil); err == nil {
		t.Fatal("empty dir list should be rejected")
	}
}

func TestCapLineWidth(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := CapLineWidth("short\n"+long+"\nend", 100)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "short" || lines[2] != "end" {
		t.Fatalf("short lines must be untouched: %q %q", lines[0], lines[2])
	}
	if n := utf8.RuneCountInString(lines[1]); n > 140 {
		t.Fatalf("long line not truncated: %d runes", n)
	}
	if !strings.Contains(lines[1], "truncated 4900 chars") {
		t.Fatalf("expected a truncation marker, got %q", lines[1][:80])
	}
	// UTF-8 must not be split mid-rune.
	cn := strings.Repeat("日", 300)
	if out := CapLineWidth(cn, 10); !utf8.ValidString(out) {
		t.Fatalf("truncation broke UTF-8: %q", out)
	}
	if CapLineWidth("no change", 100) != "no change" {
		t.Fatal("text under the cap must be returned verbatim")
	}
}

// The agent reaches for hdfs reporting commands constantly; they are read-only
// and must pass the allowlist.
func TestValidateReadOnlyCommandAllowsHDFS(t *testing.T) {
	ok := []string{
		"hdfs dfsadmin -report",
		"hdfs dfsadmin -safemode get",
		"hdfs haadmin -getAllServiceState",
		"hdfs fsck / -files -blocks",
		"hdfs dfsadmin -report 2>&1 | head -60",
		"hadoop version",
		"jps -l | grep NameNode",
	}
	for _, c := range ok {
		if err := ValidateReadOnlyCommand(c); err != nil {
			t.Errorf("%q should be allowed, got %v", c, err)
		}
	}
}

// A "|" inside quotes is regex alternation, not a pipeline. Treating it as a
// separator rejected valid commands.
func TestValidateReadOnlyCommandQuoteAwarePipes(t *testing.T) {
	ok := []string{
		`grep -iE "safemode|corrupt|missing block" /var/log/hadoop-hdfs/hdfs.log`,
		`grep -iE 'safemode|safe mode' /var/log/hadoop-hdfs/hdfs.log`,
		`egrep "ERROR|FATAL" /var/log/yarn.log`,
	}
	for _, c := range ok {
		if err := ValidateReadOnlyCommand(c); err != nil {
			t.Errorf("%q should be allowed, got %v", c, err)
		}
	}
}

// Anything that could write a file or mutate state must still be refused.
func TestValidateReadOnlyCommandStillRejectsWrites(t *testing.T) {
	bad := []string{
		"rm -rf /data",
		"cat /etc/passwd > /tmp/leak",
		"echo hi >> /etc/hosts",
		"hdfs dfsadmin -report > /tmp/x",
		"hdfs dfs -rm /tmp/x",
		"sudo hdfs dfsadmin -report",
		"dd if=/dev/zero of=/dev/sda",
		"kill -9 1234",
		"touch /tmp/x",
	}
	for _, c := range bad {
		if err := ValidateReadOnlyCommand(c); err == nil {
			t.Errorf("%q must be rejected", c)
		}
	}
}

// Real log names embed the instance id and node name, so a glob is the only
// practical way to name them. The expanded form must stay bounded.
func TestBuildLogCommandGlob(t *testing.T) {
	cmd, err := BuildLogCommand("/var/log/hadoop-hdfs/hadoop-hadoop-namenode-*.log", "ERROR|Exception", 50)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "'/var/log/hadoop-hdfs/hadoop-hadoop-namenode-*.log'") {
		t.Fatalf("glob must not be quoted or the shell cannot expand it: %s", cmd)
	}
	if !strings.Contains(cmd, "ls -d /var/log/hadoop-hdfs/hadoop-hadoop-namenode-*.log") {
		t.Fatalf("glob existence must be probed with ls, not [ -f ]: %s", cmd)
	}
	// Every glob branch must bound total output, not per-file output.
	if !strings.Contains(cmd, "| tail -n 50") {
		t.Fatalf("glob output must be bounded: %s", cmd)
	}
	if !strings.HasSuffix(cmd, "fi") {
		t.Fatalf("command must stay wrapped in the no-file guard: %s", cmd)
	}
}

func TestBuildLogCommandGlobNoKeywords(t *testing.T) {
	cmd, err := BuildLogCommand("/var/log/hadoop-yarn/*.log", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "tail -q") {
		t.Fatalf("glob tail must be quiet (no per-file headers): %s", cmd)
	}
	if !strings.Contains(cmd, "| tail -n 100") {
		t.Fatalf("glob output must be bounded: %s", cmd)
	}
}

// A glob is still a path: it cannot smuggle shell syntax.
func TestValidateLogPathGlobSafety(t *testing.T) {
	ok := []string{
		"/var/log/hadoop-hdfs/*.log",
		"/var/log/hadoop-hdfs/hadoop-hadoop-namenode-*.log",
		"/var/log/hadoop-hdfs/hadoop-*.log",
	}
	for _, p := range ok {
		if err := ValidateLogPath(p); err != nil {
			t.Errorf("%q should be allowed, got %v", p, err)
		}
	}
	bad := []string{
		"/var/log/$(whoami)/x.log",
		"/var/log/a;rm -rf /.log",
		"/var/log/`id`.log",
		"/var/log/../etc/passwd",
		"/var/log/a b.log",
		"/var/log/x.log|cat",
	}
	for _, p := range bad {
		if err := ValidateLogPath(p); err == nil {
			t.Errorf("%q must be rejected", p)
		}
	}
}

// Word-boundary matching: "KILLED" is a YARN app state, not the kill command.
func TestValidateReadOnlyCommandVerbBoundaries(t *testing.T) {
	ok := []string{
		"yarn application -list -appStates KILLED",
		`grep -i "killed" /var/log/hadoop-yarn/yarn.log`,
		`grep -c "removed" /var/log/hadoop-hdfs/hdfs-audit.log`,
		`grep -i "commit" /var/log/hadoop-hdfs/hdfs-audit.log`,
	}
	for _, c := range ok {
		if err := ValidateReadOnlyCommand(c); err != nil {
			t.Errorf("%q should be allowed, got %v", c, err)
		}
	}
	bad := []string{"kill -9 1", "pkill java", "mv /a /b", "touch /tmp/x", "systemctl stop hadoop"}
	for _, c := range bad {
		if err := ValidateReadOnlyCommand(c); err == nil {
			t.Errorf("%q must be rejected", c)
		}
	}
}

func TestFilterLogNoiseDropsClasspathDumps(t *testing.T) {
	in := strings.Join([]string{
		`2026-09-15 11:35:04,244 INFO org.apache.hadoop.ipc.Server: AccessControlException: Access denied for user root`,
		`STARTUP_MSG:   classpath = /home/hadoop/contrib/*.jar:/home/hadoop/.../error_prone_annotations-2.3.4.jar`,
		`2026-09-15 11:35:04,244 INFO org.apache.zookeeper.ClientCnxn: Client environment:java.class.path=/a:/b`,
		`2026-09-15 13:42:19,271 ERROR org.apache.hadoop.yarn.logaggregation.Service: Error reading root log dir`,
	}, "\n")
	out, dropped := FilterLogNoise(in)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if strings.Contains(out, "classpath") {
		t.Fatalf("classpath lines survived: %q", out)
	}
	if !strings.Contains(out, "AccessControlException") || !strings.Contains(out, "Error reading root log dir") {
		t.Fatalf("real hits were dropped: %q", out)
	}
}

func TestFilterLogNoiseLeavesCleanInputAlone(t *testing.T) {
	in := "a\nb"
	out, dropped := FilterLogNoise(in)
	if out != in || dropped != 0 {
		t.Fatalf("got %q dropped=%d", out, dropped)
	}
}

func TestCapSearchSnippetKeepsNewestLines(t *testing.T) {
	in := "l1\nl2\nl3\nl4\nl5"
	out, dropped := CapSearchSnippet(in, 2)
	if out != "l4\nl5" {
		t.Fatalf("got %q", out)
	}
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
}

func TestCapSearchSnippetNoOpUnderLimit(t *testing.T) {
	out, dropped := CapSearchSnippet("l1\nl2\n", 5)
	if out != "l1\nl2" || dropped != 0 {
		t.Fatalf("got %q dropped=%d", out, dropped)
	}
}

func TestSeveritySummaryCountsLevels(t *testing.T) {
	in := "x ERROR y\nz ERROR w\nq WARN r\nFATAL\nUnauthorizedException"
	got := SeveritySummary(in)
	for _, want := range []string{"FATAL=1", "ERROR=2", "WARN=1", "Exception=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
	if s := SeveritySummary("classpath = /x/error_prone_annotations-2.3.4.jar"); s != "" {
		t.Fatalf("lowercase jar name should not count as a level, got %q", s)
	}
}

// These four commands were rejected verbatim during a live run on the
// uhadoop-1v9waqiylmqj cluster; each rejection burned a whole LLM turn.
func TestValidateReadOnlyCommandAcceptsObservedDiagnosticCommands(t *testing.T) {
	allowed := []string{
		`C=/home/hadoop/.versions/hadoop-3.2.1/etc/hadoop; echo "=== core-site ==="; cat $C/core-site.xml 2>/dev/null | grep -iE "superuser|permission"`,
		`ls -la /home/hadoop/conf 2>/dev/null; readlink -f /home/hadoop/conf 2>/dev/null`,
		`grep -iE "superuser|permission|authoriz|property|name>|value>" /home/hadoop/.versions/hadoop-3.2.1/etc/hadoop/core-site.xml`,
		`echo "=== /tmp perms ==="; hdfs dfs -ls -d /tmp /tmp/hive 2>&1`,
	}
	for _, cmd := range allowed {
		if err := ValidateReadOnlyCommand(cmd); err != nil {
			t.Errorf("ValidateReadOnlyCommand(%q) = %v, want nil", cmd, err)
		}
	}
}

func TestValidateReadOnlyCommandStillRejectsDangerousForms(t *testing.T) {
	rejected := []string{
		`C=/home/hadoop; rm -rf /data`,
		`PATH=/tmp/evil; grep x /var/log/y`,
		`LD_PRELOAD=/tmp/x.so cat /var/log/hdfs.log`,
		`echo hi > /etc/passwd`,
		`cat /var/log/hdfs.log | tee /tmp/x`,
		`sudo -u hdfs hdfs dfs -ls /tmp`,
		`echo "$(id)"`,
		`cat /var/log/hdfs.log; C=/a /tmp/evil`,
	}
	for _, cmd := range rejected {
		if err := ValidateReadOnlyCommand(cmd); err == nil {
			t.Errorf("ValidateReadOnlyCommand(%q) = nil, want error", cmd)
		}
	}
}

func TestValidateReadOnlyCommandKeepsQuoteAwareSplitting(t *testing.T) {
	// The "|" is inside double quotes, so it is a regex alternation; if the
	// substitution check clobbered the working copy, this would split into a
	// segment starting with "value>" and be rejected.
	cmd := `grep -E "name>|value>" /var/log/hadoop-hdfs/hdfs-site.xml`
	if err := ValidateReadOnlyCommand(cmd); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
	if err := ValidateReadOnlyCommand(`echo "` + "`" + `id` + "`" + `"`); err == nil {
		t.Fatal("backtick substitution inside double quotes must be rejected")
	}
}

// These commands were also rejected verbatim during live runs.
func TestValidateReadOnlyCommandAcceptsPathsAndAssignments(t *testing.T) {
	allowed := []string{
		`export HADOOP_CONF_DIR=/home/hadoop/.versions/hadoop-3.2.1/etc/hadoop; cat $HADOOP_CONF_DIR/core-site.xml`,
		`/home/hadoop/.versions/hadoop-3.2.1/bin/hdfs dfsadmin -report`,
		`cd /var/log/hadoop-hdfs; ls -l | head -20`,
		`export C=/home/hadoop/conf; grep -r x $C 2>/dev/null`,
	}
	for _, cmd := range allowed {
		if err := ValidateReadOnlyCommand(cmd); err != nil {
			t.Errorf("ValidateReadOnlyCommand(%q) = %v, want nil", cmd, err)
		}
	}
}

func TestValidateReadOnlyCommandRejectsExportsAndPathsThatWidenAccess(t *testing.T) {
	rejected := []string{
		`export PATH=/tmp/evil; ls`,
		`export LD_PRELOAD=/tmp/x.so; cat /var/log/hdfs.log`,
		`/tmp/evil/rm -rf /data`,
		`/bin/rm -rf /data`,
		`cd /var/log; rm -f x`,
		`export A=/a && export PATH=/tmp/x`,
		`export C=/a; /usr/bin/tee /tmp/x`,
	}
	for _, cmd := range rejected {
		if err := ValidateReadOnlyCommand(cmd); err == nil {
			t.Errorf("ValidateReadOnlyCommand(%q) = nil, want error", cmd)
		}
	}
}

func TestValidateReadOnlyCommandAcceptsForLoop(t *testing.T) {
	cmd := `for f in /var/log/hadoop-hdfs/*.log; do echo "=== $f ==="; grep -c ERROR $f; done`
	if err := ValidateReadOnlyCommand(cmd); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

func TestValidateReadOnlyCommandRejectsLoopWithWriteBody(t *testing.T) {
	for _, cmd := range []string{
		`for f in /a/*; do rm -f $f; done`,
		`for f in /a/*; do cat $f; done > /tmp/out`,
		`for f in /a/*; do tee /tmp/x; done`,
	} {
		if err := ValidateReadOnlyCommand(cmd); err == nil {
			t.Errorf("ValidateReadOnlyCommand(%q) = nil, want error", cmd)
		}
	}
}
