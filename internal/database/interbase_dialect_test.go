package database

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveInterBaseDialect(t *testing.T) {
	diagFailure := errors.New("info read failed")

	tests := []struct {
		name         string
		alias        string
		requested    int
		reported     int64
		diagErr      error
		wantResolved int
		wantReattach bool
		wantWarnings int
		wantMentions []string
		wantAbsent   []string
	}{
		{
			name:         "auto-detect on a dialect 3 database attaches once",
			alias:        "centrale",
			requested:    0,
			reported:     3,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 0,
		},
		{
			name:         "auto-detect on a dialect 1 database costs one extra attach",
			alias:        "legacy",
			requested:    0,
			reported:     1,
			wantResolved: 1,
			wantReattach: true,
			wantWarnings: 0,
		},
		{
			name:         "auto-detect on an unexpected reported dialect falls back to 3 and warns",
			alias:        "odd",
			requested:    0,
			reported:     2,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"odd", "2", "3"},
		},
		{
			name:         "a pinned dialect that agrees is silent",
			alias:        "centrale",
			requested:    3,
			reported:     3,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 0,
		},
		{
			name:         "a pinned dialect 1 that disagrees connects and warns",
			alias:        "centrale",
			requested:    1,
			reported:     3,
			wantResolved: 1,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"centrale", "dialect 1", "dialect 3", "parsing", "query attachment", "catalog DDL reconstruction", "source database dialect 3", "dialect: 0"},
			wantAbsent:   []string{"lex and render types as dialect 1"},
		},
		{
			name:         "a pinned dialect 3 that disagrees connects and warns",
			alias:        "legacy",
			requested:    3,
			reported:     1,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"legacy", "dialect 3", "dialect 1", "parsing", "query attachment", "catalog DDL reconstruction", "source database dialect 1"},
			wantAbsent:   []string{"lex and render types as dialect 3"},
		},
		{
			name:         "a diagnostics failure with auto-detect falls back to 3 and warns",
			alias:        "offline",
			requested:    0,
			diagErr:      diagFailure,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"offline", "diagnostics", "3"},
		},
		{
			name:         "a diagnostics failure with a pinned dialect keeps the pin and warns",
			alias:        "offline",
			requested:    1,
			diagErr:      diagFailure,
			wantResolved: 1,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"offline", "diagnostics", "1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveInterBaseDialect(tt.alias, tt.requested, tt.reported, tt.diagErr)

			if got.Resolved != tt.wantResolved {
				t.Errorf("Resolved = %d, want %d", got.Resolved, tt.wantResolved)
			}
			if got.Reattach != tt.wantReattach {
				t.Errorf("Reattach = %v, want %v", got.Reattach, tt.wantReattach)
			}
			if len(got.Warnings) != tt.wantWarnings {
				t.Fatalf("got %d warnings, want %d: %v", len(got.Warnings), tt.wantWarnings, got.Warnings)
			}
			for _, mention := range tt.wantMentions {
				if !strings.Contains(got.Warnings[0], mention) {
					t.Errorf("warning %q does not mention %q", got.Warnings[0], mention)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got.Warnings[0], absent) {
					t.Errorf("warning %q must not claim %q", got.Warnings[0], absent)
				}
			}
			for _, warning := range got.Warnings {
				if strings.Contains(warning, "\n") {
					t.Errorf("warning must be a single line: %q", warning)
				}
				if !strings.HasPrefix(warning, "interbase: ") {
					t.Errorf("warning must be prefixed for the log: %q", warning)
				}
			}
		})
	}
}

func TestResolveInterBaseDialectReattachesOnlyForAutoDetectedDialect1(t *testing.T) {
	// The locked cost model: exactly one extra attach, and only here.
	for _, requested := range []int{0, 1, 3} {
		for _, reported := range []int64{1, 3} {
			got := resolveInterBaseDialect("a", requested, reported, nil)
			want := requested == 0 && reported == 1
			if got.Reattach != want {
				t.Errorf("requested=%d reported=%d: Reattach = %v, want %v", requested, reported, got.Reattach, want)
			}
		}
	}
}
