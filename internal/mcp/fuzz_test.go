package mcp

import (
	"encoding/json"
	"testing"
)

func FuzzParseRequest(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/list"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		request, rpcErr := ParseRequest(raw)
		if rpcErr != nil {
			if request != nil {
				t.Fatal("rejected request must not return a parsed value")
			}
			return
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if replay, replayErr := ParseRequest(encoded); replayErr != nil || replay.Method != request.Method {
			t.Fatalf("accepted request did not round trip: %v", replayErr)
		}
	})
}
