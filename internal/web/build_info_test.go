package web

import (
	"strings"
	"testing"
)

func TestBuildLinksRespectReleaseHistory(t *testing.T) {
	for _, test := range []struct{ version, repository, prefix string }{
		{"0.5.3", "buntzen-pass-bot", "buntzen-pass-bot-v"},
		{"0.4.0", "buntzen-pass-bot", "buntzen-pass-bot-v"},
		{"0.5.4", "lake-pass-bot", "lake-pass-bot-v"},
		{"1.0.0", "lake-pass-bot", "lake-pass-bot-v"},
		{"0.0.0-ci", "lake-pass-bot", "lake-pass-bot-v"},
	} {
		build := applicationBuild(test.version, "")
		if !strings.Contains(build.ReleaseURL, "/"+test.repository+"/releases/tag/"+test.prefix+test.version) {
			t.Fatalf("version %s links to %s", test.version, build.ReleaseURL)
		}
	}
}
