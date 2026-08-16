package util

import "testing"

func TestExtractCookieValueByKey(t *testing.T) {
	cookie := "a=1; tenant_id = acme-corp ;b=2"
	if got := ExtractCookieValueByKey(cookie, "tenant_id"); got != "acme-corp" {
		t.Errorf("want acme-corp, got %q", got)
	}
	if got := ExtractCookieValueByKey(cookie, "missing"); got != "" {
		t.Errorf("want empty, got %q", got)
	}
	if got := ExtractCookieValueByKey("", "tenant_id"); got != "" {
		t.Errorf("want empty for empty cookie, got %q", got)
	}
}

func TestHasAnySuffix(t *testing.T) {
	suffixes := []string{"/completions", "/messages"}
	cases := map[string]bool{
		"/v1/chat/completions":          true,
		"/v1/chat/completions?stream=1": true,
		"/v1/messages":                  true,
		"/v1/embeddings":                false,
		"/completionsX":                 false,
	}
	for path, want := range cases {
		if got := HasAnySuffix(path, suffixes); got != want {
			t.Errorf("%s: want %v, got %v", path, want, got)
		}
	}
	if !HasAnySuffix("/anything", []string{"*"}) {
		t.Errorf("wildcard should match everything")
	}
}
