package control

import (
	"bytes"
	"testing"
)

func BenchmarkWriteMessage(b *testing.B) {
	msg := &Hello{
		Type:     TypeHello,
		Protocol: ProtocolVersion,
		Token:    "0123456789abcdef",
		ClientID: "client-1",
		Mappings: nil,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadMessage(b *testing.B) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &Ping{Type: TypePing, TS: 123}); err != nil {
		b.Fatal(err)
	}
	payload := buf.Bytes()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		if _, err := ReadMessage(bytes.NewReader(payload)); err != nil {
			b.Fatal(err)
		}
	}
}
