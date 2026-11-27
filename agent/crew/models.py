"""Model factories for the Strands plugin, chosen by AGENT_MODEL.

The Workflow always asks for the model named "ziggy"; the Worker decides what
that is. `StrandsPlugin(models=...)` calls the factory on the Worker, outside
the Workflow, so the Workflow code is the same whichever model runs.

- bedrock: Amazon Bedrock through Strands' BedrockModel, model or inference
  profile ID from BEDROCK_MODEL_ID, credentials from boto3's default chain (on
  AgentCore, the runtime's execution role).
- stub: a scripted model for running offline. It is labelled as a stub on
  screen, and the Worker never falls back to it.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
from collections.abc import AsyncGenerator, AsyncIterable, Callable
from typing import Any

from botocore.config import Config as BotocoreConfig
from strands.models import BedrockModel, Model
from strands.models.bedrock import DEFAULT_READ_TIMEOUT
from strands.types.content import Messages
from strands.types.streaming import StreamEvent

MODEL_NAME = "ziggy"
STUB_MODEL_ID = "stub (scripted, offline)"


class ConfigError(Exception):
    """Required configuration is missing or invalid; the Worker must not start."""


def model_factory_from_env(env: dict[str, str] | os._Environ[str] = os.environ) -> Callable[[], Model]:
    """Return the factory AGENT_MODEL names, or raise ConfigError."""
    choice = env.get("AGENT_MODEL", "")
    if choice == "bedrock":
        model_id = env.get("BEDROCK_MODEL_ID", "")
        if not model_id:
            raise ConfigError("AGENT_MODEL=bedrock needs BEDROCK_MODEL_ID (a Bedrock model or inference profile ID)")

        def bedrock() -> Model:
            # Temporal retries the model Activity, so Botocore must not retry
            # inside an attempt as well.
            return BedrockModel(
                model_id=model_id,
                boto_client_config=BotocoreConfig(read_timeout=DEFAULT_READ_TIMEOUT, retries={"max_attempts": 0}),
            )

        return bedrock
    if choice == "stub":
        delay = float(env.get("AGENT_STUB_DELAY_SECONDS", "1.5"))
        return lambda: StubModel(delay_seconds=delay)
    raise ConfigError(f"AGENT_MODEL must be bedrock or stub, got {choice!r}")


class StubModel(Model):
    """A scripted stand-in for Bedrock: same tools, no network.

    It reads the conversation so far and plays the next step of a fixed
    plan: recent orders, menu, stock, rewards, then a quote built from the
    newest order with what the café is out of swapped and the named people's
    lines made vegan, then a short reply. It reasons from the tool results,
    so it builds the same cart the real model does for the demo's request.
    """

    def __init__(self, delay_seconds: float = 1.5) -> None:
        self._config: dict[str, Any] = {"model_id": STUB_MODEL_ID}
        self._delay = delay_seconds

    def update_config(self, **model_config: Any) -> None:
        self._config.update(model_config)

    def get_config(self) -> dict[str, Any]:
        return self._config

    def structured_output(self, output_model: Any, prompt: Messages, system_prompt: str | None = None, **kwargs: Any) -> AsyncGenerator[dict[str, Any], None]:
        raise NotImplementedError("the stub model has no structured output")

    async def stream(self, messages: Messages, tool_specs: Any = None, system_prompt: str | None = None, **kwargs: Any) -> AsyncIterable[StreamEvent]:
        await asyncio.sleep(self._delay)
        text, tool = next_step(messages)
        # The tool use ID must be the same on every attempt of this call.
        tool_use_id = f"stub-{len(messages)}"
        yield {"messageStart": {"role": "assistant"}}
        yield {"contentBlockDelta": {"delta": {"text": text}}}
        yield {"contentBlockStop": {}}
        if tool is not None:
            name, args = tool
            yield {"contentBlockStart": {"start": {"toolUse": {"toolUseId": tool_use_id, "name": name}}}}
            yield {"contentBlockDelta": {"delta": {"toolUse": {"input": json.dumps(args)}}}}
            yield {"contentBlockStop": {}}
        yield {"messageStop": {"stopReason": "tool_use" if tool else "end_turn"}}
        yield {"metadata": {"usage": {"inputTokens": 0, "outputTokens": 0, "totalTokens": 0}, "metrics": {"latencyMs": int(self._delay * 1000)}}}


def _text(message: dict[str, Any]) -> str:
    return "".join(c.get("text", "") for c in message.get("content", []))


def _tool_results(messages: Messages) -> dict[str, dict[str, Any]]:
    """The latest result of each tool, by tool name: status and parsed body."""
    names: dict[str, str] = {}
    results: dict[str, dict[str, Any]] = {}
    for message in messages:
        for block in message.get("content", []):
            if "toolUse" in block:
                names[block["toolUse"]["toolUseId"]] = block["toolUse"]["name"]
            if "toolResult" in block:
                result = block["toolResult"]
                text = "".join(c.get("text", "") for c in result.get("content", []))
                try:
                    body = json.loads(text)
                except ValueError:
                    body = {"error": text}
                results[names.get(result["toolUseId"], "")] = {"status": result.get("status"), "body": body}
    return results


def next_step(messages: Messages) -> tuple[str, tuple[str, dict[str, Any]] | None]:
    """The stub's answer to the conversation so far: text, and a tool call or None."""
    first = _text(messages[0])
    member = re.search(r"member_id=([a-z0-9-]+)", first)
    cafe = re.search(r"cafe_id=([a-z0-9-]+)", first)
    if not member or not cafe:
        return "I can't tell who is ordering or where, so I can't build the order.", None
    member_id, cafe_id = member.group(1), cafe.group(1)
    last = messages[-1]
    is_user_text = last.get("role") == "user" and not any("toolResult" in b for b in last.get("content", []))

    if is_user_text and len(messages) > 1:
        paid = re.search(r"Order (\S+) will be ready at (\S+) at ([^.]+)\. (\d+) Stardust", _text(last))
        if paid:
            number, ready, cafe_name, points = paid.groups()
            return (
                f"Order {number} is in. {cafe_name} has it ready at {ready}, and {points} Stardust are on your account. "
                "I'll let you know when your order is ready for pickup!"
            ), None
        pickup = re.search(r"order (\S+) is ready for pickup at ([^.]+)\.", _text(last))
        if pickup:
            number, cafe_name = pickup.groups()
            return f"Order {number} is ready for pickup at {cafe_name}. Enjoy, crew!", None
        return "I'm the offline stand-in model, so I can only build the order I was scripted for. Tap Pay, or start a new order.", None

    done = _tool_results(messages)
    if "get_recent_orders" not in done:
        return "Checking your recent orders.", ("get_recent_orders", {"member_id": member_id})
    if "get_menu" not in done:
        return "Reading today's menu.", ("get_menu", {"cafe_id": cafe_id})
    if "get_stock" not in done:
        return "Checking what the café has in stock.", ("get_stock", {"cafe_id": cafe_id})
    if "get_rewards" not in done:
        return "Looking at your rewards.", ("get_rewards", {"member_id": member_id})
    failed = [name for name, r in done.items() if r["status"] != "success" and name != "quote_cart"]
    if failed:
        return f"I couldn't read what I need from the store ({', '.join(failed)}), so I can't build the order yet.", None
    if "quote_cart" not in done:
        lines, changes = build_cart(first, done)
        return "Pricing the cart.", ("quote_cart", {"cafe_id": cafe_id, "member_id": member_id, "lines": lines, "reward_id": "best"})
    quote = done["quote_cart"]
    if quote["status"] != "success":
        return f"I couldn't price the order: {quote['body'].get('error', 'unknown problem')}.", None
    q = quote["body"]
    _, changes = build_cart(first, done)
    reward = q.get("reward")
    saving = f" {reward['name']} saves {float(reward['discount']):.2f} cr." if reward else ""
    return f"Here's your order. {' '.join(changes)}{saving} The total is {float(q['total']):.2f} cr.", None


def build_cart(request: str, done: dict[str, dict[str, Any]]) -> tuple[list[dict[str, Any]], list[str]]:
    """The newest order again, with out-of-stock options swapped and named people's lines made vegan."""
    orders = done["get_recent_orders"]["body"]["orders"]
    menu = {item["id"]: item for item in done["get_menu"]["body"]["items"]}
    option_effects = done["get_menu"]["body"]["options"]
    non_vegan = set(done["get_menu"]["body"]["nonVeganIngredients"])
    out = set(done["get_stock"]["body"]["outOfStock"])
    vegans = set(re.findall(r"\b([A-Z][a-z]+)(?:'s| is) (?:now )?vegan", request))

    def ingredients(item_id: str, options: list[str]) -> list[str]:
        ings = list(menu[item_id]["ingredients"])
        for name in options:
            o = option_effects[name]
            if o.get("replace"):
                ings = [o["with"] if i == o["replace"] else i for i in ings]
            elif o.get("remove"):
                ings = [i for i in ings if i != o["remove"]]
            elif o.get("add"):
                ings.append(o["add"])
        return ings

    lines: list[dict[str, Any]] = []
    changes: list[str] = []
    for line in orders[0]["lines"] if orders else []:
        item_id, options, note = line["itemId"], list(line.get("options", [])), line.get("note", "")
        item = menu[item_id]
        for opt in list(options):
            if any(i in out for i in ingredients(item_id, [opt])) and "milk:almond" in item["options"]:
                options[options.index(opt)] = "milk:almond"
                changes.append(f"{opt.split(':')[1].title()} milk is out, so the {item['name'].lower()} is almond.")
        if note in vegans and set(ingredients(item_id, options)) & non_vegan:
            for fix, needs in (("milk:almond", "dairy-milk"), ("no-cream", "whipped-cream"), ("no-honey", "honey")):
                if needs in ingredients(item_id, options) and fix in item["options"]:
                    options = [o for o in options if not o.startswith("milk:")] + [fix] if fix.startswith("milk:") else options + [fix]
            if set(ingredients(item_id, options)) & non_vegan:
                swap = next(
                    (m for m in menu.values() if m["category"] == item["category"] and m["veganAsServed"] and not set(m["ingredients"]) & out),
                    None,
                )
                if swap:
                    changes.append(f"{note} gets the {swap['name'].lower()} instead of the {item['name'].lower()}.")
                    item_id, options = swap["id"], []
            else:
                changes.append(f"{note}'s {item['name'].lower()} is made vegan.")
        lines.append({"item_id": item_id, "qty": line["qty"], "options": options, "note": note})
    return lines, changes
