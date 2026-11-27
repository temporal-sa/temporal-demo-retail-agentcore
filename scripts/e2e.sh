#!/bin/sh
# End to end through the published ports, against `make demo`: order, see the
# cart wait for the member with no Worker running, replace the agent's process,
# pay, and see a new Worker finish the order without repeating a model or tool
# call, with one charge in the store's ledger.
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

wait_for "v['order']['phase'] == 'placed' and v['order']['status'] == 'Completed'" 90 "a new Worker resumed and placed the order"
view | python3 -c "
import json, sys
o = json.load(sys.stdin)['order']
p, ledger = o['proof'], o['ledger']
before = int(sys.argv[1])
assert p['repeats'] == 0, p
assert len(p['workers']) >= 2 and p['workers'][0] != p['workers'][-1], p['workers']
assert p['modelCalls'] == before + 1, (before, p['modelCalls'])
for name in ('charge_payment', 'place_order', 'award_stardust'):
    assert sum(r['name'] == name for r in p['rows']) == 1, name
kinds = sorted(e['kind'] for e in ledger['entries'])
assert kinds == ['order', 'payment', 'stardust'], kinds
assert ledger['replays'] == 0, ledger
assert o['placed']['amount'] == 48.24, o['placed']
print(f\"ok: {p['modelCalls']} model calls and {p['toolCalls']} tool calls, each once; one charge of 48.24, order {o['placed']['orderNumber']}\")
" "$before"
echo PASS
