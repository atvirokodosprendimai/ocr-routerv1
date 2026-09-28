package agent

import "testing"

// TestInputExtTakesOnlyASafeExtension is the whole security argument for this
// feature in one table.
//
// ⚠ THE FILENAME IS CLIENT-SUPPLIED. Any customer can upload a file called
// anything, and that string arrives at a worker running on a host the customer
// does not own. The basename is therefore NEVER used: the materialised file is
// always named after the job id, and only an extension is taken from what the
// client sent. That is why traversal, separators and shell metacharacters below
// are not "handled" — they cannot reach a path component at all.
//
// The job id is a different case and deliberately untreated: it comes from the
// router, which is the endpoint this worker was configured to trust, not from a
// customer.
func TestInputExtTakesOnlyASafeExtension(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		want     string
	}{
		{"an ordinary document", "report.docx", ".docx"},
		{"uppercase is kept — LibreOffice accepts either", "SCAN.PDF", ".PDF"},
		{"digits are ordinary", "archive.7z", ".7z"},
		{"only the last extension", "backup.tar.gz", ".gz"},
		{"a single character", "note.c", ".c"},

		{"no extension at all", "README", ""},
		{"nothing to take", "", ""},
		{"a trailing dot is not an extension", "weird.", ""},
		{"a dotfile is not an extension", ".bashrc", ""},

		// ⚠ Each of these is a path or a command if the BASENAME is used. None
		// of them can be, because only the extension is taken and it is then
		// appended to the job id.
		{"traversal", "../../../../etc/passwd.pdf", ".pdf"},
		{"an absolute path", "/etc/cron.d/evil.pdf", ".pdf"},
		{"a separator inside the extension", "x.pd/f", ""},
		{"a shell metacharacter", "x.sh;rm -rf /", ""},
		{"a space", "x.p df", ""},
		{"a NUL byte", "x.pd\x00f", ""},
		{"a newline", "x.pd\nf", ""},
		{"a dash, which could read as a flag", "x.-pdf", ""},
		{"non-ASCII", "x.pdfé", ""},

		// A bound, so a filename cannot make the path arbitrarily long. 16
		// characters is longer than any real extension and short enough that
		// nothing downstream has to think about it.
		{"a long but plausible extension", "x.abcdefghijklmnop", ".abcdefghijklmnop"},
		{"one character past the bound", "x.abcdefghijklmnopq", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputExt(tc.filename); got != tc.want {
				t.Errorf("inputExt(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}
