// Command codeowners-tool makes safe, intent-level, verifiable changes to
// GitHub CODEOWNERS files, and audits them for rot.
package main

import (
	"os"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
