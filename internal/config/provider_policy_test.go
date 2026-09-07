package config

import (
	"strings"
	"testing"
	"time"
)

func TestOperatorProviderPolicyParsing(t *testing.T) {
	for _, raw := range []string{`{}`, `[{}]`, `[{"origin":"http://messages.example"}]`, `[{"origin":"https://messages.example","wildcard":true}]`, `[] []`, strings.Repeat(" ", 16385), `[{"origin":"https://messages.example"},{"origin":"https://MESSAGES.example:443"}]`} {
		if _, err := providerPolicy(raw); err == nil {
			t.Errorf("accepted malformed policy %q", raw[:min(len(raw), 200)])
		}
	}
	policy, err := providerPolicy(`[{"origin":"http://messages.example:1234","networks":["127.0.0.1/32"]},{"origin":"https://messages.example"}]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://messages.example:1234", "https://messages.example"} {
		if _, err := policy.NewClient(endpoint, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := policy.NewClient("http://messages.example:1235", time.Second); err == nil {
		t.Fatal("port boundary lost")
	}
	empty, err := providerPolicy("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.NewClient("https://messages.example", time.Second); err == nil {
		t.Fatal("empty operator policy admitted member URL")
	}
}

func TestBlueBubblesFormDefaultDoesNotAuthorizeDestination(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("BLUEBUBBLES_URL", "http://127.0.0.1:1234")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.BlueBubblesPolicy.NewClient(cfg.BlueBubblesURL, time.Second); err == nil {
		t.Fatal("form default became implicit network approval")
	}
}
