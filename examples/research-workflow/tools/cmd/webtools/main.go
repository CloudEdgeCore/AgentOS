// webtools serves the research workflow's two tenant tools over HTTPS for
// standalone (non-test) deployments. Tests embed the package directly.
package main

import (
	"log"
	"net/http"

	"github.com/CloudEdgeCore/AgentOS/examples/research-workflow/tools/webtools"
)

func main() {
	server := webtools.New(webtools.Corpus())
	listener, _, endpoint, err := webtools.SelfSignedTLSListener(server)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("webtools serving on %s (self-signed certificate)", endpoint)
	if err := (&http.Server{Handler: server}).Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
