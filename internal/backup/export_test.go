package backup

// SetPartBytesForTest lowers the database part size so a small database splits. Test-only.
func SetPartBytesForTest(n int64) (restore func()) {
	old := partBytes
	partBytes = n
	return func() { partBytes = old }
}
