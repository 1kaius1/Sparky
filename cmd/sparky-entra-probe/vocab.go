// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "regexp"

// Vocabulary is a list of standard names that may be shown in the report as
// they are. A name a response contains that is not on the list is shown as
// "other-N": a custom claim or an extension attribute can carry an application
// ID or a tenant name in its name, and the report must not.
type Vocabulary struct {
	names map[string]bool
	// family matches a whole family of standard names, such as the xms_* claims.
	family *regexp.Regexp
}

// Known reports whether name may be shown.
func (v Vocabulary) Known(name string) bool {
	return v.names[name] || (v.family != nil && v.family.MatchString(name))
}

func vocabulary(family string, names ...string) Vocabulary {
	v := Vocabulary{names: map[string]bool{}}
	for _, n := range names {
		v.names[n] = true
	}
	if family != "" {
		v.family = regexp.MustCompile(family)
	}
	return v
}

// claimVocab covers the claim names Microsoft documents for ID tokens and
// access tokens (id-token-claims-reference, access-token-claims-reference).
var claimVocab = vocabulary(`^xms_[a-z0-9_]{1,40}$`,
	"iss", "sub", "aud", "exp", "nbf", "iat", "nonce", "jti", "oid", "tid", "name", "preferred_username",
	"email", "upn", "unique_name", "ver", "uti", "rh", "aio", "sid", "roles", "wids", "groups", "hasgroups",
	"_claim_names", "_claim_sources", "scp", "azp", "azpacr", "appid", "appidacr", "idp", "idtyp", "amr", "acr",
	"auth_time", "c_hash", "at_hash", "given_name", "family_name", "ipaddr", "tenant_ctry", "tenant_region_scope",
	"acct", "login_hint", "controls", "cnf", "ctry", "fwd", "in_corp", "onprem_sid", "pwd_exp", "pwd_url",
	"sub_jwk", "verified_primary_email", "verified_secondary_email", "vnet", "signin_state", "deviceid",
)

// headerVocab covers JWT header members.
var headerVocab = vocabulary("", "typ", "alg", "kid", "x5t", "nonce", "jku", "x5u", "cty")

// discoveryVocab covers the members of an OpenID Connect discovery document,
// those in OpenID Connect Discovery 1.0 and the Microsoft ones its documents
// are known to include.
var discoveryVocab = vocabulary("",
	"issuer", "authorization_endpoint", "token_endpoint", "userinfo_endpoint", "jwks_uri", "registration_endpoint",
	"scopes_supported", "response_types_supported", "response_modes_supported", "grant_types_supported",
	"subject_types_supported", "id_token_signing_alg_values_supported", "token_endpoint_auth_methods_supported",
	"claims_supported", "claims_parameter_supported", "request_parameter_supported", "request_uri_parameter_supported",
	"code_challenge_methods_supported", "end_session_endpoint", "frontchannel_logout_supported",
	"http_logout_supported", "backchannel_logout_supported", "check_session_iframe", "device_authorization_endpoint",
	"kerberos_endpoint", "tenant_region_scope", "cloud_instance_name", "cloud_graph_host_name", "msgraph_host",
	"rbac_url", "mtls_endpoint_aliases", "require_request_uri_registration", "claim_types_supported",
)

// jwkVocab covers the members of a JSON Web Key in a key set (RFC 7517) and the
// Microsoft issuer member. The private members (d, p, q, dp, dq, qi) are
// deliberately absent: if a key set ever contained one, the report would show
// "other-N" and not its name.
var jwkVocab = vocabulary("", "kty", "use", "kid", "x5t", "x5t#S256", "n", "e", "x5c", "x5u", "issuer", "alg", "key_ops", "cloud_instance_name")

// jsonTypeName names the JSON type of a decoded value.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "other"
}

// typesOf maps each member name of a decoded JSON object to the type of its
// value.
func typesOf(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = jsonTypeName(v)
	}
	return out
}
