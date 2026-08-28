package sentinel

import (
	"strings"
	"testing"
)

func TestFileDownloadPathMatchesChat202608(t *testing.T) {
	p := fileDownloadPath("file_abc", "conv-1")
	for _, want := range []string{
		"/backend-api/files/download/file_abc",
		"conversation_id=conv-1",
		"inline=false",
		"download_intent=false",
		"include_library_file_state=true",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("path %q missing %q", p, want)
		}
	}
}
