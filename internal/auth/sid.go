// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"encoding/binary"
	"fmt"
)

// decodeObjectSID converts an AD objectSid attribute's raw binary value
// into its string form (e.g. "S-1-5-21-3623811015-3361044348-30300820-1013").
// AD returns this attribute as raw bytes over LDAP, not as printable text -
// see https://learn.microsoft.com/windows/win32/adschema/a-objectsid for the
// binary layout this decodes:
//
//	byte 0:    revision
//	byte 1:    sub-authority count (N)
//	bytes 2-7: 48-bit big-endian identifier authority
//	bytes 8+:  N x 4-byte little-endian sub-authorities
func decodeObjectSID(raw []byte) (string, error) {
	const headerLen = 8

	if len(raw) < headerLen {
		return "", fmt.Errorf("objectSid too short: got %d bytes, want at least %d", len(raw), headerLen)
	}

	revision := raw[0]
	subAuthorityCount := int(raw[1])

	var authority uint64
	for _, b := range raw[2:headerLen] {
		authority = authority<<8 | uint64(b)
	}

	wantLen := headerLen + subAuthorityCount*4
	if len(raw) != wantLen {
		return "", fmt.Errorf("objectSid length mismatch: got %d bytes, want %d for %d sub-authorities", len(raw), wantLen, subAuthorityCount)
	}

	sid := fmt.Sprintf("S-%d-%d", revision, authority)
	for i := 0; i < subAuthorityCount; i++ {
		offset := headerLen + i*4
		subAuthority := binary.LittleEndian.Uint32(raw[offset : offset+4])
		sid = fmt.Sprintf("%s-%d", sid, subAuthority)
	}

	return sid, nil
}
