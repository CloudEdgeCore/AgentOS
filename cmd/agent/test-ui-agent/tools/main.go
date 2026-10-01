package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Example custom tool service
func main() {
	http.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"output": map[string]any{
				"status": "OK",
				"metric": "sample_telemetry",
				"value":  42.0,
			},
			"receiptId": fmt.Sprintf("rcpt_%x", time.Now().UnixNano()),
		}
		json.NewEncoder(w).Encode(resp)
	})
	fmt.Printf("[test-ui-agent Tool Server] listening on :18080...\n", "test-ui-agent")
	_ = http.ListenAndServe(":18080", nil)
}
