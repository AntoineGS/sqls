package database

// interBaseSQLRejection distinguishes SQL-level prepare errors from driver
// system, network, lock, and connection failures.
func interBaseSQLRejection(code int) bool {
	if code == 0 {
		return false
	}
	switch code {
	case -901, -902, -913, -923, -924:
		return false
	default:
		return true
	}
}
