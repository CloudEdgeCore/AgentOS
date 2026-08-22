package provider

import (
	"bytes"
	"testing"
)

func FuzzConsumeOpenAIStream(f *testing.F) {
	f.Add([]byte("data: [DONE]\n\n"))
	f.Add([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	f.Add([]byte("data: {broken\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		_, _, _ = consumeStream(bytes.NewReader(raw), StreamObserver{})
	})
}
