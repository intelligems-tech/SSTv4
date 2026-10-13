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
	value, ok := decodeAll(data)
	if !ok {
		return "", false
	}
	var out strings.Builder
	write(&out, unwrap(value, 0), 0)
	return out.String(), true
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
	trimmed := strings.TrimSpace(line)
	for start := 0; start < len(trimmed); start++ {
		if trimmed[start] != '{' && trimmed[start] != '[' {
			continue
		}
		value, ok := decodeAll([]byte(trimmed[start:]))
		if !ok {
			continue
		}
		if !isContainer(value) {
			continue
		}
		var out strings.Builder
		prefix := strings.TrimRight(trimmed[:start], " ")
		if prefix != "" {
			out.WriteString(prefix)
			out.WriteString(" ")
		}
		write(&out, unwrap(value, 0), 0)
		return out.String(), true
	}
	return "", false
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

func write(out *strings.Builder, value interface{}, level int) {
	switch v := value.(type) {
	case object:
		if len(v) == 0 {
			out.WriteString("{}")
			return
		}
		out.WriteString("{\n")
		for i, m := range v {
			out.WriteString(strings.Repeat(indent, level+1))
			writeString(out, m.key)
			out.WriteString(": ")
			write(out, m.value, level+1)
			if i < len(v)-1 {
				out.WriteString(",")
			}
			out.WriteString("\n")
		}
		out.WriteString(strings.Repeat(indent, level))
		out.WriteString("}")
	case []interface{}:
		if len(v) == 0 {
			out.WriteString("[]")
			return
		}
		out.WriteString("[\n")
		for i, item := range v {
			out.WriteString(strings.Repeat(indent, level+1))
			write(out, item, level+1)
			if i < len(v)-1 {
				out.WriteString(",")
			}
			out.WriteString("\n")
		}
		out.WriteString(strings.Repeat(indent, level))
		out.WriteString("]")
	case string:
		writeString(out, v)
	case json.Number:
		out.WriteString(v.String())
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	}
}

func writeString(out *strings.Builder, value string) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(value)
	out.WriteString(strings.TrimSuffix(buf.String(), "\n"))
}
