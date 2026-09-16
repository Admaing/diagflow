// SSH log access for UHadoop cluster nodes.
//
// Cluster metadata (describe_cluster) hands out each node's IPv4 address plus
// an access_user and a base64-encoded password. That is enough to read logs
// over SSH without any extra agent key, so this is the first implementation of
// the node-log channel; an umrAgent/agent_key transport can be swapped in
// behind the same RemoteExec interface later.
package infra

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// Credential is an SSH login handed out by the access API.
type Credential struct {
	User     string
	Password string
}

// DecodePasswd decodes the base64 password carried on NodeInfo.Passwd.
func DecodePasswd(b64 string) (string, error) {
	if b64 == "" {
		return "", fmt.Errorf("empty password")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode node password: %w", err)
	}
	return string(raw), nil
}

// CredentialFor builds a Credential from a node, decoding the base64 password.
func CredentialFor(n NodeInfo) (Credential, error) {
	pw, err := DecodePasswd(n.Passwd)
	if err != nil {
		return Credential{}, err
	}
	user := n.AccessUser
	if user == "" {
		user = "root"
	}
	return Credential{User: user, Password: pw}, nil
}

// RemoteExec runs one read-only command on a node and returns its stdout.
type RemoteExec interface {
	Run(ctx context.Context, host string, cred Credential, command string) (string, error)
}

// SSHExec runs commands over SSH. Host keys are not verified: cluster nodes
// are reached over the private network by IP and carry per-cluster ephemeral
// keys, so there is no stable known_hosts to pin against.
type SSHExec struct {
	Port           int
	Timeout        time.Duration
	MaxOutputBytes int
	clientConfig   func(cred Credential) *ssh.ClientConfig
}

// NewSSHExec builds an SSH executor with sane defaults (port 22, 20s, 1 MiB).
func NewSSHExec() *SSHExec {
	return &SSHExec{Port: 22, Timeout: 20 * time.Second, MaxOutputBytes: 1 << 20}
}

func (e *SSHExec) config(cred Credential) *ssh.ClientConfig {
	if e.clientConfig != nil {
		return e.clientConfig(cred)
	}
	return &ssh.ClientConfig{
		User:            cred.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cred.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // private-network nodes, ephemeral keys
		Timeout:         e.Timeout,
	}
}

// hostPort renders a dial address, bracketing IPv6 literals.
func hostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Run opens an SSH session, executes command, and caps the captured output.
func (e *SSHExec) Run(ctx context.Context, host string, cred Credential, command string) (string, error) {
	port := e.Port
	if port == 0 {
		port = 22
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	maxOut := e.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = 1 << 20
	}
	addr := hostPort(host, port)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)

	go func() {
		client, err := ssh.Dial("tcp", addr, e.config(cred))
		if err != nil {
			done <- result{err: fmt.Errorf("ssh dial %s: %w", addr, err)}
			return
		}
		defer client.Close()
		session, err := client.NewSession()
		if err != nil {
			done <- result{err: fmt.Errorf("ssh session %s: %w", addr, err)}
			return
		}
		defer session.Close()

		var stdout, stderr bytes.Buffer
		session.Stdout = &limitedWriter{w: &stdout, n: maxOut}
		session.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
		err = session.Run(command)
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			done <- result{out: stdout.String(), err: fmt.Errorf("ssh %s: %s", addr, msg)}
			return
		}
		done <- result{out: stdout.String()}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(timeout + 5*time.Second):
		return "", fmt.Errorf("ssh %s: timed out after %s", addr, timeout)
	}
}

// limitedWriter silently drops bytes past the cap but reports success so the
// remote command is not killed mid-stream.
type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

// ---- safe log command construction ----

// logPathRe restricts log paths to a conservative, absolute, read-only shape.
// Globs are allowed because real log names embed the instance id and node name
// ("hadoop-hadoop-namenode-<id>-master1.log"), so the only practical way to
// name one is a pattern. The character class is still tight enough that a
// glob cannot smuggle in shell syntax.
var logPathRe = regexp.MustCompile(`^/[A-Za-z0-9._@/+*-]*$`)

// hasGlob reports whether a validated path needs shell expansion.
func hasGlob(p string) bool { return strings.ContainsAny(p, "*") }

// maxLogScanLines bounds how much of a file grep/tail will read on the node.
const maxLogScanLines = 200000

// NoFileMarker is printed by a log command when the target file does not
// exist, so callers can tell "file missing" apart from "no matching lines".
const NoFileMarker = "__DIAGFLOW_NOFILE__"

// BuildLogCommand renders a bounded, read-only tail/grep command.
//
// Without keywords it returns the last maxLines lines; with keywords it scans
// the tail of the file and returns the last matching lines, so a huge log
// never gets read end to end.
func BuildLogCommand(logPath, keywords string, maxLines int) (string, error) {
	if err := ValidateLogPath(logPath); err != nil {
		return "", err
	}
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > 2000 {
		maxLines = 2000
	}
	// A glob must stay unquoted so the remote shell expands it; the path has
	// already passed logPathRe, so it cannot contain shell metacharacters.
	path := shellQuote(logPath)
	if hasGlob(logPath) {
		path = logPath
	}
	var body string
	if strings.TrimSpace(keywords) == "" {
		if hasGlob(logPath) {
			// A glob can match many files; bound the TOTAL output instead of
			// tailing maxLines out of each match.
			body = fmt.Sprintf("tail -q -n %d -- %s | tail -n %d", maxLines, path, maxLines)
		} else {
			body = fmt.Sprintf("tail -n %d -- %s", maxLines, path)
		}
	} else {
		scan := maxLines * 50
		if scan > maxLogScanLines {
			scan = maxLogScanLines
		}
		// Fixed-grep pattern: keywords are treated as an extended regex, but
		// are always single-quoted so they cannot break out into shell syntax.
		tail := "tail"
		if hasGlob(logPath) {
			tail = "tail -q"
		}
		body = fmt.Sprintf("%s -n %d -- %s | grep -E -i -- %s | tail -n %d",
			tail, scan, path, shellQuote(keywords), maxLines)
	}
	// Wrap so an empty result means "no matching lines", while a missing file
	// is unambiguous. Without this the two are indistinguishable.
	// A glob is tested with a listing, because [ -f ] is false for patterns.
	probe := fmt.Sprintf("[ -f %s ]", shellQuote(logPath))
	if hasGlob(logPath) {
		probe = fmt.Sprintf("ls -d %s >/dev/null 2>&1", path)
	}
	return fmt.Sprintf("if %s; then %s; else echo %s; fi",
		probe, body, shellQuote(NoFileMarker)), nil
}

// shellQuote wraps s in single quotes, escaping embedded single quotes. The
// result is always a single literal word for the remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// logDirRe restricts log directories to /var/log and below.
var logDirRe = regexp.MustCompile(`^/var/log(/[A-Za-z0-9._@+-]+)*/?$`)

// DefaultLogDirs are the conventional UHadoop/HDP log roots, probed by
// list_logs when the caller does not name a directory.
var DefaultLogDirs = []string{
	"/var/log/hadoop-hdfs",
	"/var/log/hadoop-yarn",
	"/var/log/hadoop-mapreduce",
	"/var/log/hive",
	"/var/log/hue",
	"/var/log/livy",
	"/var/log/spark",
	"/var/log/zookeeper",
	"/var/log/umr",
	"/var/log/mysql",
}

// ValidateLogDir reports whether a caller-supplied directory may be listed.
func ValidateLogDir(dir string) error {
	if !logDirRe.MatchString(dir) || hasDotDotSegment(dir) {
		return fmt.Errorf("unsafe log directory %q: only /var/log and below is listable", dir)
	}
	return nil
}

// hasDotDotSegment reports whether a path contains a ".." component, which
// would escape an otherwise-valid prefix allowlist.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// BuildListLogsCommand renders a read-only listing of the given directories,
// skipping ones that do not exist so a missing component is not an error.
func BuildListLogsCommand(dirs []string) (string, error) {
	if len(dirs) == 0 {
		return "", fmt.Errorf("no directories to list")
	}
	parts := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if err := ValidateLogDir(dir); err != nil {
			return "", err
		}
		q := shellQuote(dir)
		// Trailing slash makes ls follow symlinked log directories (UHadoop
		// exposes /var/log/hadoop-* as symlinks into /data/.log).
		parts = append(parts, fmt.Sprintf("if [ -d %s ]; then echo %s; ls -l %s; fi",
			q, q, shellQuote(strings.TrimSuffix(dir, "/")+"/")))
	}
	return strings.Join(parts, "; "), nil
}

// ValidateLogPath reports whether a caller-supplied path is safe to read.
func ValidateLogPath(p string) error {
	if !logPathRe.MatchString(p) || hasDotDotSegment(p) {
		return fmt.Errorf("unsafe log path %q: only absolute paths of [A-Za-z0-9._@/+*-] without '..' are allowed", p)
	}
	return nil
}

// LogReader fetches logs from cluster nodes through a RemoteExec transport.
type LogReader struct {
	Exec    RemoteExec
	Timeout time.Duration
}

// ReadLog fetches maxLines of a node's log file, optionally filtered.
func (r *LogReader) ReadLog(ctx context.Context, host string, cred Credential, logPath, keywords string, maxLines int) (string, error) {
	cmd, err := BuildLogCommand(logPath, keywords, maxLines)
	if err != nil {
		return "", err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := r.Exec.Run(ctx, host, cred, cmd)
	if err != nil {
		return out, err
	}
	if strings.Contains(out, NoFileMarker) {
		return "", fmt.Errorf("%s: log file %s does not exist", host, logPath)
	}
	// Empty output is a valid result: no matching lines.
	return CapLineWidth(out, MaxLogLineChars), nil
}

// MaxLogLineChars bounds one returned log line. Java startup banners embed the
// entire multi-megabyte classpath on a single line; two such lines are enough
// to fill the whole result and hide the actual log entries.
const MaxLogLineChars = 2000

// CapLineWidth truncates over-long lines rune-by-rune, so a caller always gets
// a readable record instead of one line that eats the entire output budget.
func CapLineWidth(text string, max int) string {
	if max <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	changed := false
	for i, l := range lines {
		if utf8.RuneCountInString(l) <= max {
			continue
		}
		runes := []rune(l)
		lines[i] = string(runes[:max]) + fmt.Sprintf("…[truncated %d chars]", len(runes)-max)
		changed = true
	}
	if !changed {
		return text
	}
	return strings.Join(lines, "\n")
}

// noiseLineRe matches log lines that are configuration dumps rather than
// diagnostics. Hadoop prints the whole multi-megabyte classpath on one
// STARTUP_MSG line, and jar names such as "error_prone_annotations" or
// "slf4j-api" match a severity search, so a naive grep buries the real hits
// under thousands of characters of noise.
var noiseLineRe = regexp.MustCompile(`(?i)(STARTUP_MSG:|java\.class\.path=|Client environment:java\.class\.path)`)

// FilterLogNoise drops configuration-dump lines from a log snippet and reports
// how many were removed.
func FilterLogNoise(text string) (string, int) {
	if text == "" {
		return "", 0
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	dropped := 0
	for _, l := range lines {
		if noiseLineRe.MatchString(l) {
			dropped++
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n"), dropped
}

// CapSearchSnippet keeps at most maxLines of a search result. Search output
// arrives in chronological order, so the tail carries the freshest evidence.
// The number of suppressed (older) matches is reported instead of silently
// dropped, so a caller knows the snippet is not the whole story.
func CapSearchSnippet(text string, maxLines int) (string, int) {
	if maxLines <= 0 {
		return text, 0
	}
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return "", 0
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= maxLines {
		return trimmed, 0
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n"), len(lines) - maxLines
}

// severityRe captures the log levels that matter for a first-pass triage.
var severityRe = regexp.MustCompile(`\b(FATAL|ERROR|WARN)\b`)

// SeveritySummary renders a compact one-line histogram ("ERROR=2 WARN=5") of
// the levels present in a snippet, so a caller can judge blast radius without
// reading every line. Empty when no level token appears.
func SeveritySummary(text string) string {
	if text == "" {
		return ""
	}
	counts := map[string]int{}
	for _, m := range severityRe.FindAllString(text, -1) {
		counts[m]++
	}
	if len(counts) == 0 {
		return ""
	}
	order := []string{"FATAL", "ERROR", "WARN"}
	parts := make([]string, 0, len(order))
	for _, lvl := range order {
		if n := counts[lvl]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", lvl, n))
		}
	}
	exc := strings.Count(text, "Exception")
	if exc > 0 {
		parts = append(parts, fmt.Sprintf("Exception=%d", exc))
	}
	return strings.Join(parts, " ")
}

// patternRe restricts keyword/pattern arguments (grep patterns, YARN states,
// app ids). It deliberately excludes shell metacharacters; the value is still
// shell-quoted before use, so this is defence in depth.
var patternRe = regexp.MustCompile(`^[A-Za-z0-9._,:*=/@| -]*$`)

// ValidatePattern rejects patterns that contain shell-hostile characters.
func ValidatePattern(p string) error {
	if p == "" {
		return nil
	}
	if !patternRe.MatchString(p) {
		return fmt.Errorf("unsafe pattern %q: only [A-Za-z0-9._,:*=/@| -] are allowed", p)
	}
	return nil
}

// ShellQuote exposes single-quote shell escaping for callers that assemble
// commands from validated arguments.
func ShellQuote(s string) string { return shellQuote(s) }

// readOnlyCommands is the allowlist of command heads ssh_exec may invoke.
var readOnlyCommands = map[string]bool{
	"cat": true, "grep": true, "egrep": true, "find": true, "tail": true,
	"head": true, "ls": true, "ps": true, "df": true, "free": true,
	"du": true, "wc": true, "echo": true, "stat": true, "ss": true,
	"netstat": true, "ip": true, "hostname": true, "uptime": true,
	"uname": true, "awk": true, "sed": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "yarn": true, "pwd": true, "nproc": true,
	"date": true, "id": true, "jar": true, "jps": true, "journalctl": true,
	// Path/process inspection helpers the agent reaches for when a log name
	// embeds a version directory or a symlink. All are read-only.
	"readlink": true, "realpath": true, "basename": true, "dirname": true,
	"lsof": true, "vmstat": true, "pidof": true,
	// "cd" runs no command of its own; every command chained after it is still
	// checked separately.
	"cd": true,
	// hdfs/hadoop are read-only reporting commands (dfsadmin -report,
	// fsck, haadmin -getAllServiceState). Their absence from this list made
	// the agent retry several times per run; every retry costs a whole
	// thinking-mode LLM turn.
	"hdfs": true, "hadoop": true,
	// mysql is admitted only for inline read-only queries; every mysql segment
	// is additionally checked by ValidateMySQLCommand, which rejects writes,
	// multi-statement input, dangerous client options and comments.
	"mysql": true,
}

// forbiddenSubstrings are unambiguous shell constructs that could mutate
// state or escape the allowlist.
var forbiddenSubstrings = []string{
	"| sh", "|sh", "| bash", "sudo", "curl", "wget",
	"python", "perl", "tee ", "mkfs",
}

// forbiddenSubstitutions are command-substitution forms. Unlike operators they
// are live inside double quotes, so they are checked against text where only
// single quotes have been stripped.
var forbiddenSubstitutions = []string{"$(", "`"}

// forbiddenVerbs are matched on word boundaries. Substring matching here
// rejected legitimate commands: "yarn application -list -appStates KILLED"
// contains "kill", and a log grep for the word "killed" is a normal thing to
// do. Each rejection cost a whole thinking-mode LLM turn.
var forbiddenVerbs = regexp.MustCompile(`\b(rm|rmdir|mv|dd|chmod|chown|kill|pkill|shutdown|reboot|truncate|shred|mount|umount|systemctl|service|touch)\b`)

// benignRedirects are stderr redirects that only affect where diagnostics go;
// they cannot write to a file we didn't name. Agents reach for "2>&1" and
// "2>/dev/null" constantly, and rejecting them wasted whole turns.
var benignRedirects = []string{"2>&1", "2>/dev/null", "2> /dev/null", "2> &1"}

// stripQuoted removes quoted spans so the safety checks can inspect only the
// text the shell treats as operators. Whether double quotes count depends on
// the construct:
//
//   - Operators (| ; > &) are inert inside both kinds of quotes, so a quoted
//     ">" in grep -E "name>|value>" is a regex alternation, not a redirect.
//     Checking the raw string rejected such commands outright, and each
//     rejection cost a whole thinking-mode LLM turn.
//   - Command substitution ("$(...)" and backticks) still runs inside double
//     quotes, so callers passing dropDouble=false keep those visible.
func stripQuoted(cmd string, dropDouble bool) string {
	var b strings.Builder
	var quote rune
	for _, c := range cmd {
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'':
			quote = c
		case '"':
			if dropDouble {
				quote = c
			} else {
				b.WriteRune(c)
			}
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// assignmentRe matches a bare shell variable assignment with a literal,
// metacharacter-free value ("C=/home/hadoop/conf"). Agents use one to keep a
// long version-pinned config path readable; the allowlist check rejected the
// whole command line each time, burning a turn. The value cannot contain
// shell syntax, so an assignment cannot smuggle in a second command.
var assignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[A-Za-z0-9._/@:+-]*$`)

// unsafeAssignments are names whose assignment changes how later commands
// resolve binaries or libraries, which could turn a read-only command into an
// arbitrary one.
var unsafeAssignments = map[string]bool{
	"path": true, "ifs": true, "bash_env": true, "env": true,
	"ld_preload": true, "ld_library_path": true, "pythonpath": true,
}

// checkAssignment validates a standalone "NAME=value" segment. Agents use one
// to shorten a long version-pinned path, and rejecting it cost a whole turn.
func checkAssignment(seg string) error {
	if !assignmentRe.MatchString(seg) {
		return nil
	}
	if name, _, _ := strings.Cut(seg, "="); unsafeAssignments[name] {
		return fmt.Errorf("assignment to %q is not allowed", name)
	}
	return nil
}

// commandAllowed reports whether a segment head may run. An absolute path is
// judged by its base name, because agents routinely invoke the version-pinned
// binary directly ("/home/hadoop/.versions/hadoop-3.2.1/bin/hdfs") and the
// full path was rejected as an unknown command. Only the allowlisted base
// names pass, and no write command can create such a binary in the first
// place, so this does not widen what can be executed.
func commandAllowed(head string) bool {
	if readOnlyCommands[head] {
		return true
	}
	if i := strings.LastIndex(head, "/"); i >= 0 {
		return readOnlyCommands[head[i+1:]]
	}
	return false
}

// loopKeywords are the shell words of a "for f in ...; do ...; done" loop,
// which agents use to iterate log files. splitSegments breaks the loop body
// into ordinary segments, so the words that introduce a command ("do") are
// checked against the word that follows, while the words that run nothing
// ("for", "done") pass as-is. Without this the loop was rejected at the first
// segment and cost a whole thinking-mode turn.
var loopKeywords = map[string]bool{"for": true, "done": true}

// ValidateReadOnlyCommand enforces a read-only command allowlist for ssh_exec.
func ValidateReadOnlyCommand(cmd string) error {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return fmt.Errorf("empty command")
	}
	lower := strings.ToLower(trimmed)
	// Drop harmless stderr redirects before the write-redirect check below.
	for _, ok := range benignRedirects {
		lower = strings.ReplaceAll(lower, ok, "")
	}
	// Quoted text is data, never an operator, so operators are checked on a
	// copy with both quote styles removed.
	unsafe := stripQuoted(lower, true)
	for _, bad := range forbiddenSubstrings {
		if strings.Contains(unsafe, bad) {
			return fmt.Errorf("command contains forbidden construct %q", bad)
		}
	}
	// Command substitution survives double quotes, so only single quotes hide
	// it. Keep `lower` intact: splitSegments below is quote-aware and needs the
	// quotes to tell a "|" inside a pattern from a real pipeline.
	subst := stripQuoted(lower, false)
	for _, bad := range forbiddenSubstitutions {
		if strings.Contains(subst, bad) {
			return fmt.Errorf("command contains forbidden construct %q", bad)
		}
	}
	if m := forbiddenVerbs.FindString(unsafe); m != "" {
		return fmt.Errorf("command contains forbidden construct %q", m)
	}
	// Any remaining redirect would write to a path, so keep refusing it.
	if strings.Contains(unsafe, ">") {
		return fmt.Errorf("command contains forbidden construct \">\"")
	}
	// Every pipeline / sequence segment must start with an allowed command.
	for _, seg := range splitSegments(lower) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		// A standalone assignment is a segment too, and it runs no command.
		if len(fields) == 1 {
			if err := checkAssignment(fields[0]); err != nil {
				return err
			}
			if assignmentRe.MatchString(fields[0]) {
				continue
			}
		}
		// "export NAME=value" is the same idea with a keyword in front.
		if fields[0] == "export" && len(fields) == 2 && assignmentRe.MatchString(fields[1]) {
			if err := checkAssignment(fields[1]); err != nil {
				return err
			}
			continue
		}
		// "do" runs the command that follows it, so that word is the head.
		if fields[0] == "do" {
			if len(fields) < 2 {
				continue
			}
			if !commandAllowed(fields[1]) {
				return fmt.Errorf("command %q is not in the read-only allowlist", fields[1])
			}
			if isMySQLCommand(fields[1]) {
				if err := ValidateMySQLCommand(strings.TrimSpace(seg)); err != nil {
					return err
				}
			}
			continue
		}
		if loopKeywords[fields[0]] {
			continue
		}
		if !commandAllowed(fields[0]) {
			return fmt.Errorf("command %q is not in the read-only allowlist", fields[0])
		}
		// A bare "mysql" head is only read-only once its inline query is
		// checked; commandAllowed alone would admit any mysql invocation.
		if isMySQLCommand(fields[0]) {
			if err := ValidateMySQLCommand(strings.TrimSpace(seg)); err != nil {
				return err
			}
		}
	}
	return nil
}

// isMySQLCommand reports whether a command head invokes the mysql client,
// including an absolute path to the version-pinned binary.
func isMySQLCommand(head string) bool {
	if i := strings.LastIndex(head, "/"); i >= 0 {
		head = head[i+1:]
	}
	return head == "mysql"
}

// splitSegments splits a command on ; | && so each part can be checked.
// It is quote-aware: a "|" inside 'a|b' or "a|b" is data, not a pipeline,
// and splitting there rejected legitimate commands like
// grep -iE "safemode|corrupt" -- a real false positive.
func splitSegments(cmd string) []string {
	var (
		segs  []string
		cur   strings.Builder
		quote rune
	)
	runes := []rune(cmd)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if quote != 0 {
			cur.WriteRune(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteRune(c)
		case '|', ';':
			segs = append(segs, cur.String())
			cur.Reset()
		case '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				segs = append(segs, cur.String())
				cur.Reset()
				i++
			} else {
				cur.WriteRune(c)
			}
		default:
			cur.WriteRune(c)
		}
	}
	segs = append(segs, cur.String())
	return segs
}
