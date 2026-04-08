#!/bin/sh
set -eu

ALICE="http://alice:5000"
BOB="http://bob:5000"
ALICE_TOKEN="alice-e2e-token"
BOB_TOKEN="bob-e2e-token"
ALICE_REGISTRY_TOKEN="alice-registry-token"
BOB_REGISTRY_TOKEN="bob-registry-token"

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  FAIL: $1"; }

check() {
  name="$1"; shift
  expected="$1"; shift
  body=$(curl -sf "$@") || { fail "$name (curl error)"; return; }
  case "$body" in
    *"$expected"*) pass "$name" ;;
    *) fail "$name — expected '$expected', got: $body" ;;
  esac
}

check_status() {
  name="$1"; shift
  expected="$1"; shift
  status=$(curl -so /dev/null -w '%{http_code}' "$@") || true
  if [ "$status" = "$expected" ]; then
    pass "$name"
  else
    fail "$name — expected HTTP $expected, got $status"
  fi
}

poll_until() {
  name="$1"; shift
  expected="$1"; shift
  timeout="$1"; shift
  elapsed=0
  while [ "$elapsed" -lt "$timeout" ]; do
    body=$(curl -sf "$@" 2>/dev/null) || true
    case "$body" in
      *"$expected"*) pass "$name"; return ;;
    esac
    sleep 1
    elapsed=$((elapsed + 1))
  done
  fail "$name — timed out after ${timeout}s waiting for '$expected'"
}

# ==============================================================
echo ""
echo "=== Federation E2E Tests ==="
echo ""

# --- Health ---
echo "--- Phase 0: Health ---"
check "alice healthy" '"status":"ok"' "$ALICE/healthz"
check "bob healthy"   '"status":"ok"' "$BOB/healthz"

# --- Actor documents ---
echo "--- Phase 1: Identity ---"
check "alice actor doc"     '"type":"Application"' -H 'Accept: application/activity+json' "$ALICE/ap/actor"
check "bob actor doc"       '"type":"Application"' -H 'Accept: application/activity+json' "$BOB/ap/actor"
check "alice actor has key" '"publicKeyPem"'       -H 'Accept: application/activity+json' "$ALICE/ap/actor"
check "bob actor has key"   '"publicKeyPem"'       -H 'Accept: application/activity+json' "$BOB/ap/actor"
check "alice webfinger"     '"subject"' "$ALICE/.well-known/webfinger?resource=acct:registry@alice"
check "bob webfinger"       '"subject"' "$BOB/.well-known/webfinger?resource=acct:registry@bob"

# --- Pre-follow state ---
echo "--- Phase 2: Pre-follow ---"
check "alice no followers" '[]' -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows"
check "bob no followers"   '[]' -H "Authorization: Bearer $BOB_TOKEN"   "$BOB/api/admin/follows"
check "alice no pending"   '[]' -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows/pending"
check "bob no pending"     '[]' -H "Authorization: Bearer $BOB_TOKEN"   "$BOB/api/admin/follows/pending"

# --- Alice follows Bob (signed Follow → signature verification → auto-Accept) ---
echo "--- Phase 3: Follow (Alice → Bob) ---"

follow_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://bob:5000/ap/actor"}' \
  "$ALICE/api/admin/follows") || { fail "alice follow bob (curl error)"; }

case "$follow_resp" in
  *"bob"*) pass "alice follow bob" ;;
  *) fail "alice follow bob — got: $follow_resp" ;;
esac

poll_until "bob has alice as follower" 'alice' 15 \
  -H "Authorization: Bearer $BOB_TOKEN" "$BOB/api/admin/follows"

# /ap/following lists accepted outgoing follows — proves Alice received Bob's signed Accept.
poll_until "alice outgoing follow accepted" 'bob' 15 \
  -H 'Accept: application/activity+json' "$ALICE/ap/following"

# --- Bob follows Alice (reverse direction) ---
echo "--- Phase 4: Follow (Bob → Alice) ---"

follow_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://alice:5000/ap/actor"}' \
  "$BOB/api/admin/follows") || { fail "bob follow alice (curl error)"; }

case "$follow_resp" in
  *"alice"*) pass "bob follow alice" ;;
  *) fail "bob follow alice — got: $follow_resp" ;;
esac

poll_until "alice has bob as follower" 'bob' 15 \
  -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows"

poll_until "bob outgoing follow accepted" 'alice' 15 \
  -H 'Accept: application/activity+json' "$BOB/ap/following"

# --- Push manifest on Alice, verify Bob receives it ---
echo "--- Phase 5: Manifest federation ---"

REPO="alice/myapp"
BLOB_CONTENT="e2e-test-blob-content-$(date +%s)"
BLOB_DIGEST="sha256:$(printf '%s' "$BLOB_CONTENT" | sha256sum | cut -d' ' -f1)"

blob_status=$(curl -so /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ALICE_REGISTRY_TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-raw "$BLOB_CONTENT" \
  "$ALICE/v2/$REPO/blobs/uploads/?digest=$BLOB_DIGEST") || true

case "$blob_status" in
  201|202) pass "push blob to alice" ;;
  *) fail "push blob to alice — HTTP $blob_status" ;;
esac

MANIFEST="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"digest\":\"$BLOB_DIGEST\",\"size\":${#BLOB_CONTENT},\"mediaType\":\"application/vnd.oci.image.config.v1+json\"},\"layers\":[]}"
MANIFEST_DIGEST="sha256:$(printf '%s' "$MANIFEST" | sha256sum | cut -d' ' -f1)"

manifest_status=$(curl -so /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ALICE_REGISTRY_TOKEN" \
  -H "Content-Type: application/vnd.oci.image.manifest.v1+json" \
  --data-raw "$MANIFEST" \
  "$ALICE/v2/$REPO/manifests/latest") || true

case "$manifest_status" in
  201) pass "push manifest to alice" ;;
  *) fail "push manifest to alice — HTTP $manifest_status" ;;
esac

poll_until "bob received federated manifest" "$MANIFEST_DIGEST" 30 \
  "$BOB/v2/$REPO/manifests/latest"

check_status "bob pull manifest by digest" "200" \
  "$BOB/v2/$REPO/manifests/$MANIFEST_DIGEST"

check_status "bob pull blob via pull-through" "200" \
  "$BOB/v2/$REPO/blobs/$BLOB_DIGEST"

# --- Unfollow ---
echo "--- Phase 6: Unfollow ---"

unfollow_resp=$(curl -sf -X DELETE \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://bob:5000/ap/actor"}' \
  "$ALICE/api/admin/follows") || { fail "alice unfollow bob (curl error)"; }

case "$unfollow_resp" in
  *"bob"*) pass "alice unfollow bob" ;;
  *) fail "alice unfollow bob — got: $unfollow_resp" ;;
esac

# ==============================================================
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
echo ""

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
