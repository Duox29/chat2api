// Package pow implements DeepSeekHashV1, the proof-of-work used by
// chat.deepseek.com's /api/v0/chat/* endpoints.
//
// Reverse-engineered from DeepSeek's own pure-JS fallback worker
// (frontend chunk 76608) and validated against a real accepted challenge:
//
//	prefix = salt + "_" + expireAt + "_"
//	answer = smallest i in [0, difficulty) with H(prefix + itoa(i)) == challenge
//
// where H is Keccak-f[1600] with rate=1088 bits (136 bytes), 256-bit output,
// SHA3-style domain padding (0x06 ... 0x80) — but only 23 rounds, applying
// round constants RC[1]..RC[23] (the first round with RC[0] is skipped).
package pow

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Challenge is a PoW challenge as returned by create_pow_challenge
// (field names normalized from the API's snake_case).
type Challenge struct {
	Algorithm  string
	Challenge  string // target hex digest (64 chars)
	Salt       string
	Difficulty int
	Signature  string
	ExpireAt   int64
}

// Keccak-f[1600] round constants.
var rc = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// Rho rotation offsets r[x][y].
var rho = [5][5]uint{
	{0, 36, 3, 41, 18},
	{1, 44, 10, 45, 2},
	{62, 6, 43, 15, 61},
	{28, 55, 25, 21, 56},
	{27, 20, 39, 8, 14},
}

func rotl(x uint64, n uint) uint64 { n &= 63; return x<<n | x>>(64-n) }

// keccakF applies rounds [from, to) of Keccak-f to the 25-lane state
// (lane index = x + 5*y).
func keccakF(s *[25]uint64, from, to int) {
	var c, d [5]uint64
	var b [25]uint64
	for rnd := from; rnd < to; rnd++ {
		// theta
		for x := 0; x < 5; x++ {
			c[x] = s[x] ^ s[x+5] ^ s[x+10] ^ s[x+15] ^ s[x+20]
		}
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ rotl(c[(x+1)%5], 1)
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				s[x+5*y] ^= d[x]
			}
		}
		// rho + pi
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				b[y+5*((2*x+3*y)%5)] = rotl(s[x+5*y], rho[x][y])
			}
		}
		// chi
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				s[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		// iota
		s[0] ^= rc[rnd]
	}
}

const rate = 136 // bytes (1088 bits)

// Hash computes DeepSeekHashV1(msg).
func Hash(msg []byte) [32]byte {
	// pad10*1 with SHA3 domain suffix 0x06
	padded := make([]byte, 0, len(msg)+rate)
	padded = append(padded, msg...)
	padded = append(padded, 0x06)
	for (len(padded)+1)%rate != 0 {
		padded = append(padded, 0x00)
	}
	padded = append(padded, 0x80)

	var s [25]uint64
	for off := 0; off < len(padded); off += rate {
		block := padded[off : off+rate]
		for i := 0; i < rate/8; i++ {
			var v uint64
			for j := 0; j < 8; j++ {
				v |= uint64(block[8*i+j]) << (8 * uint(j))
			}
			s[i] ^= v
		}
		keccakF(&s, 1, 24) // 23 rounds, RC[1]..RC[23]
	}
	var out [32]byte
	for i := 0; i < 4; i++ {
		for j := 0; j < 8; j++ {
			out[8*i+j] = byte(s[i] >> (8 * uint(j)))
		}
	}
	return out
}

// Solve finds the PoW answer for ch.
func Solve(ch Challenge) (int, error) {
	if ch.Algorithm != "DeepSeekHashV1" {
		return 0, fmt.Errorf("unsupported PoW algorithm: %s", ch.Algorithm)
	}
	if len(ch.Challenge) != 64 {
		return 0, errors.New("bad PoW challenge length")
	}
	if ch.Difficulty <= 0 {
		return 0, errors.New("bad PoW difficulty")
	}
	prefix := ch.Salt + "_" + strconv.FormatInt(ch.ExpireAt, 10) + "_"
	target := ch.Challenge
	for i := 0; i < ch.Difficulty; i++ {
		h := Hash([]byte(prefix + strconv.Itoa(i)))
		match := true
		for k := 0; k < 32; k++ {
			if "0123456789abcdef"[h[k]>>4] != target[2*k] || "0123456789abcdef"[h[k]&15] != target[2*k+1] {
				match = false
				break
			}
		}
		if match {
			return i, nil
		}
	}
	return 0, fmt.Errorf("PoW solve failed: no solution in [0, %d)", ch.Difficulty)
}

// HeaderValue builds the x-ds-pow-response header for a solved challenge.
func HeaderValue(ch Challenge, answer int, targetPath string) string {
	payload, _ := json.Marshal(map[string]interface{}{
		"algorithm":   ch.Algorithm,
		"challenge":   ch.Challenge,
		"salt":        ch.Salt,
		"answer":      answer,
		"signature":   ch.Signature,
		"target_path": targetPath,
	})
	return base64.StdEncoding.EncodeToString(payload)
}
