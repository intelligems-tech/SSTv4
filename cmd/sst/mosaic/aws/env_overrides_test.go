package aws

import (
	"reflect"
	"sort"
	"testing"
)

func TestApplyDevEnvOverridesListedKeysWin(t *testing.T) {
	forwarded := []string{"RESOURCE_ENV=prod", "OTHER=keep", "AWS_REGION=us-east-1"}
	process := []string{"RESOURCE_ENV=dev-4", "OTHER=ignored", "FRONTEND_ENV=local", "PATH=/bin"}
	got := ApplyDevEnvOverrides(forwarded, process, "RESOURCE_ENV, FRONTEND_ENV")
	sort.Strings(got)
	want := []string{"AWS_REGION=us-east-1", "FRONTEND_ENV=local", "OTHER=keep", "RESOURCE_ENV=dev-4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestApplyDevEnvOverridesUnsetKeysAreIgnored(t *testing.T) {
	forwarded := []string{"RESOURCE_ENV=prod"}
	got := ApplyDevEnvOverrides(forwarded, []string{"PATH=/bin"}, "RESOURCE_ENV")
	if !reflect.DeepEqual(got, forwarded) {
		t.Fatalf("got %v want %v", got, forwarded)
	}
}

func TestApplyDevEnvOverridesNoKeysIsIdentity(t *testing.T) {
	forwarded := []string{"A=1"}
	got := ApplyDevEnvOverrides(forwarded, []string{"A=2"}, "")
	if !reflect.DeepEqual(got, forwarded) {
		t.Fatalf("got %v want %v", got, forwarded)
	}
}

func TestEnvMapLaterEntriesWin(t *testing.T) {
	got := envMap([]string{"A=1", "B=x=y", "A=2"})
	if got["A"] != "2" || got["B"] != "x=y" {
		t.Fatalf("got %v", got)
	}
}
