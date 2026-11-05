package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
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
)

const sniffBytes = 64 * 1024

var (
	gzipMagic = []byte{0x1f, 0x8b}
	// 2020-01-02T03:04:05.678+0000 I  COMMAND  [conn1] ...
	legacyLinePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\s+[DIWEF]\d?\s+\S+\s+\[`)

	errLegacyTextLog = errors.New("legacy text log format (MongoDB 4.2 or older) is not supported; mongodb-logstat reads structured JSON logs (MongoDB 5.0 and later)")
	errNotMongoLog   = errors.New("not a MongoDB log file (expected structured JSON log lines, optionally gzip-compressed)")
)

// openLog opens path and returns a reader of its log text, decompressing
// gzip transparently, plus the format detected from the first bytes.
func openLog(path string) (io.Reader, logFormat, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, formatUnknown, nil, err
	}
	closeFn := f.Close
	raw := bufio.NewReaderSize(f, sniffBytes)
	var reader *bufio.Reader = raw
	if head, _ := raw.Peek(len(gzipMagic)); bytes.Equal(head, gzipMagic) {
		gz, err := gzip.NewReader(raw)
		if err != nil {
			f.Close()
			return nil, formatUnknown, nil, err
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
		return nil, formatUnknown, nil, err
	}
	return reader, detectLogFormat(head), closeFn, nil
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
		return formatJSON
	case sawLegacy:
		return formatLegacy
	default:
		return formatUnknown
	}
}

// maxLinePrefix bounds the prefix that may precede a structured log line,
// such as the RFC3339Nano timestamp added by "kubectl logs --timestamps" or
// "docker logs -t".
const maxLinePrefix = 48

// structuredStart returns the offset where the structured log document starts
// in line, or -1 when the line is not a structured log line. One leading
// token that starts with a digit (a timestamp) followed by a space is allowed.
func structuredStart(line []byte) int {
	if len(line) > 1 && line[0] == '{' && line[1] == '"' {
		return 0
	}
	if len(line) == 0 || line[0] < '0' || line[0] > '9' {
		return -1
	}
	sp := bytes.IndexByte(line, ' ')
	if sp <= 0 || sp > maxLinePrefix || sp+2 >= len(line) || line[sp+1] != '{' || line[sp+2] != '"' {
		return -1
	}
	return sp + 1
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
	_, format, closeFn, err := openLog(path)
	if err != nil {
		return formatUnknown, err
	}
	closeFn()
	return format, nil
}
