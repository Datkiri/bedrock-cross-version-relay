package protocol

const (
	HeightMapDataNone = iota
	HeightMapDataHasData
	HeightMapDataTooHigh
	HeightMapDataTooLow
	HeightMapDataAllCopied
)

const (
	SubChunkResultUndefined = iota
	SubChunkResultSuccess
	SubChunkResultChunkNotFound
	SubChunkResultInvalidDimension
	SubChunkResultPlayerNotFound
	SubChunkResultIndexOutOfBounds
	SubChunkResultSuccessAllAir
)

// SubChunkEntry contains the data of a sub-chunk entry relative to a center sub chunk position, used for the sub-chunk
// requesting system introduced in v1.18.10.
type SubChunkEntry struct {
	// Offset contains the offset between the sub-chunk position and the center position.
	Offset SubChunkOffset
	// Result is always one of the constants defined in the SubChunkResult constants.
	Result byte
	// RawPayload contains the serialized sub-chunk data, if present.
	RawPayload Optional[[]byte]
	// HeightMapType is always one of the constants defined in the HeightMapData constants.
	HeightMapType byte
	// HeightMapData is the data for the height map, if present.
	HeightMapData Optional[HeightMap]
	// RenderHeightMapType is always one of the constants defined in the HeightMapData constants.
	RenderHeightMapType byte
	// RenderHeightMapData is the data for the render height map, if present.
	RenderHeightMapData Optional[HeightMap]
	// BlobHash is the hash of the blob, if present.
	BlobHash Optional[uint64]
}

// Marshal encodes/decodes a SubChunkEntry.
func (x *SubChunkEntry) Marshal(r IO) {
	Single(r, &x.Offset)
	// ★ VERIFIED AGAINST REALITY — measured with the client's own codec
	// (bedrock-codec-3.0.0.Beta13, `org.cloudburstmc.protocol.bedrock.data.SubChunkRequestResult`):
	//   0=UNDEFINED  1=SUCCESS  2=CHUNK_NOT_FOUND  3=INVALID_DIMENSION
	//   4=PLAYER_NOT_FOUND  5=INDEX_OUT_OF_BOUNDS  6=SUCCESS_ALL_AIR
	// That list matches the SubChunkResult* constants above 1:1 ⇒ the wire value IS the internal
	// value, do NOT translate anything.
	//
	// The old code did `result = x.Result - 1`, based on the claim that "2193 wire is 0..5 and
	// Undefined was dropped in 2168". That claim is WRONG: the client enum has all 7 members with
	// UNDEFINED at 0. Consequence: SUCCESS(1) was written as wire 0 ⇒ the client read UNDEFINED ⇒
	// every sub-chunk was skipped ⇒ empty world / crash. Proven by letting the client decode the
	// bytes the relay sent: `SubChunkData(..., result=UNDEFINED, ...)`.
	if _, reading := r.(*Reader); reading {
		r.Uint8(&x.Result)
		if x.Result > SubChunkResultSuccessAllAir {
			r.UnknownEnumOption(x.Result, "subchunk result (2193)")
			return
		}
	} else {
		if x.Result > SubChunkResultSuccessAllAir {
			r.UnknownEnumOption(x.Result, "subchunk result")
			return
		}
		r.Uint8(&x.Result)
	}
	OptionalFunc(r, &x.RawPayload, r.ByteSlice)
	r.Uint8(&x.HeightMapType)

	OptionalMarshaler(r, &x.HeightMapData)
	r.Uint8(&x.RenderHeightMapType)

	OptionalMarshaler(r, &x.RenderHeightMapData)
	OptionalFunc(r, &x.BlobHash, r.Uint64)
}

// SubChunkOffset represents an offset from the base position of another sub chunk.
type SubChunkOffset [3]int8

// Marshal encodes/decodes a SubChunkOffset.
func (x *SubChunkOffset) Marshal(r IO) {
	r.Int8(&x[0])
	r.Int8(&x[1])
	r.Int8(&x[2])
}

// HeightMap holds the height of every column of a sub-chunk, indexed as [z][x].
type HeightMap [16][16]int8

// Marshal encodes/decodes a HeightMap.
func (x *HeightMap) Marshal(r IO) {
	for z := range x {
		n := uint32(16)
		r.Varuint32(&n)
		if n != 16 {
			r.InvalidValue(n, "height map row", "must hold 16 heights")
			return
		}
		for i := range x[z] {
			r.Int8(&x[z][i])
		}
	}
}
