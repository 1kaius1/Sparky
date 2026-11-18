// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeentra

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
)

// Key is an RSA signing key with the key ID it is published under.
type Key struct {
	KID     string
	Private *rsa.PrivateKey
}

// jwk is the public half in the JSON Web Key form a JWKS document lists.
//
// UNVERIFIED: Microsoft's OpenID Connect page names the document (jwks_uri) and
// says it returns "the public signing keys" but shows no sample of its shape.
// The members used here (kty, use, kid, n, e) are the standard JWK ones; a real
// tenant also lists x5t, x5c and a templated issuer, which this fake leaves
// out. The probe's key-set step records the real shape.
func (k *Key) jwk() map[string]string {
	return map[string]string{
		"kty": "RSA",
		"use": "sig",
		"kid": k.KID,
		"n":   base64.RawURLEncoding.EncodeToString(k.Private.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.Private.E)).Bytes()),
	}
}

// publicDER is the key's public half as PKIX DER, used as the HMAC secret in the
// algorithm-confusion token (see signJWT).
func (k *Key) publicDER() []byte {
	der, err := x509.MarshalPKIXPublicKey(&k.Private.PublicKey)
	if err != nil {
		panic(err) // an RSA public key always marshals
	}
	return der
}

// keyPool caches generated keys for the whole process. RSA key generation takes
// a noticeable fraction of a second, and a test suite creates many servers; the
// keys are throwaway test keys, so sharing them between servers costs nothing.
var keyPool struct {
	mu   sync.Mutex
	keys map[int]*Key
}

func poolKey(i int) *Key {
	keyPool.mu.Lock()
	defer keyPool.mu.Unlock()
	if keyPool.keys == nil {
		keyPool.keys = map[int]*Key{}
	}
	if k, ok := keyPool.keys[i]; ok {
		return k
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(priv.N.Bytes())
	k := &Key{KID: base64.RawURLEncoding.EncodeToString(sum[:20]), Private: priv}
	keyPool.keys[i] = k
	return k
}

// IDTokenSpec is an ID token before it is signed. A test changes it, through
// Server.OnIDToken, to produce the token Entra must never be believed to send.
type IDTokenSpec struct {
	// Header and Claims are the JWT header and payload. alg and kid in Header
	// are filled in from Alg and SignWith unless the test set them.
	Header map[string]any
	Claims map[string]any

	// Alg is how the token is signed: "RS256" (the default), "RS384", "HS256"
	// (HMAC keyed with the published public key - the classic algorithm
	// confusion attack) or "none" (no signature).
	Alg string

	// SignWith is the key to sign with; nil means the current published key.
	// A key from NewUnpublishedKey is not in the key set, so the signature
	// cannot be checked.
	SignWith *Key
}

// signJWT returns the compact serialization of spec.
func signJWT(spec *IDTokenSpec, current *Key) string {
	alg := spec.Alg
	if alg == "" {
		alg = "RS256"
	}
	key := spec.SignWith
	if key == nil {
		key = current
	}
	header := map[string]any{"typ": "JWT"}
	for k, v := range spec.Header {
		header[k] = v
	}
	if _, set := header["alg"]; !set {
		header["alg"] = alg
	}
	if _, set := header["kid"]; !set {
		header["kid"] = key.KID
	}

	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(header) + "." + enc(spec.Claims)

	var sig []byte
	switch alg {
	case "RS256":
		sum := sha256.Sum256([]byte(signingInput))
		sig = mustSign(key, crypto.SHA256, sum[:])
	case "RS384":
		sum := sha512.Sum384([]byte(signingInput))
		sig = mustSign(key, crypto.SHA384, sum[:])
	case "HS256":
		mac := hmac.New(sha256.New, key.publicDER())
		mac.Write([]byte(signingInput))
		sig = mac.Sum(nil)
	case "none":
	default:
		panic(fmt.Sprintf("fakeentra: unsupported alg %q", alg))
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func mustSign(key *Key, h crypto.Hash, digest []byte) []byte {
	sig, err := rsa.SignPKCS1v15(rand.Reader, key.Private, h, digest)
	if err != nil {
		panic(err)
	}
	return sig
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func randomGUID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
