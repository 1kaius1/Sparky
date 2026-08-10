// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"encoding/binary"
	"testing"
)

// encodeObjectSIDForTest builds the raw bytes for a SID from its parts,
// mirroring the documented binary layout decodeObjectSID parses - see
// https://learn.microsoft.com/windows/win32/adschema/a-objectsid. Used to
// verify decode is the correct inverse, rather than relying on hardcoded
// byte literals that could be wrong from the start.
func encodeObjectSIDForTest(revision byte, authority uint64, subAuthorities []uint32) []byte {
	raw := make([]byte, 8+len(subAuthorities)*4)
	raw[0] = revision
	raw[1] = byte(len(subAuthorities))

	for i := 0; i < 6; i++ {
		raw[7-i] = byte(authority >> (8 * i))
	}

	for i, sub := range subAuthorities {
		binary.LittleEndian.PutUint32(raw[8+i*4:], sub)
	}

	return raw
}

func TestDecodeObjectSID_RoundTrip(t *testing.T) {
	tests := []struct {
		name           string
		revision       byte
		authority      uint64
		subAuthorities []uint32
		want           string
	}{
		{
			name:           "typical domain user SID",
			revision:       1,
			authority:      5,
			subAuthorities: []uint32{21, 3623811015, 3361044348, 30300820, 1013},
			want:           "S-1-5-21-3623811015-3361044348-30300820-1013",
		},
		{
			name:           "no sub-authorities",
			revision:       1,
			authority:      5,
			subAuthorities: nil,
			want:           "S-1-5",
		},
		{
			name:           "single sub-authority",
			revision:       1,
			authority:      5,
			subAuthorities: []uint32{32},
			want:           "S-1-5-32",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := encodeObjectSIDForTest(tt.revision, tt.authority, tt.subAuthorities)

			got, err := decodeObjectSID(raw)
			if err != nil {
				t.Fatalf("decodeObjectSID() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("decodeObjectSID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecodeObjectSID_TooShort(t *testing.T) {
	_, err := decodeObjectSID([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("decodeObjectSID() succeeded on a too-short input, want an error")
	}
}

func TestDecodeObjectSID_LengthMismatch(t *testing.T) {
	// Header claims 2 sub-authorities (8 bytes) but only provides 4 more
	// bytes of payload (1 sub-authority's worth).
	raw := encodeObjectSIDForTest(1, 5, []uint32{21})
	raw[1] = 2

	_, err := decodeObjectSID(raw)
	if err == nil {
		t.Fatal("decodeObjectSID() succeeded on a length mismatch, want an error")
	}
}

func TestDecodeObjectSID_Empty(t *testing.T) {
	_, err := decodeObjectSID(nil)
	if err == nil {
		t.Fatal("decodeObjectSID() succeeded on empty input, want an error")
	}
}
