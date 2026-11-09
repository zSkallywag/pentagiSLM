package smallmodel

import (
	"regexp"
	"strconv"
)

var (
	reExitCode = regexp.MustCompile(`\[exit code: (-?\d+)\]\s*$`)
	// An HTTP failure in the forms a tool output carries it: a status line, a
	// JSON error body, or curl's -w "%{http_code}" printed on a line of its own.
	reHTTPFailure = regexp.MustCompile(
		`HTTP/\d(?:\.\d)?\s+[45]\d\d\b` +
			`|"(?:code|status|statusCode|status_code)"\s*:\s*"?[45]\d\d\b` +
			`|(?m:^\s*[45]\d\d\s*$)`,
	)
	reToolFailure = regexp.MustCompile(
		`handled with error:|command not found|No such file or directory|Permission denied`,
	)
)

// LooksFailed reports whether a tool output shows a failure by a deterministic
// marker: a non-zero exit code, an HTTP error status, or the tool's own error
// wrapper. It errs toward false: an output that merely mentions an error is not
// one, because the caller acts only on outputs it is sure about.
func LooksFailed(output string) bool {
	if m := reExitCode.FindStringSubmatch(output); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil && code != 0 {
			return true
		}
	}
	return reHTTPFailure.MatchString(output) || reToolFailure.MatchString(output)
}
