package gqlsec

import (
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

func parse(t *testing.T, q string) *ast.QueryDocument {
	t.Helper()
	doc, err := parser.ParseQuery(&ast.Source{Input: q})
	if err != nil {
		t.Fatalf("query failed to parse: %v", err)
	}
	return doc
}

func TestSelectionDepth(t *testing.T) {
	doc := parse(t, `query { a { b { c } } }`)
	if d := selectionDepth(doc.Operations[0].SelectionSet, doc, map[string]bool{}); d != 3 {
		t.Errorf("expected depth 3, got %d", d)
	}
	doc = parse(t, `query { ...F } fragment F on Query { a { b } }`)
	if d := selectionDepth(doc.Operations[0].SelectionSet, doc, map[string]bool{}); d != 2 {
		t.Errorf("fragment depth: expected 2, got %d", d)
	}
	doc = parse(t, `query { __schema { types { name } } }`)
	if d := selectionDepth(doc.Operations[0].SelectionSet, doc, map[string]bool{}); d != 0 {
		t.Errorf("introspection must not count, got %d", d)
	}
}
