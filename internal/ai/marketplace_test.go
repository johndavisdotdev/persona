package ai

import "testing"

func TestNormalizeSource(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/mkt":     "acme/mkt",
		"https://github.com/acme/mkt.git": "acme/mkt",
		"git@github.com:acme/mkt.git":     "acme/mkt",
		"acme/mkt":                        "acme/mkt",
	} {
		if got := NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchRanksNameMatches(t *testing.T) {
	c := NewCatalog()
	c.byID["m"] = &Marketplace{Name: "m", Plugins: []CatalogPlugin{
		{Name: "misc", Description: "mentions linear once"},
		{Name: "linear", Description: "Linear issue tracking"},
	}}
	hits := c.Search("linear issues", "", 5)
	if len(hits) != 2 || hits[0].ID != "linear@m" {
		t.Fatalf("unexpected ranking: %+v", hits)
	}
}
