package project

import (
	"reflect"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

func TestDeclarationPreviewExcludesAllResourceChanges(t *testing.T) {
	args, err := targetArguments(&StackInput{Command: "diff", ExcludeAll: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--exclude", "**"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestNormalTargetsStillIncludeDependents(t *testing.T) {
	urn := resource.URN("urn:pulumi:prod::backend::random:index/randomBytes:RandomBytes::LambdaEncryptionKey")
	args, err := targetArguments(&StackInput{Command: "diff", Target: []string{"LambdaEncryptionKey"}}, []apitype.ResourceV3{{URN: urn}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--target", string(urn), "--target-dependents"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestExcludeAllRejectsWritesAndTargetCombinations(t *testing.T) {
	for _, input := range []*StackInput{
		{Command: "deploy", ExcludeAll: true},
		{Command: "diff", Target: []string{"Key"}, ExcludeAll: true},
		{Command: "diff", Exclude: []string{"Key"}, ExcludeAll: true},
	} {
		if _, err := targetArguments(input, nil); err == nil {
			t.Fatalf("accepted invalid exclude-all request: %+v", input)
		}
	}
}

func TestTargetsMustExist(t *testing.T) {
	if _, err := targetArguments(&StackInput{Command: "diff", Target: []string{"Missing"}}, nil); err == nil {
		t.Fatal("accepted missing target")
	}
}
