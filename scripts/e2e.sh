#!/bin/sh
# End to end through the published ports, against `make demo`: order, see the
# cart wait for the member with no Worker running, replace the agent's process,
# pay, and see a new Worker finish the order without repeating a model or tool
# call, with one charge in the store's ledger. Then the kitchen marks it ready
# and another new Worker tells the member. Then two support chats: a refund
# the policy allows on the tap, and a full refund that waits for a person
# (E2E_TIMERS=1 lets the review timers decide it instead, about 3 minutes).
set -eu
API=${API:-http://localhost:8080}
JAR=$(mktemp); OTHER=$(mktemp); trap 'rm -f "$JAR" "$OTHER"' EXIT
ID=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
TEXT="Same crew order as last Friday, for Titan Ring. Mo's vegan now. Use whatever reward saves the most."

field() { python3 -c "import json,sys; v=json.load(sys.stdin); print(eval(sys.argv[1], {}, {'v': v}))" "$1"; }
view() { curl -sf -b "$JAR" -c "$JAR" "$API/api/view?order=$ID"; }
post() { curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -c "$JAR" -H 'Content-Type: application/json' -d "$2" "$API$1"; }
wait_for() { # wait_for <python expr on v> <seconds> <what>
  i=0
  until [ "$(view | field "$1")" = True ]; do
    i=$((i + 1)); [ "$i" -le "$2" ] || { echo "FAIL: $3"; view; exit 1; }
    sleep 1
  done
  echo "ok: $3 (${i}s)"
}

curl -sf -c "$JAR" "$API/healthz" >/dev/null
code=$(post /api/orders "{\"id\":\"$ID\",\"text\":\"$TEXT\"}")
[ "$code" = 201 ] || { echo "FAIL: start returned $code"; exit 1; }
echo "ok: started order $ID"

wait_for "v['order']['phase'] == 'confirm'" 90 "Ziggy priced a cart and is waiting for the member"
view | python3 -c "
import json, sys
q = json.load(sys.stdin)['order']['quote']
assert (q['subtotal'], q['reward']['id'], q['total'], q['earns']) == (56.75, 'crew-pack', 48.24, 482), q
print('ok: cart is 56.75, Crew Pack saves 8.51, total 48.24')
"
before=$(view | field "v['order']['proof']['modelCalls']")
quote=$(view | field "v['order']['quote']['quoteId']")
wait_for "not v['worker']['running']" 90 "the Worker drained; no compute while the member decides"

code=$(curl -s -o /dev/null -w '%{http_code}' -b "$OTHER" "$API/api/view?order=$ID")
[ "$code" = 404 ] || { echo "FAIL: another session saw the order ($code)"; exit 1; }
echo "ok: another browser session cannot see it"

docker compose kill agent >/dev/null 2>&1 && docker compose start agent >/dev/null 2>&1
echo "ok: replaced the agent process (a new AgentCore session)"
[ "$(view | field "v['order']['phase']")" = confirm ] || { echo "FAIL: lost its place"; exit 1; }

code=$(post "/api/orders/$ID/pay" "{\"quoteId\":\"$quote\"}")
[ "$code" = 202 ] || { echo "FAIL: pay returned $code"; exit 1; }
echo "ok: payment accepted with no Worker running"

wait_for "v['order']['phase'] == 'preparing'" 90 "a new Worker resumed and placed the order"
# Sandbox 2 finishes its work in seconds, so see it running before seeing it drain.
wait_for "v['worker']['running']" 30 "sandbox 2 is running"
wait_for "not v['worker']['running']" 90 "the Worker drained while the kitchen makes the order"

code=$(post "/api/orders/$ID/ready" '{}')
[ "$code" = 202 ] || { echo "FAIL: ready returned $code"; exit 1; }
echo "ok: kitchen marked it ready with no Worker running"

wait_for "v['order']['phase'] == 'ready' and v['order']['status'] == 'Completed'" 90 "a new Worker told the member it's ready for pickup"
view | python3 -c "
import json, sys
o = json.load(sys.stdin)['order']
p, ledger = o['proof'], o['ledger']
before = int(sys.argv[1])
assert p['repeats'] == 0, p
assert len(p['workers']) >= 3 and p['workers'][0] != p['workers'][-1], p['workers']
assert p['modelCalls'] == before + 2, (before, p['modelCalls'])
for name in ('charge_payment', 'place_order', 'award_stardust'):
    assert sum(r['name'] == name for r in p['rows']) == 1, name
kinds = sorted(e['kind'] for e in ledger['entries'])
assert kinds == ['order', 'payment', 'stardust'], kinds
assert ledger['replays'] == 0, ledger
assert o['placed']['amount'] == 48.24, o['placed']
print(f\"ok: {p['modelCalls']} model calls and {p['toolCalls']} tool calls, each once; one charge of 48.24, order {o['placed']['orderNumber']}\")
" "$before"

# Support: a missing wrap is refunded on the member's tap; a full refund
# waits, inside the agent's issue_refund tool, for a person.
SID=""
sview() { curl -sf -b "$JAR" -c "$JAR" "$API/api/view?order=$ID&support=$SID"; }
swait() { # swait <python expr on v> <seconds> <what>
  i=0
  until [ "$(sview | field "$1")" = True ]; do
    i=$((i + 1)); [ "$i" -le "$2" ] || { echo "FAIL: $3"; sview; exit 1; }
    sleep 1
  done
  echo "ok: $3 (${i}s)"
}
support() { # support <opening message>
  SID=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
  code=$(post "/api/orders/$ID/support" "{\"id\":\"$SID\"}")
  [ "$code" = 201 ] || { echo "FAIL: support start returned $code"; exit 1; }
  code=$(post "/api/orders/$ID/support/$SID/messages" "{\"text\":\"$1\"}")
  [ "$code" = 202 ] || { echo "FAIL: support message returned $code"; exit 1; }
  swait "v['support']['phase'] == 'choosing' and bool(v['support']['offer'])" 90 "Ziggy offered choices for \"$1\""
  first=$(sview | field "v['support']['offer'][0]['id']")
  code=$(post "/api/orders/$ID/support/$SID/choose" "{\"optionId\":\"$first\"}")
  [ "$code" = 202 ] || { echo "FAIL: choose returned $code"; exit 1; }
}

support "My order was missing the wrap"
swait "v['support']['status'] == 'Completed'" 90 "the refund ran on the tap and the chat closed"
sview | python3 -c "
import json, sys
s = json.load(sys.stdin)['support']
assert s['resolution']['kind'] == 'refund' and s['resolution']['decidedBy'] == 'policy', s['resolution']
assert not s.get('escalation'), s['escalation']
assert [e['kind'] for e in s['ledger']['entries']] == ['refund'], s['ledger']
print(f\"ok: {s['resolution']['summary']}, no review needed\")
"

support "My entire order was terrible"
swait "v['support']['phase'] == 'review' and v['support']['escalation']['tier'] == 'support'" 90 "the full refund is waiting on a support agent, inside the tool"
[ "$(sview | field "len((v['support'].get('ledger') or {}).get('entries') or [])")" = 0 ] || { echo "FAIL: refunded before review"; exit 1; }
swait "not v['worker']['running']" 90 "the sandbox ended while the tool call waits"
if [ "${E2E_TIMERS:-0}" = 1 ]; then
  swait "v['support']['escalation']['tier'] == 'manager'" 120 "nobody reviewed it, so the timer moved it to a manager"
  swait "v['support']['status'] == 'Completed'" 150 "nobody reviewed it, so the timer approved it"
  [ "$(sview | field "v['support']['escalation']['decidedBy']")" = timer ] || { echo "FAIL: not approved by the timer"; exit 1; }
else
  code=$(post "/api/orders/$ID/support/$SID/review" '{"approve":true}')
  [ "$code" = 202 ] || { echo "FAIL: review returned $code"; exit 1; }
  echo "ok: a support agent approved it with no Worker running"
  swait "v['support']['status'] == 'Completed'" 90 "a new sandbox carried on inside the tool and refunded"
fi
sview | python3 -c "
import json, sys
s = json.load(sys.stdin)['support']
refunds = [e for e in s['ledger']['entries'] if e['kind'] == 'refund']
assert len(refunds) == 1 and refunds[0]['result']['amount'] == 48.24 and s['ledger']['replays'] == 0, s['ledger']
assert s['proof']['repeats'] == 0, s['proof']
print('ok: one refund of 48.24, nothing repeated')
"
echo PASS
