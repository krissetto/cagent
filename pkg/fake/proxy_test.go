package fake

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
)

func TestDefaultMatcherNormalizesPromptFilePaths(t *testing.T) {
	matcher := DefaultMatcher(func(err error) { t.Fatal(err) })
	cassetteBody := `{"system":[{"text":"Instructions from: FILE\nbody"}]}`

	for _, body := range []string{
		`{"system":[{"text":"Instructions from: /tmp/repo/AGENTS.md\nbody"}]}`,
		`{"system":[{"text":"Instructions from: C:\\Users\\runner\\repo\\AGENTS.md\nbody"}]}`,
	} {
		req, err := http.NewRequest(http.MethodPost, "https://example.test/v1", io.NopCloser(strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		if !matcher(req, cassette.Request{Method: http.MethodPost, URL: "https://example.test/v1", Body: cassetteBody}) {
			t.Fatalf("prompt path was not normalized: %s", body)
		}
	}
}
