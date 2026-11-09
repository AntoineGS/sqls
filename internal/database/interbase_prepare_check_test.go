package database

import "testing"

func TestInterBaseSQLRejectionExcludesSystemAndConnectionCodes(t *testing.T) {
	for _, code := range []int{-901, -902, -913, -923, -924} {
		if interBaseSQLRejection(code) {
			t.Errorf("interBaseSQLRejection(%d) = true, want false for a non-SQL connection/system failure", code)
		}
	}

	for _, code := range []int{-104, -204, -206} {
		if !interBaseSQLRejection(code) {
			t.Errorf("interBaseSQLRejection(%d) = false, want true for a server SQL rejection", code)
		}
	}
	if interBaseSQLRejection(0) {
		t.Error("interBaseSQLRejection(0) = true, want false")
	}
}
