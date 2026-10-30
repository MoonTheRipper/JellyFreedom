package main

import (
	"fmt"
	"os"

	"jellyfreedom/internal/update"
)

// runUpdateVerdict prints whether installing CANDIDATE over CURRENT is an upgrade, the
// same version, a downgrade, or unknowable. `jellyfreedom --update` calls it so the shell
// CLI orders versions with the same code as the dashboard instead of a copy in bash.
//
// The answer is on stdout and the exit status is always 0 for a well-formed call, so the
// caller cannot mistake a verdict for a failure — or a failure for a verdict.
func runUpdateVerdict(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: orchestrator update-verdict CURRENT CANDIDATE")
		return 2
	}
	fmt.Println(update.InstallVerdict(args[0], args[1]))
	return 0
}
