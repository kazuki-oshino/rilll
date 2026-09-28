package kiro

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
)

var errEmptySessionBPersona = errors.New("session B persona: empty content")
var errEmptyAnswerPolicy = errors.New("answer policy: empty content")

func sessionBPersonaFacetName(name string) (string, bool) {
	switch name {
	case "architecture-quality":
		return "architecture-quality", true
	case implPersonaName:
		return "impl-direction-answerer", true
	default:
		return "", false
	}
}

func resolveSessionBPersona(cwd, name string) (inquiryloop.AnswerPersona, error) {
	facetName, ok := sessionBPersonaFacetName(name)
	if !ok {
		return inquiryloop.AnswerPersona{}, fmt.Errorf("session B persona %q is not mapped to a Persona facet: %w", name, facet.ErrFacetNotFound)
	}

	resolved, err := facet.ResolveFacet(cwd, facet.Persona, facetName)
	if err != nil {
		return inquiryloop.AnswerPersona{}, fmt.Errorf("session B persona %q (facet %q): resolve facet: %w", name, facetName, err)
	}
	if resolved.Content == "" {
		return inquiryloop.AnswerPersona{}, errEmptySessionBPersona
	}
	return inquiryloop.AnswerPersona{
		Name:       name,
		SourcePath: resolved.SourcePath,
		Content:    resolved.Content,
	}, nil
}

func resolveAnswerPolicy(cwd, name string) (inquiryloop.AnswerPolicy, error) {
	resolved, err := facet.ResolveFacet(cwd, facet.Policy, name)
	if err != nil {
		return inquiryloop.AnswerPolicy{}, fmt.Errorf("answer policy %q: resolve facet: %w", name, err)
	}
	if strings.TrimSpace(resolved.Content) == "" {
		return inquiryloop.AnswerPolicy{}, errEmptyAnswerPolicy
	}
	return inquiryloop.AnswerPolicy{
		Name:       name,
		SourcePath: resolved.SourcePath,
		Content:    resolved.Content,
	}, nil
}

func resolveAnswerPolicies(cwd string, names ...string) ([]inquiryloop.AnswerPolicy, error) {
	policies := make([]inquiryloop.AnswerPolicy, 0, len(names))
	for _, name := range names {
		policy, err := resolveAnswerPolicy(cwd, name)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

func formatSessionBPersonaError(name string, err error) string {
	facetName, ok := sessionBPersonaFacetName(name)
	if !ok {
		return fmt.Sprintf(
			"session B persona %q is not supported; expected one of: architecture-quality, impl-direction. Error: %v",
			name,
			err,
		)
	}

	expected := fmt.Sprintf(
		"<cwd>/.rilll-config/facets/personas/%s.md or embed:facets/personas/%s.md",
		facetName,
		facetName,
	)
	switch {
	case errors.Is(err, errEmptySessionBPersona):
		return fmt.Sprintf(
			"session B persona %q (facet %q) is empty; expected non-empty content at %s. Error: %v",
			name,
			facetName,
			expected,
			err,
		)
	case errors.Is(err, facet.ErrFacetNotFound):
		return fmt.Sprintf(
			"session B persona %q (facet %q) was not found; expected %s. Error: %v",
			name,
			facetName,
			expected,
			err,
		)
	default:
		return fmt.Sprintf(
			"session B persona %q (facet %q) resolution failed; expected %s. Error: %v",
			name,
			facetName,
			expected,
			err,
		)
	}
}
