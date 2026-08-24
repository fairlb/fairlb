#!/usr/bin/env bash
# End-to-end check against a running instance: can somebody who just started
# this actually use it?
#
# # Why this exists
#
# Every other check in this repository answers "does the code do what the code
# says". None of them answer "does a person who runs the documented command end
# up with something they can use". Those come apart more often than they sound
# like they should: an instance can compile, pass its tests, answer every health
# probe, and still be impossible to sign in to.
#
# So this drives the product the way the README tells someone to: start it, set
# it up in the browser's stead, sign in, create a key, and call the API with it.
#
# # Usage
#
#   scripts/smoke.sh [base-url]      # default http://localhost:8080
#
# gate-honesty: no skip paths. Every step asserts a specific status code and
# fails the whole run on the first mismatch; there is no branch that reports
# success without having made every call. The one thing it cannot do on its own
# is start the instance — if nothing is listening, the readiness wait times out
# and fails rather than reporting an empty pass.
set -euo pipefail

BASE="${1:-http://localhost:8080}"
EMAIL="${SMOKE_EMAIL:-smoke@example.com}"
PASSWORD="${SMOKE_PASSWORD:-a-long-enough-password}"
JAR="$(mktemp)"
trap 'rm -f "$JAR"' EXIT

say() { printf '\n\033[36m==> %s\033[0m\n' "$*"; }
note() { printf '  \033[90m%s\033[0m\n' "$*"; }
die() { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# Same-origin Origin header on writes: the CSRF guard refuses writes that
# arrive without one, which is the point of it.
ORIGIN="$BASE"

req() { # method path [body] -> prints "status<TAB>body"
    local method=$1 path=$2 body=${3:-}
    local args=(-s -o /dev/null -w '%{http_code}' -X "$method" -b "$JAR" -c "$JAR"
                -H "Origin: $ORIGIN" -H 'Content-Type: application/json')
    [ -n "$body" ] && args+=(-d "$body")
    curl "${args[@]}" "$BASE$path"
}

body_of() { # method path [body] -> prints body
    local method=$1 path=$2 body=${3:-}
    local args=(-s -X "$method" -b "$JAR" -c "$JAR"
                -H "Origin: $ORIGIN" -H 'Content-Type: application/json')
    [ -n "$body" ] && args+=(-d "$body")
    curl "${args[@]}" "$BASE$path"
}

say "waiting for $BASE"
for i in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/staff/v1/meta" || true)
    [ "$code" = "200" ] && { note "ready after ${i}s"; break; }
    [ "$i" = "60" ] && die "not ready after 60s (last status: ${code:-none})"
    sleep 1
done

say "first run state"
meta=$(body_of GET /api/staff/v1/meta)
state=$(printf '%s' "$meta" | sed -n 's/.*"setup_state":"\([a-z]*\)".*/\1/p')
[ -n "$state" ] || die "no setup_state in /meta: $meta"
note "setup_state=$state"

if [ "$state" = "available" ]; then
    say "creating the first administrator"
    code=$(req POST /api/staff/v1/setup "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
    [ "$code" = "204" ] || die "setup answered $code, want 204"
    note "created and signed in"
else
    say "signing in (this instance already has an administrator)"
    code=$(req POST /api/staff/v1/auth/login "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
    [ "$code" = "204" ] || die "sign-in answered $code, want 204"
fi

say "session works"
code=$(req GET /api/staff/v1/auth/me)
[ "$code" = "200" ] || die "/auth/me answered $code — the cookie does not authenticate"

# The cookie has to be the thing carrying the session, not some side effect of
# running on the same host: a request without the jar must be refused.
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/staff/v1/auth/me")
[ "$code" = "401" ] || die "/auth/me answered $code without a cookie, want 401"
note "authenticated with the cookie, refused without it"

say "creating an API key"
# A name that cannot collide with a previous run's. Key names are unique per
# instance, and this script is meant to be run repeatedly against the same one —
# a fixed name works exactly once and then fails with a 400 that looks like a
# product defect. Random rather than a timestamp: two runs can share a second.
suffix=$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')
key_json=$(body_of POST /api/staff/v1/keys "{\"name\":\"smoke-$suffix\"}")
key=$(printf '%s' "$key_json" | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
key_id=$(printf '%s' "$key_json" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$key" ] || die "no plaintext key in the response: $key_json"
[ -n "$key_id" ] || die "no key id in the response: $key_json"
note "key issued (${key:0:12}…)"

say "calling the data plane with it"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $key" "$BASE/v1/models")
[ "$code" = "200" ] || die "/v1/models answered $code with a fresh key, want 200"

# And it has to be the key that opened the door.
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/models")
[ "$code" = "401" ] || die "/v1/models answered $code with no key, want 401"
note "accepted with the key, refused without it"

say "revoking it"
code=$(req DELETE "/api/staff/v1/keys/$key_id")
[ "$code" = "204" ] || die "revoke answered $code, want 204"
# Revocation has to reach the data plane immediately. The endpoint returns 204
# either way; what says the cache was invalidated is the next request failing.
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $key" "$BASE/v1/models")
[ "$code" = "401" ] || die "a revoked key still worked ($code) — revocation did not reach the data plane"
note "revoked, and the data plane stopped accepting it at once"

printf '\n\033[32m✓ smoke passed — set up, signed in, issued a key, called the API\033[0m\n'
