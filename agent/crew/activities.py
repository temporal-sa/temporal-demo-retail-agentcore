"""The agent's Activities: every call to the store API is one.

Three kinds:

- TOOLS run the agent's tools. The tools themselves are in the Workflow
  (workflows.py), bound to the order's member and café: the model decides
  when to call one, and the Workflow fills in who and where.
- CHECKOUT runs after the member pays, called by the Workflow itself, never
  by the model. Each sends the Workflow ID as its Idempotency-Key, so a
  retried attempt gets the store's stored result instead of a second charge.
- SUPPORT runs from the support chat's tools, after the café's policy (and
  a person, when the policy says so) allows it, keyed by the chat's ID.

The store is a stateless REST API (store/ in this repo) standing in for a
café chain's real systems. On AgentCore it is reached over the public
internet, so every request carries the store's bearer token.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any

import httpx
from temporalio import activity
from temporalio.exceptions import ApplicationError


@dataclass
class CartLine:
    """One line of a cart."""

    item_id: str
    qty: int
    options: list[str] = field(default_factory=list)
    note: str = ""


class StoreClient:
    """One HTTP client per Worker process, created on first use."""

    def __init__(self) -> None:
        self._client: httpx.AsyncClient | None = None

    def get(self) -> httpx.AsyncClient:
        if self._client is None:
            base = os.environ.get("STORE_API_URL", "").rstrip("/")
            token = os.environ.get("STORE_API_TOKEN", "")
            if not base or not token:
                raise ApplicationError("STORE_API_URL and STORE_API_TOKEN must be set", type="StoreNotConfigured", non_retryable=True)
            # Bounded below the Activities' start-to-close timeouts, so a slow
            # store fails an attempt and Temporal retries it.
            self._client = httpx.AsyncClient(
                base_url=base,
                headers={"Authorization": f"Bearer {token}", "User-Agent": "crew-order-agent"},
                timeout=httpx.Timeout(8.0, connect=3.0),
            )
        return self._client


store = StoreClient()


async def _call(method: str, path: str, *, json: Any = None, params: dict[str, Any] | None = None, idempotency_key: str | None = None) -> tuple[dict[str, Any], bool]:
    """Send one request. Returns the body and whether the store replayed a stored write."""
    headers = {"Idempotency-Key": idempotency_key} if idempotency_key else {}
    try:
        resp = await store.get().request(method, path, json=json, params=params, headers=headers)
    except httpx.TransportError as err:
        # Raised as is: Temporal retries the Activity.
        raise RuntimeError(f"store unreachable: {err}") from err
    if resp.status_code >= 500 or resp.status_code == 429:
        raise RuntimeError(f"store answered {resp.status_code}; retrying")
    body = resp.json() if resp.content else {}
    if resp.status_code >= 400:
        # The request itself is wrong: retrying sends the same thing. The
        # message goes back to the model as the tool's result, so it can fix
        # the cart and try again.
        problems = body.get("errors") or [body.get("detail", "")]
        message = f"{body.get('title', resp.reason_phrase)}: " + "; ".join(p for p in problems if p)
        raise ApplicationError(message, type=f"Store{resp.status_code}", non_retryable=True)
    return body, resp.headers.get("Idempotent-Replayed") == "true"


def _money(v: Any) -> str:
    return f"{float(v):.2f}"


@activity.defn
async def get_recent_orders(member_id: str) -> dict[str, Any]:
    """The member's five most recent orders, newest first."""
    body, _ = await _call("GET", f"/members/{member_id}/orders", params={"limit": 5})
    newest = body["orders"][0] if body["orders"] else None
    items = sum(line["qty"] for line in newest["lines"]) if newest else 0
    body["summary"] = f"{len(body['orders'])} recent orders; newest {newest['number']} on {newest['placedAt'][:10]}, {items} items" if newest else "no orders yet"
    return body


@activity.defn
async def get_menu(cafe_id: str) -> dict[str, Any]:
    """A café's menu, with each item's ingredients and options."""
    body, _ = await _call("GET", f"/cafes/{cafe_id}/menu")
    body["summary"] = f"{body['cafeName']} menu: {len(body['items'])} items"
    return body


@activity.defn
async def get_stock(cafe_id: str) -> dict[str, Any]:
    """What a café is out of today, and its pickup time."""
    body, _ = await _call("GET", f"/cafes/{cafe_id}/stock")
    out = ", ".join(body["outOfStock"]) or "nothing"
    body["summary"] = f"{body['cafeName']} is out of {out} today"
    return body


@activity.defn
async def get_rewards(member_id: str) -> dict[str, Any]:
    """The member's Stardust balance and rewards."""
    body, _ = await _call("GET", f"/members/{member_id}/rewards")
    body["summary"] = f"{len(body['rewards'])} rewards, {body['stardust']} Stardust"
    return body


@activity.defn
async def quote_cart(cafe_id: str, member_id: str, lines: list[CartLine], reward_id: str) -> dict[str, Any]:
    """Price a cart for a member at a café, with a reward ID, "best", or "" for none. The store refuses, listing every problem, a cart it can't make."""
    payload = {
        "cafeId": cafe_id,
        "memberId": member_id,
        "rewardId": reward_id,
        "lines": [{"itemId": line.item_id, "qty": line.qty, "options": line.options, "note": line.note} for line in lines],
    }
    body, _ = await _call("POST", "/quotes", json=payload)
    reward = body.get("reward")
    saving = f", {reward['name']} saves {_money(reward['discount'])}" if reward else ""
    body["summary"] = f"{body['itemCount']} items, {_money(body['subtotal'])}{saving}, total {_money(body['total'])}"
    return body


TOOLS = [get_recent_orders, get_menu, get_stock, get_rewards, quote_cart]


@activity.defn
async def charge_payment(order_key: str, quote_id: str) -> dict[str, Any]:
    """Charge the member's saved card for a quote, once per order key."""
    body, replayed = await _call("POST", "/payments", json={"quoteId": quote_id}, idempotency_key=order_key)
    activity.logger.info("payment payment_id=%s amount=%s replayed=%s", body["paymentId"], body["amount"], replayed)
    return {**body, "replayed": replayed}


@activity.defn
async def place_order(order_key: str, quote_id: str, payment_id: str) -> dict[str, Any]:
    """Send the paid order to the café, once per order key."""
    body, replayed = await _call("POST", "/orders", json={"quoteId": quote_id, "paymentId": payment_id}, idempotency_key=order_key)
    activity.logger.info("order placed order_number=%s replayed=%s", body["orderNumber"], replayed)
    return {**body, "replayed": replayed}


@activity.defn
async def award_stardust(order_key: str, member_id: str, points: int, order_number: str, for_order_key: str = "", reason: str = "") -> dict[str, Any]:
    """Add Stardust to the member's balance, once per key.

    Checkout passes the order's key. Support passes the chat's key, and the
    order's as for_order_key, so the store files it in the order's record.
    """
    payload: dict[str, Any] = {"points": points, "orderNumber": order_number}
    if for_order_key:
        payload |= {"orderKey": for_order_key, "reason": reason}
    body, replayed = await _call("POST", f"/members/{member_id}/stardust", json=payload, idempotency_key=order_key)
    activity.logger.info("stardust awarded points=%s replayed=%s", body["points"], replayed)
    return {**body, "replayed": replayed}


CHECKOUT = [charge_payment, place_order, award_stardust]


@activity.defn
async def look_up_order(order_key: str) -> dict[str, Any]:
    """The order's support record: what was paid, and every refund, comp and award support gave, from any chat."""
    body, _ = await _call("GET", f"/orders/{order_key}/support")
    activity.logger.info("order record refunded=%s remediations=%d", body["refunded"], len(body["remediations"]))
    return body


@activity.defn
async def issue_refund(support_key: str, order_key: str, payment_id: str, amount: float, reason: str, items: list[str] | None = None) -> dict[str, Any]:
    """Refund part of an order's payment to the card, once per support chat.

    The store refuses a refund that would take the order's refunds, from
    every chat, past what was paid.
    """
    payload = {"orderKey": order_key, "paymentId": payment_id, "amount": amount, "items": items or [], "reason": reason}
    body, replayed = await _call("POST", "/refunds", json=payload, idempotency_key=support_key)
    activity.logger.info("refund refund_id=%s amount=%s replayed=%s", body["refundId"], body["amount"], replayed)
    return {**body, "replayed": replayed}


@activity.defn
async def place_comp(support_key: str, cafe_id: str, member_id: str, lines: list[CartLine], for_order: str, reason: str, order_key: str = "") -> dict[str, Any]:
    """Place an order at no charge, once per support chat."""
    payload = {
        "cafeId": cafe_id,
        "memberId": member_id,
        "lines": [{"itemId": line.item_id, "qty": line.qty, "options": line.options, "note": line.note} for line in lines],
        "forOrder": for_order,
        "orderKey": order_key,
        "reason": reason,
    }
    body, replayed = await _call("POST", "/comps", json=payload, idempotency_key=support_key)
    activity.logger.info("comp order_number=%s value=%s replayed=%s", body["orderNumber"], body["value"], replayed)
    return {**body, "replayed": replayed}


# Run by the support chat's tools, inside the Workflow, after its policy check.
# They also use award_stardust, with the chat's key.
SUPPORT = [look_up_order, issue_refund, place_comp]
