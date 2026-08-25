// webtools serves the research workflow's two tenant tools over HTTPS for
// standalone (non-test) deployments. Tests embed the package directly.
//
// Backend selection (roadmap P1):
//   - default: the embedded deterministic corpus (CI / offline)
//   - AGENTOS_RESEARCH_LIVE_WEB=1: real internet backends — search provider
//     via AGENTOS_RESEARCH_SEARCH_PROVIDER ("doubao"|"brave"|"bing") with its key in
//     AGENTOS_RESEARCH_SEARCH_KEY, and the hardened live fetcher.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/CloudEdgeCore/AgentOS/examples/research-workflow/tools/webtools"
)

func main() {
	server := webtools.New(webtools.Corpus())
	if os.Getenv("AGENTOS_RESEARCH_LIVE_WEB") == "1" {
		provider := os.Getenv("AGENTOS_RESEARCH_SEARCH_PROVIDER")
		key := os.Getenv("AGENTOS_RESEARCH_SEARCH_KEY")
		var search webtools.SearchProvider
		switch provider {
		case "doubao", "volcengine":
			search = &webtools.DoubaoSearch{APIKey: key}
		case "brave":
			search = &webtools.BraveSearch{APIKey: key}
		case "bing":
			search = &webtools.BingSearch{APIKey: key}
		default:
			log.Fatalf("unknown AGENTOS_RESEARCH_SEARCH_PROVIDER %q (want doubao, brave or bing)", provider)
		}
		server = server.WithBackend(&webtools.CompositeBackend{
			SearchProvider: search,
			FetchProvider:  &webtools.LiveFetch{},
		})
		log.Printf("live web enabled: search=%s fetch=hardened-http", provider)
	}
	listener, _, endpoint, err := webtools.SelfSignedTLSListener(server)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("webtools serving on %s (self-signed certificate)", endpoint)
	if err := (&http.Server{Handler: server}).Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
