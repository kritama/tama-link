package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCatalogValidate(t *testing.T) {
	t.Parallel()

	valid := Catalog{Operations: []Descriptor{
		testDescriptor(t),
		testDescriptor(t, func(d *Descriptor) { d.Name = "other" }),
	}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}

	duplicate := Catalog{Operations: []Descriptor{
		testDescriptor(t),
		testDescriptor(t),
	}}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate operation names accepted, want error")
	}

	invalid := Catalog{Operations: []Descriptor{
		testDescriptor(t, func(d *Descriptor) { d.Name = "" }),
	}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid descriptor accepted, want error")
	}
}

func TestCatalogFindAndNames(t *testing.T) {
	t.Parallel()

	c := Catalog{Operations: []Descriptor{
		testDescriptor(t, func(d *Descriptor) { d.Name = "zeta" }),
		testDescriptor(t, func(d *Descriptor) { d.Name = "alpha" }),
	}}

	names := c.Names()
	if want := []string{"alpha", "zeta"}; !slices.Equal(names, want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}

	got, ok := c.Find("alpha")
	if !ok || got.Name != "alpha" {
		t.Fatalf("Find(alpha) = %+v, %v", got, ok)
	}
	if _, ok := c.Find("missing"); ok {
		t.Fatal("Find(missing) = true, want false")
	}
}

func TestCallableExcludesUnsupportedOperations(t *testing.T) {
	t.Parallel()

	c := Catalog{Operations: []Descriptor{
		testDescriptor(t, func(d *Descriptor) { d.Name = "message" }),
		testDescriptor(t, func(d *Descriptor) {
			d.Name = "review"
			d.Strategy = StrategyUnsupported
		}),
	}}
	if names := c.Callable().Names(); !slices.Equal(names, []string{"message"}) {
		t.Fatalf("Callable names = %v, want [message]", names)
	}
	if !slices.Equal(c.Names(), []string{"message", "review"}) {
		t.Fatal("Callable mutated the pinned catalog")
	}
}

func TestSignaturesProjectClientSchema(t *testing.T) {
	t.Parallel()

	d := testDescriptor(t, func(d *Descriptor) {
		d.ClientSchema = json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"Text"},"kind":{"type":["string","null"],"description":"Optional kind"}},"required":["content"]}`)
	})
	c := Catalog{Operations: []Descriptor{d}}

	sigs := c.Signatures()
	if len(sigs) != 1 {
		t.Fatalf("Signatures() length = %d, want 1", len(sigs))
	}
	sig := sigs[0]
	if sig.Name != "message" || sig.Description != "Send one message to Tama." {
		t.Fatalf("signature = %+v", sig)
	}
	if want := []Argument{
		{Name: "content", Type: "string", Description: "Text", Required: true},
		{Name: "kind", Type: "null|string", Description: "Optional kind"},
	}; !slices.Equal(sig.Arguments, want) {
		t.Fatalf("arguments = %+v, want %+v", sig.Arguments, want)
	}
}

func TestSignaturesFallBackToInputSchema(t *testing.T) {
	t.Parallel()

	c := Catalog{Operations: []Descriptor{testDescriptor(t)}}
	sigs := c.Signatures()

	if want := []Argument{
		{Name: "content", Type: "string", Description: "Message text", Required: true},
		{Name: "recipient", Type: "string"},
	}; !slices.Equal(sigs[0].Arguments, want) {
		t.Fatalf("arguments = %+v, want %+v", sigs[0].Arguments, want)
	}
}

func TestSubmitDescriptionIsBoundedAndDeterministic(t *testing.T) {
	t.Parallel()

	ops := make([]Descriptor, 0, 40)
	for i := 0; i < 40; i++ {
		ops = append(ops, testDescriptor(t, func(d *Descriptor) {
			d.Name = fmt.Sprintf("op%02d", i)
		}))
	}
	c := Catalog{Operations: ops}

	first := c.SubmitDescription()
	if second := c.SubmitDescription(); second != first {
		t.Fatal("SubmitDescription() is not deterministic")
	}
	if !strings.Contains(first, "... 8 more operation(s) omitted") {
		t.Fatalf("description missing omission marker:\n%s", first)
	}
	if len(first) > maxSubmitDescription {
		t.Fatalf("description length %d exceeds %d", len(first), maxSubmitDescription)
	}
	if !strings.HasPrefix(first, "op00:") {
		t.Fatalf("description not sorted by name:\n%s", first)
	}
}
