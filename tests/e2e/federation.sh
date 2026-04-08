#!/bin/sh
set -eu

ALICE="http://alice.test:5000"
BOB="http://bob.test:5000"
CHARLIE="http://charlie.test:5000"
ALICE_TOKEN="alice-e2e-token"
BOB_TOKEN="bob-e2e-token"
CHARLIE_TOKEN="charlie-e2e-token"
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

check_absent() {
  name="$1"; shift
  unwanted="$1"; shift
  body=$(curl -sf "$@") || { fail "$name (curl error)"; return; }
  case "$body" in
    *"$unwanted"*) fail "$name — found unwanted '$unwanted' in: $body" ;;
    *) pass "$name" ;;
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

poll_until_status() {
  name="$1"; shift
  expected="$1"; shift
  timeout="$1"; shift
  elapsed=0
  while [ "$elapsed" -lt "$timeout" ]; do
    status=$(curl -so /dev/null -w '%{http_code}' "$@" 2>/dev/null) || true
    if [ "$status" = "$expected" ]; then
      pass "$name"
      return
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  fail "$name — timed out after ${timeout}s waiting for HTTP $expected (last: $status)"
}

# ==============================================================
echo ""
echo "=== Federation E2E Tests ==="
echo ""

# ==============================================================
echo "--- Phase 0: Health ---"
check "alice healthy"   '"status":"ok"' "$ALICE/healthz"
check "bob healthy"     '"status":"ok"' "$BOB/healthz"
check "charlie healthy" '"status":"ok"' "$CHARLIE/healthz"

# ==============================================================
echo "--- Phase 1: Identity ---"
check "alice actor doc"     '"type":"Application"' -H 'Accept: application/activity+json' "$ALICE/ap/actor"
check "bob actor doc"       '"type":"Application"' -H 'Accept: application/activity+json' "$BOB/ap/actor"
check "charlie actor doc"   '"type":"Application"' -H 'Accept: application/activity+json' "$CHARLIE/ap/actor"
check "alice actor has key" '"publicKeyPem"'       -H 'Accept: application/activity+json' "$ALICE/ap/actor"
check "bob actor has key"   '"publicKeyPem"'       -H 'Accept: application/activity+json' "$BOB/ap/actor"
check "alice webfinger"     '"subject"' "$ALICE/.well-known/webfinger?resource=acct:registry@alice.test"
check "bob webfinger"       '"subject"' "$BOB/.well-known/webfinger?resource=acct:registry@bob.test"
check "charlie webfinger"   '"subject"' "$CHARLIE/.well-known/webfinger?resource=acct:registry@charlie.test"

# ==============================================================
echo "--- Phase 2: Pre-follow ---"
check "alice no followers" 'null' -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows"
check "bob no followers"   'null' -H "Authorization: Bearer $BOB_TOKEN"   "$BOB/api/admin/follows"
check "alice no pending"   'null' -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows/pending"
check "bob no pending"     'null' -H "Authorization: Bearer $BOB_TOKEN"   "$BOB/api/admin/follows/pending"

# ==============================================================
echo "--- Phase 3: Auth rejection ---"

# Admin API rejects missing token
check_status "admin no token" "401" \
  "$ALICE/api/admin/follows"

# Admin API rejects wrong token
check_status "admin wrong token" "401" \
  -H "Authorization: Bearer wrong-token" "$ALICE/api/admin/follows"

# Registry push rejects wrong token
check_status "registry push wrong token" "401" \
  -X POST -H "Authorization: Bearer wrong-token" \
  -H "Content-Type: application/octet-stream" \
  --data-raw "test" \
  "$ALICE/v2/alice/test/blobs/uploads/?digest=sha256:0000000000000000000000000000000000000000000000000000000000000000"

# Registry GET is public (no auth needed)
check_status "registry pull is public" "404" \
  "$ALICE/v2/alice/nonexistent/manifests/latest"

# Unsigned POST to inbox is rejected
check_status "unsigned inbox post rejected" "401" \
  -X POST -H "Content-Type: application/activity+json" \
  --data-raw '{"type":"Follow","actor":"http://evil.test/ap/actor","object":"http://alice.test:5000/ap/actor"}' \
  "$ALICE/ap/inbox"

# ==============================================================
echo "--- Phase 4: Follow (Alice → Bob, auto-accept) ---"

follow_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://bob.test:5000/ap/actor"}' \
  "$ALICE/api/admin/follows") || { fail "alice follow bob (curl error)"; }

case "$follow_resp" in
  *"bob"*) pass "alice follow bob" ;;
  *) fail "alice follow bob — got: $follow_resp" ;;
esac

poll_until "bob has alice as follower" 'alice' 15 \
  -H "Authorization: Bearer $BOB_TOKEN" "$BOB/api/admin/follows"

poll_until "alice outgoing follow accepted" 'bob' 15 \
  -H 'Accept: application/activity+json' "$ALICE/ap/following"

# ==============================================================
echo "--- Phase 5: Follow (Bob → Alice, auto-accept) ---"

follow_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://alice.test:5000/ap/actor"}' \
  "$BOB/api/admin/follows") || { fail "bob follow alice (curl error)"; }

case "$follow_resp" in
  *"alice"*) pass "bob follow alice" ;;
  *) fail "bob follow alice — got: $follow_resp" ;;
esac

poll_until "alice has bob as follower" 'bob' 15 \
  -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows"

poll_until "bob outgoing follow accepted" 'alice' 15 \
  -H 'Accept: application/activity+json' "$BOB/ap/following"

# ==============================================================
echo "--- Phase 6: Follow + Reject (Alice → Charlie, manual reject) ---"

# Alice follows Charlie (charlie has autoAccept: none)
follow_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://charlie.test:5000/ap/actor"}' \
  "$ALICE/api/admin/follows") || { fail "alice follow charlie (curl error)"; }

case "$follow_resp" in
  *"charlie"*) pass "alice follow charlie" ;;
  *) fail "alice follow charlie — got: $follow_resp" ;;
esac

# Charlie should have a pending follow request from Alice (not auto-accepted)
poll_until "charlie has pending from alice" 'alice' 15 \
  -H "Authorization: Bearer $CHARLIE_TOKEN" "$CHARLIE/api/admin/follows/pending"

# Charlie's followers list should NOT have Alice yet
check_absent "charlie has no followers yet" 'alice' \
  -H "Authorization: Bearer $CHARLIE_TOKEN" "$CHARLIE/api/admin/follows"

# Charlie rejects Alice's follow
reject_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $CHARLIE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://alice.test:5000/ap/actor"}' \
  "$CHARLIE/api/admin/follows/reject") || { fail "charlie reject alice (curl error)"; }

case "$reject_resp" in
  *"alice"*) pass "charlie reject alice" ;;
  *) fail "charlie reject alice — got: $reject_resp" ;;
esac

# Pending should be empty now
sleep 1
check_absent "charlie no pending after reject" 'alice' \
  -H "Authorization: Bearer $CHARLIE_TOKEN" "$CHARLIE/api/admin/follows/pending"

# ==============================================================
echo "--- Phase 7: Follow + Accept (Bob → Charlie, manual accept) ---"

follow_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://charlie.test:5000/ap/actor"}' \
  "$BOB/api/admin/follows") || { fail "bob follow charlie (curl error)"; }

case "$follow_resp" in
  *"charlie"*) pass "bob follow charlie" ;;
  *) fail "bob follow charlie — got: $follow_resp" ;;
esac

poll_until "charlie has pending from bob" 'bob' 15 \
  -H "Authorization: Bearer $CHARLIE_TOKEN" "$CHARLIE/api/admin/follows/pending"

# Charlie manually accepts Bob
accept_resp=$(curl -sf -X POST \
  -H "Authorization: Bearer $CHARLIE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://bob.test:5000/ap/actor"}' \
  "$CHARLIE/api/admin/follows/accept") || { fail "charlie accept bob (curl error)"; }

case "$accept_resp" in
  *"bob"*) pass "charlie accept bob" ;;
  *) fail "charlie accept bob — got: $accept_resp" ;;
esac

# Bob should now appear in Charlie's followers
poll_until "charlie has bob as follower" 'bob' 15 \
  -H "Authorization: Bearer $CHARLIE_TOKEN" "$CHARLIE/api/admin/follows"

# Bob's outgoing follow to Charlie should be accepted
poll_until "bob outgoing follow to charlie accepted" 'charlie' 15 \
  -H 'Accept: application/activity+json' "$BOB/ap/following"

# ==============================================================
echo "--- Phase 8: Manifest federation ---"

REPO="alice.test/myapp"
BLOB_CONTENT="e2e-test-blob-content-$(date +%s)"
BLOB_DIGEST="sha256:$(printf '%s' "$BLOB_CONTENT" | sha256sum | cut -d' ' -f1)"

blob_body=$(mktemp)
blob_status=$(curl -s -o "$blob_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ALICE_REGISTRY_TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-raw "$BLOB_CONTENT" \
  "$ALICE/v2/$REPO/blobs/uploads/?digest=$BLOB_DIGEST") || true

case "$blob_status" in
  201|202) pass "push blob to alice" ;;
  *) fail "push blob to alice — HTTP $blob_status: $(cat "$blob_body")" ;;
esac
rm -f "$blob_body"

MANIFEST="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"digest\":\"$BLOB_DIGEST\",\"size\":${#BLOB_CONTENT},\"mediaType\":\"application/vnd.oci.image.config.v1+json\"},\"layers\":[]}"
MANIFEST_DIGEST="sha256:$(printf '%s' "$MANIFEST" | sha256sum | cut -d' ' -f1)"

manifest_body=$(mktemp)
manifest_status=$(curl -s -o "$manifest_body" -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ALICE_REGISTRY_TOKEN" \
  -H "Content-Type: application/vnd.oci.image.manifest.v1+json" \
  --data-raw "$MANIFEST" \
  "$ALICE/v2/$REPO/manifests/latest") || true

case "$manifest_status" in
  201) pass "push manifest to alice" ;;
  *) fail "push manifest to alice — HTTP $manifest_status: $(cat "$manifest_body")" ;;
esac
rm -f "$manifest_body"

# Bob (follower) should receive the manifest
poll_until "bob received federated manifest" "$BLOB_DIGEST" 30 \
  "$BOB/v2/$REPO/manifests/latest"

check_status "bob pull manifest by digest" "200" \
  "$BOB/v2/$REPO/manifests/$MANIFEST_DIGEST"

check_status "bob pull blob via pull-through" "200" \
  "$BOB/v2/$REPO/blobs/$BLOB_DIGEST"

# ==============================================================
echo "--- Phase 8b: Repo-scoped blob isolation ---"

# Blobs pushed to alice.test/myapp must NOT be readable from non-existent repos.
check_status "alice blob 404 from nonexistent repo" "404" \
  "$ALICE/v2/library/unknown/blobs/$BLOB_DIGEST"

check_status "alice blob 404 from arbitrary path" "404" \
  "$ALICE/v2/fake/repo/blobs/$BLOB_DIGEST"

check_status "alice manifest 404 from nonexistent repo" "404" \
  "$ALICE/v2/library/unknown/manifests/$MANIFEST_DIGEST"

# Same checks on Bob (federated copy).
check_status "bob blob 404 from nonexistent repo" "404" \
  "$BOB/v2/library/unknown/blobs/$BLOB_DIGEST"

check_status "bob blob 404 from arbitrary path" "404" \
  "$BOB/v2/fake/repo/blobs/$BLOB_DIGEST"

# Blobs are still accessible from the correct repo.
check_status "alice blob 200 from correct repo" "200" \
  "$ALICE/v2/$REPO/blobs/$BLOB_DIGEST"

check_status "bob blob 200 from correct repo" "200" \
  "$BOB/v2/$REPO/blobs/$BLOB_DIGEST"

# ==============================================================
echo "--- Phase 9: Tag update federation ---"

BLOB_CONTENT_V2="e2e-test-blob-v2-$(date +%s)"
BLOB_DIGEST_V2="sha256:$(printf '%s' "$BLOB_CONTENT_V2" | sha256sum | cut -d' ' -f1)"

blob_body=$(mktemp)
blob_status=$(curl -s -o "$blob_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ALICE_REGISTRY_TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-raw "$BLOB_CONTENT_V2" \
  "$ALICE/v2/$REPO/blobs/uploads/?digest=$BLOB_DIGEST_V2") || true

case "$blob_status" in
  201|202) pass "push blob v2 to alice" ;;
  *) fail "push blob v2 to alice — HTTP $blob_status: $(cat "$blob_body")" ;;
esac
rm -f "$blob_body"

MANIFEST_V2="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"digest\":\"$BLOB_DIGEST_V2\",\"size\":${#BLOB_CONTENT_V2},\"mediaType\":\"application/vnd.oci.image.config.v1+json\"},\"layers\":[]}"
MANIFEST_DIGEST_V2="sha256:$(printf '%s' "$MANIFEST_V2" | sha256sum | cut -d' ' -f1)"

manifest_body=$(mktemp)
manifest_status=$(curl -s -o "$manifest_body" -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ALICE_REGISTRY_TOKEN" \
  -H "Content-Type: application/vnd.oci.image.manifest.v1+json" \
  --data-raw "$MANIFEST_V2" \
  "$ALICE/v2/$REPO/manifests/latest") || true

case "$manifest_status" in
  201) pass "push manifest v2 to alice (tag update)" ;;
  *) fail "push manifest v2 to alice — HTTP $manifest_status: $(cat "$manifest_body")" ;;
esac
rm -f "$manifest_body"

# Bob should receive the updated tag pointing to the new manifest
poll_until "bob received tag update" "$BLOB_DIGEST_V2" 30 \
  "$BOB/v2/$REPO/manifests/latest"

check_status "bob pull updated manifest by digest" "200" \
  "$BOB/v2/$REPO/manifests/$MANIFEST_DIGEST_V2"

# Old manifest should still be accessible by digest
check_status "bob still has old manifest by digest" "200" \
  "$BOB/v2/$REPO/manifests/$MANIFEST_DIGEST"

# ==============================================================
echo "--- Phase 10: Unfollow + verification ---"

# Alice unfollows Bob
unfollow_resp=$(curl -sf -X DELETE \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://bob.test:5000/ap/actor"}' \
  "$ALICE/api/admin/follows") || { fail "alice unfollow bob (curl error)"; }

case "$unfollow_resp" in
  *"bob"*) pass "alice unfollow bob" ;;
  *) fail "alice unfollow bob — got: $unfollow_resp" ;;
esac

# Bob should no longer have Alice as follower
poll_until_status "bob /ap/followers empty after unfollow" "200" 10 \
  -H 'Accept: application/activity+json' "$BOB/ap/followers"

# Give the Undo time to propagate
sleep 2

# Alice should not appear in Bob's followers anymore
check_absent "bob followers excludes alice after unfollow" 'alice' \
  -H "Authorization: Bearer $BOB_TOKEN" "$BOB/api/admin/follows"

# Bob unfollows Alice
unfollow_resp=$(curl -sf -X DELETE \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target":"http://alice.test:5000/ap/actor"}' \
  "$BOB/api/admin/follows") || { fail "bob unfollow alice (curl error)"; }

case "$unfollow_resp" in
  *"alice"*) pass "bob unfollow alice" ;;
  *) fail "bob unfollow alice — got: $unfollow_resp" ;;
esac

sleep 2

check_absent "alice followers excludes bob after unfollow" 'bob' \
  -H "Authorization: Bearer $ALICE_TOKEN" "$ALICE/api/admin/follows"

# ==============================================================
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
echo ""

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
