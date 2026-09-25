package language

import "testing"

// TestCodePointColumnToUTF16 exercises the token.Tokenizer.Col -> UTF16Column
// conversion directly: the tokenizer counts one column per Unicode code
// point, not per UTF-16 code unit, so a line containing a rune above
// U+FFFF must produce a strictly smaller code-point column than UTF-16
// column for any position after that rune.
func TestCodePointColumnToUTF16(t *testing.T) {
	text := "😀x"
	// Code point column 0 is the emoji itself; column 1 is 'x'.
	if got, err := codePointColumnToUTF16(text, 0, 0); err != nil || got != 0 {
		t.Fatalf("column 0: got (%d, %v)", got, err)
	}
	if got, err := codePointColumnToUTF16(text, 0, 1); err != nil || got != 2 {
		t.Fatalf("column 1: got (%d, %v), want (2, nil)", got, err)
	}
	if got, err := codePointColumnToUTF16(text, 0, 2); err != nil || got != 3 {
		t.Fatalf("column 2 (end of line): got (%d, %v), want (3, nil)", got, err)
	}
	if _, err := codePointColumnToUTF16(text, 0, 3); err == nil {
		t.Fatal("accepted code-point column past end of line")
	}
}

func TestCodePointColumnToUTF16MultiLineAndCRLF(t *testing.T) {
	text := "-- 😀\r\nselect Café from T"
	// Line 1 ("select Café from T") is pure BMP text, so code-point and
	// UTF-16 columns agree: "select " is 7 code points.
	if got, err := codePointColumnToUTF16(text, 1, 7); err != nil || got != 7 {
		t.Fatalf("line 1 column 7: got (%d, %v), want (7, nil)", got, err)
	}
}

func TestCodePointColumnToUTF16RejectsMalformedInput(t *testing.T) {
	text := "abc\ndef"
	cases := []struct {
		name            string
		line, codePoint int
	}{
		{"negative line", -1, 0},
		{"negative column", 0, -1},
		{"line out of range", 5, 0},
		{"column past end of line", 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := codePointColumnToUTF16(text, tc.line, tc.codePoint); err == nil {
				t.Fatalf("accepted line=%d column=%d", tc.line, tc.codePoint)
			}
		})
	}
}
