// Package compress provides zstd codec adapters for ConnectRPC's
// Compressor / Decompressor interfaces.
//
// zstd at SpeedFastest is substantially faster than gzip for the small
// protobuf payloads this service sends (quota responses, ACK sequences)
// while keeping a better compression ratio than snappy.
//
// Usage — server handler:
//
//	connect.WithCompression(compress.Name, compress.NewDecompressor, compress.NewCompressor)
//
// Usage — client:
//
//	connect.WithAcceptCompression(compress.Name, compress.NewDecompressor, compress.NewCompressor)
//	connect.WithSendCompression(compress.Name)
package compress

import (
	"github.com/klauspost/compress/zstd"
)

// Name is the content-encoding token ConnectRPC negotiates on the wire.
const Name = "zstd"

// NewCompressor returns a new zstd Compressor. *zstd.Encoder satisfies
// connect.Compressor directly (Write / Close / Reset all match).
// SpeedFastest minimises per-message latency; EncoderConcurrency(1)
// prevents the encoder from spawning its own goroutines since ConnectRPC
// pools and reuses instances single-threaded.
func NewCompressor() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		// zstd.NewWriter only errors on invalid options; panic is appropriate.
		panic("compress: zstd.NewWriter: " + err.Error())
	}
	return enc
}

// NewDecompressor returns a new zstd Decompressor.
// *zstd.Decoder almost satisfies connect.Decompressor but its Close()
// returns nothing; zstdDecoder adds the error return.
func NewDecompressor() *zstdDecoder {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		panic("compress: zstd.NewReader: " + err.Error())
	}
	return &zstdDecoder{dec}
}

// zstdDecoder wraps *zstd.Decoder to satisfy connect.Decompressor.
// The only gap is Close(): zstd.Decoder.Close() is void; we return nil.
type zstdDecoder struct {
	*zstd.Decoder
}

func (z *zstdDecoder) Close() error {
	// Do NOT close the underlying decoder. ConnectRPC pools decompressors and
	// calls Reset() after Close() for the next message; *zstd.Decoder.Reset()
	// errors if the decoder has been closed. The decoder is GC'd with the pool.
	return nil
}
