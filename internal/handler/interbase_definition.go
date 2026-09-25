package handler

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sqls-server/sqls/internal/database"
)

// snapshotTarget contains only host presentation metadata. Object resolution
// and source selection belong to language.Service; this type is used when the
// host renders a returned Source into its read-only snapshot file.
type snapshotTarget struct {
	kind       database.ObjectKind
	name       string
	provenance string
}

// singleLine collapses every common SQL/editor line ending before interpolating
// untrusted catalog/config text into one banner comment line.
func singleLine(text string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
}

// commentLines normalizes CRLF, lone CR and LF as line boundaries and prefixes
// every resulting line, so untrusted notes cannot escape SQL line comments.
func commentLines(text string) string {
	var b strings.Builder
	normalized := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	for _, line := range strings.Split(normalized, "\n") {
		b.WriteString("-- ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderSnapshot builds the read-only host presentation and reports the number
// of banner lines to offset a language-provided body-relative source range.
func renderSnapshot(target snapshotTarget, sc snapshotContext, now time.Time, body, note string) (string, int) {
	var b strings.Builder
	fmt.Fprintf(&b, "-- sqls: read-only snapshot of InterBase %s %q\n",
		singleLine(strings.ToUpper(string(target.kind))), singleLine(target.name))
	fmt.Fprintf(&b, "-- connection: %s    generated: %s\n",
		singleLine(sc.label), now.UTC().Format(time.RFC3339))
	b.WriteString("-- Editing this file does not change the database.\n")
	bannerLines := 3
	if target.provenance != "" {
		fmt.Fprintf(&b, "-- provenance: %s\n", singleLine(target.provenance))
		bannerLines++
	}
	if note != "" {
		commented := commentLines(note)
		b.WriteString(commented)
		bannerLines += strings.Count(commented, "\n")
	}
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	return b.String(), bannerLines
}

// definitionDDLTimeout bounds the detail round trip on the inline dispatch path.
const definitionDDLTimeout = 3 * time.Second

// snapshotURI turns an absolute path into a file:// URI. Escaped catalog names
// are encoded by URL.String so the URI decodes back to the real file name.
func snapshotURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}
