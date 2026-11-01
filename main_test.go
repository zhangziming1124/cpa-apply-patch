package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func parse(t *testing.T, s string) gjson.Result {
	t.Helper()
	return gjson.Parse(s)
}

// replay feeds arguments JSON into a fresh entry in chunks of n bytes and
// returns the concatenation of all emitted deltas.
func replay(t *testing.T, args string, n int) string {
	t.Helper()
	e := &trackEntry{}
	var out strings.Builder
	for i := 0; i < len(args); i += n {
		end := i + n
		if end > len(args) {
			end = len(args)
		}
		if e.phase == decodeDone || e.phase == decodeFailed {
			break
		}
		out.WriteString(e.consume(args[i:end]))
	}
	return out.String()
}

func TestIncrementalDecodeMatchesDone(t *testing.T) {
	patches := []string{
		"*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch",
		"line with \"quotes\" and \\ backslash\n\tindent",
		"中文注释 + tab\t + emoji \U0001F600 pair",
		strings.Repeat("x", 5000) + "\n" + strings.Repeat("\t indent line\n", 200),
	}
	for _, patch := range patches {
		raw, _ := json.Marshal(map[string]string{"input": patch})
		for _, n := range []int{1, 3, 7, 100, len(raw)} {
			got := replay(t, string(raw), n)
			if got != patch {
				t.Fatalf("chunk=%d: delta concat != done value\n got=%q\nwant=%q", n, got, patch)
			}
		}
	}
}

func TestDecodeFailedGoesQuiet(t *testing.T) {
	// Invalid escape flips to failed and stops emitting; must not panic.
	e := &trackEntry{}
	_ = e.consume(`{"input": "ab\q`)
	if e.phase != decodeFailed {
		t.Fatalf("expected decodeFailed, got %d", e.phase)
	}
}

func TestBridgedFingerprint(t *testing.T) {
	tool, _ := json.Marshal(customToolAsFunction("apply_patch", ""))
	if !isBridgedFunctionSchema(parse(t, string(tool))) {
		t.Fatal("own bridged tool must match fingerprint")
	}
	foreign := `{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"]}}`
	if isBridgedFunctionSchema(parse(t, foreign)) {
		t.Fatal("foreign schema must not match fingerprint")
	}
}
