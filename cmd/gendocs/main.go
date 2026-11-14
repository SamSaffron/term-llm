// Command gendocs regenerates source-backed reference sections.
package main

import (
	"log"

	"github.com/term-llm/term-llm/cmd"
	"github.com/term-llm/term-llm/internal/docsgen"
)

func main() {
	if err := docsgen.Sync("docs-site/content/reference", docsgen.Pages(cmd.ReferenceCommand()), false); err != nil {
		log.Fatal(err)
	}
}
