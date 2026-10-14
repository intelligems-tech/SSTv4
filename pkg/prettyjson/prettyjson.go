// Package prettyjson pretty prints JSON for the dev UI. Key order and number
// text are kept as written, and a string whose content is itself JSON (an
// encoded body, a double encoded payload) is decoded and printed in place,
// however deeply it is nested.
package prettyjson

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Env turns pretty JSON on for `sst dev` and the processes it starts.
const Env = "SST_PRETTY_JSON"

// Enabled reports whether pretty JSON mode is on.
func Enabled() bool {
	switch strings.ToLower(os.Getenv(Env)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// maxDepth bounds how many encoded layers are unwrapped and how deep values
// nest, so hostile input cannot recurse without end.
const maxDepth = 64

const indent = "  "

type member struct {
	key   string
	value interface{}
}

// object keeps members in the order they were written.
type object []member

// Format pretty prints a JSON document, unwrapping JSON nested in strings.
// It reports false when data is not a single JSON value.
func Format(data []byte) (string, bool) {
	lines, ok := FormatLines(data)
	if !ok {
		return "", false
	}
	return Plain(lines), true
}

// FormatOr pretty prints data, or returns it unchanged when it is not JSON.
func FormatOr(data []byte) string {
	if formatted, ok := Format(data); ok {
		return formatted
	}
	return string(data)
}

// FormatLine pretty prints a log line that is JSON, or that ends in a JSON
// object or array after some text (`INFO {"a":1}`). Lines with no such JSON
// are reported false.
func FormatLine(line string) (string, bool) {
	lines, ok := FormatLogLine(line)
	if !ok {
		return "", false
	}
	return Plain(lines), true
}

// FormatLines is Format as coloured segments, one Line per output row.
func FormatLines(data []byte) ([]Line, bool) {
	value, ok := decodeAll(data)
	if !ok {
		return nil, false
	}
	e := &emitter{}
	e.value(unwrap(value, 0), 0)
	return e.done(), true
}

// FormatLogLine is FormatLine as coloured segments; the text before the JSON
// is a Text segment.
func FormatLogLine(line string) ([]Line, bool) {
	trimmed := strings.TrimSpace(line)
	for start := 0; start < len(trimmed); start++ {
		if trimmed[start] != '{' && trimmed[start] != '[' {
			continue
		}
		value, ok := decodeAll([]byte(trimmed[start:]))
		if !ok || !isContainer(value) {
			continue
		}
		e := &emitter{}
		if prefix := strings.TrimRight(trimmed[:start], " "); prefix != "" {
			e.emit(Text, prefix+" ")
		}
		e.value(unwrap(value, 0), 0)
		return e.done(), true
	}
	return nil, false
}

func isContainer(value interface{}) bool {
	switch value.(type) {
	case object, []interface{}:
		return true
	}
	return false
}

// decodeAll decodes exactly one JSON value; trailing data is a failure.
func decodeAll(data []byte) (interface{}, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return value, true
}

func decodeValue(decoder *json.Decoder, depth int) (interface{}, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	if depth >= maxDepth {
		return nil, errTooDeep
	}
	switch delim {
	case '{':
		result := object{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, member{key: key.(string), value: value})
		}
		_, err := decoder.Token()
		return result, err
	case '[':
		result := []interface{}{}
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		_, err := decoder.Token()
		return result, err
	}
	return nil, errUnexpected
}

type decodeError string

func (e decodeError) Error() string { return string(e) }

const (
	errTooDeep    = decodeError("json nests too deeply")
	errUnexpected = decodeError("unexpected json delimiter")
)

// unwrap replaces every string holding a JSON object, array or encoded
// string with its decoded value, repeatedly, so `"\"{\\\"a\\\":1}\""` becomes
// `{"a":1}`.
func unwrap(value interface{}, depth int) interface{} {
	if depth >= maxDepth {
		return value
	}
	switch v := value.(type) {
	case object:
		result := make(object, len(v))
		for i, m := range v {
			result[i] = member{key: m.key, value: unwrap(m.value, depth+1)}
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = unwrap(item, depth+1)
		}
		return result
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return v
		}
		switch trimmed[0] {
		case '{', '[', '"':
		default:
			return v
		}
		decoded, ok := decodeAll([]byte(trimmed))
		if !ok {
			return v
		}
		if _, isString := decoded.(string); !isString && !isContainer(decoded) {
			return v
		}
		return unwrap(decoded, depth+1)
	}
	return value
}

// Kind is what a segment of formatted JSON is, for colouring.
type Kind int

const (
	Punct Kind = iota
	Key
	String
	Number
	Bool
	Null
	// Text is not JSON: the log text before a JSON value.
	Text
)

// Segment is a run of formatted output of one kind.
type Segment struct {
	Kind Kind
	Text string
}

// Line is one row of formatted output.
type Line []Segment

// Plain joins lines without colour.
func Plain(lines []Line) string {
	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteString("\n")
		}
		for _, segment := range line {
			out.WriteString(segment.Text)
		}
	}
	return out.String()
}

// ANSI colours for each kind, from the terminal's own 16 colour palette.
var ansiColors = map[Kind]string{
	Punct:  "\x1b[90m",
	Key:    "\x1b[36m",
	String: "\x1b[32m",
	Number: "\x1b[33m",
	Bool:   "\x1b[35m",
	Null:   "\x1b[35m",
}

// ANSI joins lines with terminal colours, or plain when NO_COLOR is set.
func ANSI(lines []Line) string {
	if os.Getenv("NO_COLOR") != "" {
		return Plain(lines)
	}
	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteString("\n")
		}
		for _, segment := range line {
			color, ok := ansiColors[segment.Kind]
			if !ok {
				out.WriteString(segment.Text)
				continue
			}
			out.WriteString(color)
			out.WriteString(segment.Text)
			out.WriteString("\x1b[39m")
		}
	}
	return out.String()
}

type emitter struct {
	lines []Line
	line  Line
}

func (e *emitter) emit(kind Kind, text string) {
	e.line = append(e.line, Segment{Kind: kind, Text: text})
}

func (e *emitter) newline(level int) {
	e.lines = append(e.lines, e.line)
	e.line = Line{}
	if level > 0 {
		e.emit(Punct, strings.Repeat(indent, level))
	}
}

func (e *emitter) done() []Line {
	return append(e.lines, e.line)
}

func (e *emitter) value(value interface{}, level int) {
	switch v := value.(type) {
	case object:
		if len(v) == 0 {
			e.emit(Punct, "{}")
			return
		}
		e.emit(Punct, "{")
		for i, m := range v {
			e.newline(level + 1)
			e.emit(Key, quote(m.key))
			e.emit(Punct, ": ")
			e.value(m.value, level+1)
			if i < len(v)-1 {
				e.emit(Punct, ",")
			}
		}
		e.newline(level)
		e.emit(Punct, "}")
	case []interface{}:
		if len(v) == 0 {
			e.emit(Punct, "[]")
			return
		}
		e.emit(Punct, "[")
		for i, item := range v {
			e.newline(level + 1)
			e.value(item, level+1)
			if i < len(v)-1 {
				e.emit(Punct, ",")
			}
		}
		e.newline(level)
		e.emit(Punct, "]")
	case string:
		e.emit(String, quote(v))
	case json.Number:
		e.emit(Number, v.String())
	case bool:
		if v {
			e.emit(Bool, "true")
		} else {
			e.emit(Bool, "false")
		}
	case nil:
		e.emit(Null, "null")
	}
}

func quote(value string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(value)
	return strings.TrimSuffix(buf.String(), "\n")
}
