package delegate

import "testing"

func TestProfileRequiresExplicitModelCap(t *testing.T) {
	valid := Profile{ID: "openai-fixture", Revision: "r1", BillingProvider: "openai", Model: "fixture-model", ContextTokens: 200000, MaxOutputTokens: 16384, OutputTokens: 128}
	if !valid.valid() {
		t.Fatal("fixture profile rejected")
	}
	for _, change := range []func(*Profile){func(p *Profile) { p.MaxOutputTokens = 0 }, func(p *Profile) { p.OutputTokens = p.MaxOutputTokens + 1 }, func(p *Profile) { p.ContextTokens = p.OutputTokens }, func(p *Profile) { p.BillingProvider = "openrouter" }, func(p *Profile) { p.Model = "https://arbitrary.example" }, func(p *Profile) { p.Revision = "" }, func(p *Profile) { p.Model = "auto" }} {
		p := valid
		change(&p)
		if p.valid() {
			t.Fatalf("unknown/unsafe cap/profile accepted: %+v", p)
		}
	}
	updated := valid
	updated.OutputTokens++
	if valid.digest() == updated.digest() {
		t.Fatal("profile mutation kept digest")
	}
}
