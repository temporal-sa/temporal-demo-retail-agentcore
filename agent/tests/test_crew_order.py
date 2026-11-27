"""CrewOrderWorkflow against Temporal's time-skipping test server, the stub model and a fake store.

These check what the demo claims: the agent builds the cart, the Workflow
waits for payment with no Worker running, a different Worker picks up the
payment and finishes the order, and nothing runs twice: no model call, no
tool call, and no charge.
"""

from __future__ import annotations

import json
import os
import uuid
from collections import Counter
from datetime import timedelta
from pathlib import Path

import httpx
import pytest
from temporalio.api.enums.v1 import EventType
from temporalio.client import Client, WorkflowHandle
from temporalio.strands_agents import StrandsPlugin
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from crew import activities
from crew.models import MODEL_NAME, StubModel
from crew.support import ESCALATED, TO_MANAGER, Choice, OrderContext, OrderLine, Report, ReportedItem, Review, SupportChatWorkflow, SupportRequest, SupportResult, check
from crew.workflows import CrewOrderWorkflow, KitchenReady, MemberMessage, OrderRequest, OrderResult, Payment

TASK_QUEUE = "crew-test"
# The store's own skin, so the fake answers with the same cafés, menu and
# orders. Compose mounts it into the test container (docker-compose.yml).
SKIN = json.loads(Path(os.environ.get("SKIN_PATH", Path(__file__).parents[2] / "store/internal/store/skin.json")).read_text())


class FakeStore:
    """Answers the store API's routes from the real skin, with canned prices.

    It prices every line at 1.00 cr so the test needs none of the store's
    pricing rules (the Go store's own tests cover those). It records every
    write by Idempotency-Key, as the real store does.
    """

    def __init__(self) -> None:
        self.writes: Counter[str] = Counter()
        self.quotes: list[dict] = []
        # Set to refuse every cart, as the store does one it can't make.
        self.refuse_quotes = False
        # The order's support record, as GET /orders/{key}/support returns it.
        self.record = {"paid": 48.24, "refunded": 0.0, "refundable": 48.24, "remediations": []}

    def remediate(self, kind: str, key: str, **fields: object) -> None:
        if any(r["key"] == key for r in self.record["remediations"]):
            return
        self.record["remediations"].append({"kind": kind, "key": key, "items": [], "reason": "", "ref": "", "amount": 0, **fields})
        if kind == "refund":
            self.record["refunded"] = round(self.record["refunded"] + float(fields["amount"]), 2)  # type: ignore[arg-type]
            self.record["refundable"] = round(self.record["paid"] - self.record["refunded"], 2)

    def __call__(self, request: httpx.Request) -> httpx.Response:
        path = request.url.path.removeprefix("/store/v1")
        member, cafe = SKIN["members"][0], SKIN["cafes"][0]
        # Exact paths, so a tool sent a malformed ID fails here as it does on the store.
        mine, here = f"/members/{member['id']}", f"/cafes/{cafe['id']}"
        if path == f"{mine}/orders" and request.method == "GET":
            return httpx.Response(200, json={"memberId": member["id"], "orders": member["orders"]})
        if path == f"{here}/menu":
            items = [{**m, "veganAsServed": not set(m["ingredients"]) & set(SKIN["nonVegan"])} for m in SKIN["menu"]]
            return httpx.Response(200, json={"cafeName": cafe["name"], "items": items, "options": SKIN["options"], "nonVeganIngredients": SKIN["nonVegan"]})
        if path == f"{here}/stock":
            return httpx.Response(200, json={"cafeName": cafe["name"], "outOfStock": cafe["outOfStock"], "pickupMinutes": 25})
        if path == f"{mine}/rewards":
            return httpx.Response(200, json={"stardust": member["stardust"], "rewards": member["rewards"]})
        if path == "/quotes":
            body = json.loads(request.content)
            if body["memberId"] != member["id"] or body["cafeId"] != cafe["id"]:
                return httpx.Response(422, json={"title": "Cart refused", "errors": ["quoted for the wrong member or café"]})
            if self.refuse_quotes:
                return httpx.Response(422, json={"title": "Cart refused", "errors": ["Nebula Latte is not on the menu today"]})
            count = sum(line["qty"] for line in body["lines"])
            quote = {"quoteId": f"q-{len(self.quotes)}", "lines": body["lines"], "itemCount": count, "subtotal": count, "total": count, "earns": count * 10, "reward": None}
            self.quotes.append(quote)
            return httpx.Response(200, json=quote)
        if path.startswith("/orders/") and path.endswith("/support"):
            return httpx.Response(200, json=self.record)
        key = request.headers["Idempotency-Key"]
        self.writes[f"{path}#{key}"] += 1
        replayed = {"Idempotent-Replayed": "true"} if self.writes[f"{path}#{key}"] > 1 else {}
        if path == "/payments":
            return httpx.Response(201, headers=replayed, json={"paymentId": "ch_test", "amount": 12.0})
        if path == "/orders":
            return httpx.Response(201, headers=replayed, json={"orderNumber": "OG-4242", "cafeName": cafe["name"], "readyAt": "2026-11-26T12:40:00Z"})
        if path == "/refunds":
            body = json.loads(request.content)
            self.remediate("refund", key, amount=body["amount"], items=body["items"], reason=body["reason"], ref="rf_test")
            return httpx.Response(201, headers=replayed, json={"refundId": "rf_test", "amount": body["amount"]})
        if path == "/comps":
            body = json.loads(request.content)
            self.remediate("comp", key, items=[line["itemId"] for line in body["lines"]], reason=body["reason"], ref="OG-5000")
            return httpx.Response(201, headers=replayed, json={"orderNumber": "OG-5000", "cafeName": cafe["name"], "readyAt": "2026-11-26T13:10:00Z", "value": 6.4, "total": 0})
        if path.endswith("/stardust"):
            body = json.loads(request.content)
            return httpx.Response(201, headers=replayed, json={"points": body["points"], "balance": 2430})
        return httpx.Response(404, json={"title": "No such route"})


@pytest.fixture(scope="session")
async def env():
    env = await WorkflowEnvironment.start_time_skipping()
    yield env
    await env.shutdown()


@pytest.fixture
def store():
    fake = FakeStore()
    activities.store._client = httpx.AsyncClient(base_url="http://store/store/v1", transport=httpx.MockTransport(fake))
    yield fake
    activities.store._client = None


def worker(client: Client, identity: str) -> Worker:
    return Worker(
        client,
        task_queue=TASK_QUEUE,
        identity=identity,
        # No cache: every Workflow Task replays the whole history, as a new
        # Worker on a new AgentCore session does.
        max_cached_workflows=0,
        workflows=[CrewOrderWorkflow, SupportChatWorkflow],
        activities=activities.TOOLS + activities.CHECKOUT + activities.SUPPORT,
        plugins=[StrandsPlugin(models={MODEL_NAME: lambda: StubModel(delay_seconds=0)})],
    )


async def start(client: Client) -> WorkflowHandle:
    return await client.start_workflow(
        CrewOrderWorkflow.run,
        OrderRequest(member_id="zora-vex", cafe_id="titan-ring", text="Same crew order as last Friday, for Titan Ring. Mo's vegan now."),
        id=f"crew-test-{uuid.uuid4().hex[:8]}",
        task_queue=TASK_QUEUE,
        result_type=OrderResult,
    )


async def memo(handle: WorkflowHandle) -> dict:
    # The same read the API makes: Describe, not a Query.
    return await (await handle.describe()).memo()


async def wait_for_phase(env: WorkflowEnvironment, handle: WorkflowHandle, phase: str) -> dict:
    for _ in range(300):
        m = await memo(handle)
        if m.get("phase") == phase:
            return m
        await env.sleep(timedelta(milliseconds=100))
    raise AssertionError(f"never reached {phase}; memo is {await memo(handle)}")


async def activity_attempts(handle: WorkflowHandle) -> tuple[Counter[str], list[str]]:
    scheduled: dict[int, str] = {}
    calls: Counter[str] = Counter()
    identities: list[str] = []
    async for e in handle.fetch_history_events():
        if e.event_type == EventType.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
            scheduled[e.event_id] = e.activity_task_scheduled_event_attributes.activity_type.name
        if e.event_type == EventType.EVENT_TYPE_ACTIVITY_TASK_STARTED:
            a = e.activity_task_started_event_attributes
            calls[scheduled[a.scheduled_event_id]] += a.attempt
            if a.identity not in identities:
                identities.append(a.identity)
    return calls, identities


async def test_builds_the_cart_waits_checks_out_once_and_waits_for_the_kitchen(env: WorkflowEnvironment, store: FakeStore):
    async with worker(env.client, "sandbox-1"):
        handle = await start(env.client)
        m = await wait_for_phase(env, handle, "confirm")
    # Sandbox 1 is gone. The cart is in the memo, from the history.
    lines = {(line["itemId"], tuple(line["options"]), line["note"]) for line in m["quote"]["lines"]}
    assert ("nebula-latte", ("milk:almond",), "") in lines, "oat milk is out, so the latte is almond"
    assert ("supernova-mocha", ("milk:almond", "no-cream"), "Mo") in lines, "Mo's mocha is made vegan"
    assert ("ceres-veggie-wrap", (), "Mo") in lines, "Mo's egg wrap is swapped for a vegan one"
    assert [s["tool"] for s in m["work"]] == ["get_recent_orders", "get_menu", "get_stock", "get_rewards", "quote_cart"]
    before, _ = await activity_attempts(handle)

    await handle.signal(CrewOrderWorkflow.pay, Payment(quote_id=m["quote"]["quoteId"]))
    async with worker(env.client, "sandbox-2"):
        m = await wait_for_phase(env, handle, "preparing")
    assert "ready for pickup" in m["messages"][-1]["text"]
    placed, _ = await activity_attempts(handle)

    # Sandbox 2 is gone too. The kitchen's Signal brings up sandbox 3.
    await handle.signal(CrewOrderWorkflow.kitchen_ready, KitchenReady())
    async with worker(env.client, "sandbox-3"):
        result = await handle.result()

    assert result.phase == "ready" and result.order_number == "OG-4242"
    calls, identities = await activity_attempts(handle)
    assert identities == ["sandbox-1", "sandbox-2", "sandbox-3"]
    # Every Activity ran once, on one attempt. Sandbox 2 added the checkout
    # and one model call for the reply, sandbox 3 one model call for the
    # pickup message, and neither repeated anything before it.
    assert all(n == 1 for name, n in calls.items() if name != "invoke_model"), calls
    assert placed["invoke_model"] == before["invoke_model"] + 1
    assert calls["invoke_model"] == before["invoke_model"] + 2
    assert {k: v for k, v in calls.items() if k in ("charge_payment", "place_order", "award_stardust")} == {"charge_payment": 1, "place_order": 1, "award_stardust": 1}
    assert all(n == 1 for n in store.writes.values()), store.writes
    final = await memo(handle)
    assert final["order"]["orderNumber"] == "OG-4242" and final["messages"][-1]["from"] == "ziggy"
    assert "ready for pickup" in final["messages"][-1]["text"]


async def test_a_stale_payment_is_ignored_and_a_message_gets_a_reply(env: WorkflowEnvironment, store: FakeStore):
    async with worker(env.client, "sandbox-1"):
        handle = await start(env.client)
        m = await wait_for_phase(env, handle, "confirm")
        await handle.signal(CrewOrderWorkflow.pay, Payment(quote_id="q-from-another-cart"))
        await handle.signal(CrewOrderWorkflow.message, MemberMessage(text="Add a donut"))
        for _ in range(300):
            m = await memo(handle)
            if len(m["messages"]) == 4 and m["phase"] == "confirm":
                break
            await env.sleep(timedelta(milliseconds=100))
        assert [msg["from"] for msg in m["messages"]] == ["member", "ziggy", "member", "ziggy"]
        assert not store.writes, "a payment for another cart charged nothing"
        await handle.terminate("test done")


def test_the_order_tools_never_ask_the_model_who_or_where():
    req = OrderRequest(member_id="zora-vex", cafe_id="titan-ring", text="")
    specs = {t.tool_name: t.tool_spec["inputSchema"]["json"].get("properties", {}) for t in CrewOrderWorkflow._tools(object(), req)}  # type: ignore[arg-type]
    assert set(specs) == {"get_recent_orders", "get_menu", "get_stock", "get_rewards", "quote_cart"}
    assert set(specs["quote_cart"]) == {"lines", "reward_id"}
    assert not any(specs[name] for name in specs if name != "quote_cart"), specs


async def test_a_refused_cart_reaches_the_model_with_the_stores_reason(env: WorkflowEnvironment, store: FakeStore):
    store.refuse_quotes = True
    async with worker(env.client, "sandbox-1"):
        handle = await start(env.client)
        m = await wait_for_phase(env, handle, "chat")
        await handle.terminate("test done")
    assert m.get("quote") is None, "a refused cart is never offered for payment"
    assert m["work"][-1] == {**m["work"][-1], "tool": "quote_cart", "status": "failed"}
    assert "Nebula Latte is not on the menu today" in m["messages"][-1]["text"]


# ---------- the support chat ----------

ORDER = OrderContext(
    order_key="crew-test-order", order_number="OG-4242", cafe_id="titan-ring", cafe_name="Titan Ring", member_id="zora-vex", member_name="Zora Vex",
    payment_id="ch_test", amount_paid=48.24,
    lines=[
        OrderLine(item_id="comet-cold-brew", name="Comet cold brew", qty=2, unit_price=4.95, line_price=9.90),
        OrderLine(item_id="ceres-veggie-wrap", name="Ceres veggie wrap", qty=1, unit_price=6.40, line_price=6.40, note="Mo"),
        OrderLine(item_id="zero-g-donut", name="Zero-G donut", qty=3, unit_price=2.40, line_price=7.20),
    ],
)


async def start_support(client: Client, **timeouts: int) -> WorkflowHandle:
    return await client.start_workflow(
        SupportChatWorkflow.run,
        SupportRequest(order=ORDER, greeting="Hi Zora, what went wrong with OG-4242?", **timeouts),
        id=f"crew-test-support-{uuid.uuid4().hex[:8]}",
        task_queue=TASK_QUEUE,
        result_type=SupportResult,
    )


async def test_a_missing_item_is_refunded_once_after_the_member_taps(env: WorkflowEnvironment, store: FakeStore):
    async with worker(env.client, "sandbox-1"):
        handle = await start_support(env.client)
        await wait_for_phase(env, handle, "listening")
        await handle.signal(SupportChatWorkflow.message, MemberMessage(text="My order was missing the wrap"))
        m = await wait_for_phase(env, handle, "choosing")
    # The agent is in the middle of its offer_choices call, and sandbox 1 is gone.
    labels = [o["label"] for o in m["offer"]]
    assert labels == ["Refund (6.40 cr)", "Make it again at no charge", "Something else"], labels
    await handle.signal(SupportChatWorkflow.choose, Choice(option_id=m["offer"][0]["id"]))
    async with worker(env.client, "sandbox-2"):
        result = await handle.result()
    assert result.outcome == "resolved"
    final = await memo(handle)
    assert final["resolution"]["kind"] == "refund" and final["resolution"]["decidedBy"] == "policy"
    assert not final.get("escalation"), "6.40 cr for the wrap needs no one's approval"
    assert [k.split("#")[0] for k in store.writes] == ["/refunds"] and all(n == 1 for n in store.writes.values())
    assert final["messages"][-1]["from"] == "ziggy" and "closed" in final["messages"][-1]["text"]


async def test_a_full_refund_waits_for_a_support_agent(env: WorkflowEnvironment, store: FakeStore):
    async with worker(env.client, "sandbox-1"):
        handle = await start_support(env.client)
        await wait_for_phase(env, handle, "listening")
        await handle.signal(SupportChatWorkflow.message, MemberMessage(text="My entire order was terrible"))
        m = await wait_for_phase(env, handle, "choosing")
        await handle.signal(SupportChatWorkflow.choose, Choice(option_id=m["offer"][0]["id"]))
        m = await wait_for_phase(env, handle, "review")
        assert m["escalation"]["tier"] == "support" and m["escalation"]["what"] == "Refund 48.24 cr"
        assert m["messages"][-1]["text"] == ESCALATED
        assert not store.writes, "nothing is refunded before someone approves"
        await handle.signal(SupportChatWorkflow.review, Review(approve=True))
        result = await handle.result()
    assert result.outcome == "resolved"
    final = await memo(handle)
    assert final["escalation"]["decidedBy"] == "support" and final["resolution"]["amount"] == 48.24


async def test_an_unreviewed_refund_goes_to_a_manager_then_the_timer_approves_it(env: WorkflowEnvironment, store: FakeStore):
    async with worker(env.client, "sandbox-1"):
        handle = await start_support(env.client)
        await wait_for_phase(env, handle, "listening")
        await handle.signal(SupportChatWorkflow.message, MemberMessage(text="My entire order was terrible"))
        m = await wait_for_phase(env, handle, "choosing")
        await handle.signal(SupportChatWorkflow.choose, Choice(option_id=m["offer"][0]["id"]))
        result = await handle.result()  # time skips through both review timers
    assert result.outcome == "resolved"
    final = await memo(handle)
    said = [msg["text"] for msg in final["messages"] if msg["from"] == "ziggy"]
    assert ESCALATED in said and TO_MANAGER in said
    assert final["escalation"]["decidedBy"] == "timer"
    assert sum(store.writes.values()) == 1


async def test_a_report_from_the_card_names_the_items_and_an_earlier_refund_is_not_repeated(env: WorkflowEnvironment, store: FakeStore):
    report = Report(scope="items", items=[ReportedItem(item_id="ceres-veggie-wrap", problem="missing")])
    async with worker(env.client, "sandbox-1"):
        handle = await start_support(env.client)
        await wait_for_phase(env, handle, "listening")
        await handle.signal(SupportChatWorkflow.report, report)
        m = await wait_for_phase(env, handle, "choosing")
        assert m["messages"][-2] == {**m["messages"][-2], "from": "member", "text": "Missing: Ceres veggie wrap"}
        await handle.signal(SupportChatWorkflow.choose, Choice(option_id=m["offer"][0]["id"]))
        assert (await handle.result()).outcome == "resolved"
        assert store.record["refunded"] == 6.40

        # A second chat about the same wrap: the agent reads the record first.
        again = await start_support(env.client)
        m = await wait_for_phase(env, again, "listening")
        assert m["record"]["refunded"] == 6.40
        await again.signal(SupportChatWorkflow.report, report)
        for _ in range(300):
            m = await memo(again)
            said = m.get("messages", [])
            if len(said) > 2 and said[-1]["from"] == "ziggy" and m["phase"] == "listening":
                break
            await env.sleep(timedelta(milliseconds=100))
        assert "already refunded" in m["messages"][-1]["text"], m["messages"][-1]
        assert sum(store.writes.values()) == 1, "nothing refunded twice"
        await again.terminate("test done")


async def test_the_policy_refuses_what_another_chat_already_put_right():
    record = {"paid": 48.24, "refunded": 6.40, "refundable": 41.84, "remediations": [{"kind": "refund", "items": ["ceres-veggie-wrap"], "amount": 6.40}]}
    assert check("refund", ORDER, record, amount=6.40, item_ids=["ceres-veggie-wrap"])[0] == "refused"
    assert check("comp", ORDER, record, item_ids=["ceres-veggie-wrap"])[0] == "refused"
    assert check("refund", ORDER, record, amount=48.24, item_ids=[line.item_id for line in ORDER.lines])[0] == "refused"
    assert check("refund", ORDER, record, amount=41.84, item_ids=[line.item_id for line in ORDER.lines])[0] == "approval"


async def test_the_chat_expires_when_the_member_goes_quiet(env: WorkflowEnvironment, store: FakeStore):
    async with worker(env.client, "sandbox-1"):
        handle = await start_support(env.client)
        result = await handle.result()
    assert result.outcome == "expired"
    assert (await memo(handle))["closed"]["outcome"] == "expired"
