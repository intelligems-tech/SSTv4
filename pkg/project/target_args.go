package project

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"golang.org/x/exp/slices"
)

// Excluding all resources runs the program without planning creates, updates,
// replacements or deletes. This is available only for previews.
func targetArguments(input *StackInput, resources []apitype.ResourceV3) ([]string, error) {
	if input.ExcludeAll {
		if input.Command != "diff" || len(input.Target) > 0 || len(input.Exclude) > 0 {
			return nil, fmt.Errorf("--exclude-all requires diff without --target or --exclude")
		}
		return []string{"--exclude", "**"}, nil
	}
	var args []string
	for _, name := range input.Target {
		index := slices.IndexFunc(resources, func(res apitype.ResourceV3) bool {
			return res.URN.Name() == name
		})
		if index == -1 {
			return nil, fmt.Errorf("Target not found: %v", name)
		}
		args = append(args, "--target", string(resources[index].URN))
	}
	if len(input.Target) > 0 {
		args = append(args, "--target-dependents")
	}
	return args, nil
}
