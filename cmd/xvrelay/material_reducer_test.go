package main

import (
	"bytes"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"testing"
)

func TestNativeMaterialReducerUnpacksHighItemID(t *testing.T) {
	for _, id := range []int32{347, -200} {
		for _, meta := range []uint32{3, 32768, 65535} {
			var raw bytes.Buffer
			_ = protocol.WriteVarint32(&raw, (id<<16)|int32(meta))
			raw.WriteByte(0) // no outputs; independent packed 16+16-bit fixture.
			var got protocol.MaterialReducer
			protocol.NewReader(&raw, 0, true).MaterialReducer(&got)
			if got.InputItem.NetworkID != id || got.InputItem.MetadataValue != meta {
				t.Errorf("id=%d meta=%d decoded=%+v", id, meta, got.InputItem)
			}
		}
	}
}
