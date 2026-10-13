package prettyjson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFormatKeepsKeyOrderAndNumbers(t *testing.T) {
	got, ok := Format([]byte(`{"z":1,"a":[1.50,true,null],"m":{}}`))
	want := "{\n  \"z\": 1,\n  \"a\": [\n    1.50,\n    true,\n    null\n  ],\n  \"m\": {}\n}"
	if !ok || got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFormatUnwrapsJSONNestedInStrings(t *testing.T) {
	inner := `{"id":7,"tags":["a"]}`
	once, _ := json.Marshal(inner)         // "{\"id\":7,...}"
	twice, _ := json.Marshal(string(once)) // "\"{\\\"id\\\":7,...}\""
	thrice, _ := json.Marshal(string(twice))
	for name, encoded := range map[string][]byte{"once": once, "twice": twice, "thrice": thrice} {
		doc := `{"body":` + string(encoded) + `}`
		got, ok := Format([]byte(doc))
		want := "{\n  \"body\": {\n    \"id\": 7,\n    \"tags\": [\n      \"a\"\n    ]\n  }\n}"
		if !ok || got != want {
			t.Fatalf("%s: got\n%s", name, got)
		}
	}
}

func TestFormatLeavesPlainStringsAlone(t *testing.T) {
	got, _ := Format([]byte(`{"a":"hello","b":"{not json","c":"42","d":"\"quoted\"","e":"<b>&"}`))
	for _, want := range []string{`"a": "hello"`, `"b": "{not json"`, `"c": "42"`, `"d": "quoted"`, `"e": "<b>&"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
}

func TestFormatRejectsNonJSON(t *testing.T) {
	for _, input := range []string{``, `hello`, `{"a":1} trailing`, `{"a":`} {
		if _, ok := Format([]byte(input)); ok {
			t.Fatalf("%q must not format", input)
		}
	}
	if FormatOr([]byte("plain")) != "plain" {
		t.Fatal("FormatOr returns non-JSON unchanged")
	}
}

func TestFormatLineFindsTrailingJSON(t *testing.T) {
	got, ok := FormatLine(`2026-10-12T00:00:00Z INFO {"msg":"saved","payload":"{\"n\":1}"}`)
	want := "2026-10-12T00:00:00Z INFO {\n  \"msg\": \"saved\",\n  \"payload\": {\n    \"n\": 1\n  }\n}"
	if !ok || got != want {
		t.Fatalf("got\n%s", got)
	}
	for _, line := range []string{"[INFO] started", "count: 3", "array [1, 2", `done "{}"`} {
		if _, ok := FormatLine(line); ok {
			t.Fatalf("%q has no JSON to format", line)
		}
	}
}

func TestFormatBoundsDepth(t *testing.T) {
	deep := strings.Repeat("[", maxDepth+5) + strings.Repeat("]", maxDepth+5)
	if _, ok := Format([]byte(deep)); ok {
		t.Fatal("documents nested past maxDepth are refused")
	}
}

func TestEnabled(t *testing.T) {
	t.Setenv(Env, "true")
	if !Enabled() {
		t.Fatal("true enables")
	}
	t.Setenv(Env, "0")
	if Enabled() {
		t.Fatal("0 disables")
	}
}

func TestFormatLinesTagsEachKind(t *testing.T) {
	lines, ok := FormatLines([]byte(`{"s":"x","n":1,"b":true,"z":null}`))
	if !ok {
		t.Fatal("valid JSON must format")
	}
	kinds := map[string]Kind{}
	for _, line := range lines {
		for _, segment := range line {
			kinds[segment.Text] = segment.Kind
		}
	}
	want := map[string]Kind{`"s"`: Key, `"x"`: String, `1`: Number, `true`: Bool, `null`: Null, `{`: Punct}
	for text, kind := range want {
		if kinds[text] != kind {
			t.Fatalf("%s tagged %v, want %v", text, kinds[text], kind)
		}
	}
}

func TestFormatLogLineKeepsThePrefixAsText(t *testing.T) {
	lines, ok := FormatLogLine(`INFO {"a":1}`)
	if !ok || lines[0][0] != (Segment{Kind: Text, Text: "INFO "}) {
		t.Fatalf("got %+v", lines)
	}
}

func TestANSIColoursAndHonoursNoColor(t *testing.T) {
	lines, _ := FormatLines([]byte(`{"a":"b"}`))
	t.Setenv("NO_COLOR", "")
	coloured := ANSI(lines)
	if !strings.Contains(coloured, "\x1b[36m\"a\"\x1b[39m") || !strings.Contains(coloured, "\x1b[32m\"b\"\x1b[39m") {
		t.Fatalf("keys must be cyan and strings green: %q", coloured)
	}
	t.Setenv("NO_COLOR", "1")
	if ANSI(lines) != Plain(lines) {
		t.Fatal("NO_COLOR prints plain")
	}
}
