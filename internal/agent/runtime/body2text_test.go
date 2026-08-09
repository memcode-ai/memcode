package runtime

import (
	"strings"
	"testing"
)

// Regression: a regex stripper ate a 235KB SSR'd article down to its <title>.
// body2text must extract the BODY prose and drop script/style/head noise.
func TestBody2TextExtractsBodyNotJustTitle(t *testing.T) {
	h := `<!DOCTYPE html><html><head><title>T</title>` +
		`<script>var x=1</script><style>.a{}</style></head>` +
		`<body><div id="__next"><h1>Heading</h1>` +
		`<p>The real article about research taste.</p></div>` +
		`<script>self.__next_f.push([1,"hidden rsc payload"])</script></body></html>`
	got := body2text("text/html", h)
	if !strings.Contains(got, "The real article about research taste.") {
		t.Errorf("body prose lost: %q", got)
	}
	if !strings.Contains(got, "Heading") {
		t.Errorf("heading lost: %q", got)
	}
	for _, leak := range []string{"self.__next_f", "var x=1", ".a{}", "hidden rsc payload"} {
		if strings.Contains(got, leak) {
			t.Errorf("script/style content leaked (%q): %q", leak, got)
		}
	}
}

func TestBody2TextPassesThroughNonHTML(t *testing.T) {
	j := `{"key":"value"}`
	if got := body2text("application/json", j); got != j {
		t.Errorf("JSON should pass through: %q", got)
	}
}
