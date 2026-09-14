package httpapi

// StatusForTest exposes the error→status mapper to the external test package.
//
// The mapper is deliberately unexported — nothing outside this package should be
// making that decision — but its table-driven test is the one thing standing
// between a newly added sentinel and a silent 500, so it needs a way in.
func StatusForTest(err error) int { return statusFor(err) }
