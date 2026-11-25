// Package gqlsec contains GraphQL hardening: query depth limiting.
package gqlsec

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// DepthLimit rejects operations nested deeper than Max levels. Introspection fields are
// not counted (they are controlled separately by disabling introspection in production).
type DepthLimit struct{ Max int }

var _ interface {
	graphql.HandlerExtension
	graphql.OperationInterceptor
} = DepthLimit{}

func (DepthLimit) ExtensionName() string                     { return "DepthLimit" }
func (DepthLimit) Validate(_ graphql.ExecutableSchema) error { return nil }

func (d DepthLimit) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	oc := graphql.GetOperationContext(ctx)
	if oc != nil && oc.Operation != nil && oc.Doc != nil {
		if depth := selectionDepth(oc.Operation.SelectionSet, oc.Doc, map[string]bool{}); depth > d.Max {
			return func(ctx context.Context) *graphql.Response {
				return &graphql.Response{Errors: gqlerror.List{
					gqlerror.Errorf("query depth %d exceeds maximum of %d", depth, d.Max),
				}}
			}
		}
	}
	return next(ctx)
}

func selectionDepth(set ast.SelectionSet, doc *ast.QueryDocument, visiting map[string]bool) int {
	max := 0
	for _, sel := range set {
		depth := 0
		switch s := sel.(type) {
		case *ast.Field:
			if len(s.Name) >= 2 && s.Name[:2] == "__" {
				continue
			}
			depth = 1 + selectionDepth(s.SelectionSet, doc, visiting)
		case *ast.InlineFragment:
			depth = selectionDepth(s.SelectionSet, doc, visiting)
		case *ast.FragmentSpread:
			if visiting[s.Name] { // cyclic fragments are rejected by validation; guard anyway
				depth = 1 << 20
			} else if frag := doc.Fragments.ForName(s.Name); frag != nil {
				visiting[s.Name] = true
				depth = selectionDepth(frag.SelectionSet, doc, visiting)
				delete(visiting, s.Name)
			}
		}
		if depth > max {
			max = depth
		}
	}
	return max
}
