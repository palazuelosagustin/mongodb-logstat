package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
)

// logFormat is what the first bytes of a file say it contains.
type logFormat int

const (
	formatJSON    logFormat = iota // MongoDB 4.4+ structured log (or an empty file)
	formatLegacy                   // MongoDB 4.2 or older plain-text log
	formatUnknown                  // anything else: FTDC, archives, notes, ...
	formatEmpty                    // empty or whitespace only
)

const sniffBytes = 64 * 1024

var (
	gzipMagic = []byte{0x1f, 0x8b}
	// 2020-01-02T03:04:05.678+0000 I  COMMAND  [conn1] ...
	legacyLinePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\s+[DIWEF]\d?\s+\S+\s+\[`)

	errLegacyTextLog = errors.New("legacy text log format (MongoDB 4.2 or older) is not supported; mongodb-logstat reads structured JSON logs (MongoDB 5.0 and later)")
	errNotMongoLog   = errors.New("not a MongoDB log file (expected structured JSON log lines, optionally gzip-compressed)")
)

// logSource is an open log file, decompressed when it was gzip.
type logSource struct {
	Reader io.Reader
	Format logFormat
	// Fingerprint is a hash of the first MongoDB log line (the first
	// non-empty line when there is none). Files with the same fingerprint
	// are usually copies or snapshots of one log; parseFiles confirms that
	// before skipping lines. Empty for empty files.
	Fingerprint string
	Close       func() error
}

// openLog opens path and returns a reader of its log text, decompressing
// gzip transparently, plus the format detected from the first bytes.
func openLog(path string) (*logSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	closeFn := f.Close
	raw := bufio.NewReaderSize(f, sniffBytes)
	var reader *bufio.Reader = raw
	if head, _ := raw.Peek(len(gzipMagic)); bytes.Equal(head, gzipMagic) {
		gz, err := gzip.NewReader(raw)
		if err != nil {
			f.Close()
			return nil, err
		}
		closeFn = func() error {
			gz.Close()
			return f.Close()
		}
		reader = bufio.NewReaderSize(gz, sniffBytes)
	}
	head, err := reader.Peek(sniffBytes)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		closeFn()
		return nil, err
	}
	src := &logSource{Reader: reader, Format: detectLogFormat(head), Close: closeFn}
	if first := firstLogLine(head); len(first) > 0 {
		sum := sha256.Sum256(first)
		src.Fingerprint = hex.EncodeToString(sum[:])
	}
	return src, nil
}

// firstLogLine returns the first structured MongoDB log line in head, or the
// first non-empty line when there is none. Container logs start with shell
// output that is identical across pods, so it cannot identify a log.
func firstLogLine(head []byte) []byte {
	var firstNonEmpty []byte
	for len(head) > 0 {
		line := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if isStructuredLogLine(line) {
			return line
		}
		if firstNonEmpty == nil {
			firstNonEmpty = line
		}
	}
	return firstNonEmpty
}

// detectLogFormat classifies a file from the lines in its first block. A
// structured line anywhere in the block wins: container logs (Kubernetes
// operator, Docker) often start with entrypoint shell output before mongod
// writes its first JSON line.
func detectLogFormat(head []byte) logFormat {
	sawLine, sawLegacy := false, false
	for len(head) > 0 {
		line := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		sawLine = true
		if isStructuredLogLine(line) {
			return formatJSON
		}
		if legacyLinePattern.Match(line) {
			sawLegacy = true
		}
	}
	switch {
	case !sawLine:
		return formatEmpty
	case sawLegacy:
		return formatLegacy
	default:
		return formatUnknown
	}
}

// maxLinePrefix bounds the prefix that may precede a structured log line:
// the RFC3339Nano timestamp added by "kubectl logs --timestamps" or
// "docker logs -t", or a syslog/journald header such as
// "Nov 02 10:13:11 host mongod[2619]: ".
const maxLinePrefix = 256

var syslogDocStart = []byte(` {"t":{"$date"`)

// structuredStart returns the offset where the structured log document starts
// in line, or -1 when the line is not a structured log line.
func structuredStart(line []byte) int {
	if len(line) > 1 && line[0] == '{' && line[1] == '"' {
		return 0
	}
	if len(line) == 0 {
		return -1
	}
	// One leading token that starts with a digit (a timestamp), then a space.
	if line[0] >= '0' && line[0] <= '9' {
		if sp := bytes.IndexByte(line, ' '); sp > 0 && sp <= maxLinePrefix && sp+2 < len(line) && line[sp+1] == '{' && line[sp+2] == '"' {
			return sp + 1
		}
	}
	// Any prefix (syslog, journald) followed by a MongoDB log document.
	window := line
	if len(window) > maxLinePrefix+len(syslogDocStart) {
		window = window[:maxLinePrefix+len(syslogDocStart)]
	}
	if i := bytes.Index(window, syslogDocStart); i >= 0 {
		return i + 1
	}
	return -1
}

var logTimestampField = []byte(`"t":{"$date"`)

// isStructuredLogLine is the stricter check used to classify files: the line
// must be a JSON document carrying a MongoDB log timestamp, so JSON or YAML
// files that merely contain "{" are not taken for logs.
func isStructuredLogLine(line []byte) bool {
	start := structuredStart(line)
	return start >= 0 && bytes.Contains(line[start:], logTimestampField)
}

// sniffLogFile reports the format of the file at path without reading past
// the first block.
func sniffLogFile(path string) (logFormat, error) {
	src, err := openLog(path)
	if err != nil {
		return formatUnknown, err
	}
	src.Close()
	return src.Format, nil
}
