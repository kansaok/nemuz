package journal

import (
	"crypto/rand"
	"fmt"
	"time"
)

// crockford is Crockford base32: no I, L, O, or U, so ids survive being read
// aloud and retyped without ambiguity.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewTurnID returns a 26-character ULID: 48 bits of millisecond timestamp
// followed by 80 bits of randomness.
//
// Ids sort lexicographically in the order they were created, which is why
// listing a journal directory by filename yields turns in chronological order
// with no index to consult.
func NewTurnID() (string, error) {
	return newTurnIDAt(time.Now())
}

func newTurnIDAt(t time.Time) (string, error) {
	var raw [16]byte
	ms := uint64(t.UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("journal: read randomness: %w", err)
	}
	return encodeULID(raw), nil
}

// encodeULID renders 128 bits as 26 base32 characters, most significant first.
func encodeULID(raw [16]byte) string {
	out := make([]byte, 26)
	// Read the 128 bits as a big-endian bit stream, five bits at a time. The
	// first character carries only two significant bits, matching the ULID spec.
	var bitPos uint // bits consumed so far, from the top
	for i := 0; i < 26; i++ {
		var v byte
		for b := 0; b < 5; b++ {
			// The stream is 130 bits wide; the two leading bits are zero.
			pos := int(bitPos) + b - 2
			var bit byte
			if pos >= 0 {
				bit = (raw[pos/8] >> (7 - uint(pos%8))) & 1
			}
			v = v<<1 | bit
		}
		out[i] = crockford[v]
		bitPos += 5
	}
	return string(out)
}
