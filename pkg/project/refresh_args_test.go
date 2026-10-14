package project

import (
	"reflect"
	"testing"
)

func TestRefreshImportsOnePendingCreate(t *testing.T) {
	urn := "urn:pulumi:dev::app::aws:s3/bucketObjectv2:BucketObjectv2::BridgeCode"
	args, err := refreshArguments(&StackInput{PendingCreateURN: urn, PendingCreateID: "bucket/assets/code.zip"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"refresh", "--yes", "--run-program", "--import-pending-creates", urn, "--import-pending-creates", "bucket/assets/code.zip"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestRefreshRejectsIncompletePendingCreate(t *testing.T) {
	for _, input := range []*StackInput{{PendingCreateURN: "urn:pulumi:dev::app::type::name"}, {PendingCreateID: "bucket/key"}, {PendingCreateURN: "not-a-urn", PendingCreateID: "bucket/key"}} {
		if _, err := refreshArguments(input); err == nil {
			t.Fatalf("accepted invalid pending create: %+v", input)
		}
	}
}

func TestRefreshWithoutRecoveryKeepsExistingArguments(t *testing.T) {
	args, err := refreshArguments(&StackInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"refresh", "--yes", "--run-program"}) {
		t.Fatal(args)
	}
}
