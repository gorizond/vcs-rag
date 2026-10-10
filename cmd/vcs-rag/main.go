// Command vcs-rag is a self-hosted, VCS-agnostic RAG service over source code
// and repository metadata. The real worker/MCP logic lands in later phases;
// this stub only proves the build toolchain is wired up.
package main

import "fmt"

func main() {
	fmt.Printf("vcs-rag %s\n", version)
}
