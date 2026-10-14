package multiplexer

import "testing"

func TestParseStatusSegments(t *testing.T) {
	got := ParseStatusSegments(" resources=dev-3 ,, frontend = local,bare")
	want := []StatusSegment{{Key: "resources", Value: "dev-3"}, {Key: "frontend", Value: "local"}, {Value: "bare"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(ParseStatusSegments("")) != 0 {
		t.Fatal("an empty status has no segments")
	}
}

func TestStatusLinesOrderAlertsFirstAndOmitEmptyParts(t *testing.T) {
	lines := Status{Stage: "x-local", Alerts: []string{"stuck q"}, Pool: &PoolStatus{Workers: 1}}.lines()
	if len(lines) != 3 || lines[0].left != "⚠ stuck q" || lines[1].right != "x-local" || lines[2].left != "pool" {
		t.Fatalf("got %+v", lines)
	}
	if len(Status{}.lines()) != 0 {
		t.Fatal("an empty status draws nothing")
	}
}
