package workflow

import (
	"context"
	"fmt"
	"strings"
)

// DuplicatePolicy defines how ordered selection handles repeated IDs.
type DuplicatePolicy string

const (
	// DuplicatePolicyReject rejects repeated IDs before any workflow side effect.
	DuplicatePolicyReject DuplicatePolicy = "reject"
)

// OrderedSelectParams is the ordered-list variant of SelectParams.
type OrderedSelectParams struct {
	Key             string
	Prompt          string
	Items           []SelectItem
	MinItems        int
	DuplicatePolicy DuplicatePolicy
}

// OrderedWorkflowAsk is an optional extension for ask implementations that can
// collect an ordered list of selection IDs.
type OrderedWorkflowAsk interface {
	SelectOrdered(context.Context, OrderedSelectParams) ([]string, bool, error)
}

// SelectOrdered delegates to an ask implementation's optional ordered
// selection seam and applies shared validation to the accepted IDs.
func SelectOrdered(ctx context.Context, ask WorkflowAsk, params OrderedSelectParams) ([]string, bool, error) {
	orderedAsk, ok := ask.(OrderedWorkflowAsk)
	if !ok {
		return nil, false, fmt.Errorf("%s is not supported by %T", orderedSelectSubject(params), ask)
	}
	ids, ok, err := orderedAsk.SelectOrdered(ctx, params)
	if err != nil || !ok {
		return ids, ok, err
	}
	if err := ValidateOrderedSelection(params, ids); err != nil {
		return nil, false, err
	}
	return ids, true, nil
}

// ParseOrderedCSV parses a comma-separated ordered selection value.
func ParseOrderedCSV(value string) ([]string, error) {
	tokens := strings.Split(value, ",")
	ids := make([]string, 0, len(tokens))
	for i, token := range tokens {
		id := strings.TrimSpace(token)
		if id == "" {
			return nil, fmt.Errorf("ordered selection contains empty token at position %d", i+1)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ValidateOrderedSelection checks ordered IDs against candidates without
// mutating or sorting the accepted order.
func ValidateOrderedSelection(params OrderedSelectParams, ids []string) error {
	if params.MinItems > 0 && len(ids) < params.MinItems {
		return fmt.Errorf("%s requires at least %d item. Available choices: %s", orderedSelectSubject(params), params.MinItems, joinSelectItems(params.Items))
	}

	candidates := make(map[string]struct{}, len(params.Items))
	for _, item := range params.Items {
		candidates[item.ID] = struct{}{}
	}

	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := candidates[id]; !ok {
			return fmt.Errorf("%s contains unknown feature %q. Available choices: %s", orderedSelectSubject(params), id, joinSelectItems(params.Items))
		}

		switch params.DuplicatePolicy {
		case "", DuplicatePolicyReject:
			if _, exists := seen[id]; exists {
				return fmt.Errorf("%s contains duplicate feature %q. Available choices: %s", orderedSelectSubject(params), id, joinSelectItems(params.Items))
			}
			seen[id] = struct{}{}
		default:
			return fmt.Errorf("%s uses unsupported duplicate policy %q. Available choices: %s", orderedSelectSubject(params), params.DuplicatePolicy, joinSelectItems(params.Items))
		}
	}

	return nil
}

func orderedSelectSubject(params OrderedSelectParams) string {
	if params.Key == "" {
		return "ordered selection"
	}
	return fmt.Sprintf("ordered selection --%s", params.Key)
}
