package project

import (
	"fmt"
	"strings"
)

func refreshArguments(input *StackInput) ([]string, error) {
	args := []string{"refresh", "--yes", "--run-program"}
	if input.PendingCreateURN == "" && input.PendingCreateID == "" {
		return args, nil
	}
	if !strings.HasPrefix(input.PendingCreateURN, "urn:pulumi:") || strings.TrimSpace(input.PendingCreateID) == "" {
		return nil, fmt.Errorf("pending-create recovery requires both a full Pulumi URN and its provider ID")
	}
	return append(args, "--import-pending-creates", input.PendingCreateURN, "--import-pending-creates", input.PendingCreateID), nil
}
