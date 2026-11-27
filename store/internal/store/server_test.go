package store

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-0123456789abcdef"

var testNow = time.Date(2026, 11, 26, 12, 14, 0, 0, time.UTC)

func newTestServer(t *testing.T) (*httptest.Server, *MemoryLedger) {
	t.Helper()
	skin, err := LoadSkin()
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewMemoryLedger()
	srv := httptest.NewServer(NewServer(skin, ledger, testToken, func() time.Time { return testNow }, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv, ledger
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any, headers ...string) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+testToken)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// The crew order the demo tells: last Friday's order with oat milk swapped
// for almond, and Mo's wrap and mocha made vegan.
var crewCart = QuoteRequest{
	CafeID: "titan-ring", MemberID: "zora-vex", RewardID: "best",
	Lines: []Line{
		{ItemID: "comet-cold-brew", Qty: 2},
		{ItemID: "nebula-latte", Qty: 1, Options: []string{"milk:almond"}},
		{ItemID: "supernova-mocha", Qty: 1, Options: []string{"milk:almond", "no-cream"}, Note: "Mo"},
		{ItemID: "ceres-veggie-wrap", Qty: 1, Note: "Mo"},
		{ItemID: "egg-stardust-wrap", Qty: 2},
		{ItemID: "asteroid-bagel", Qty: 2},
		{ItemID: "zero-g-donut", Qty: 3},
	},
}

func TestQuotePicksTheBestReward(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, q := call(t, srv, "POST", Prefix+"/quotes", crewCart)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, q)
	}
	reward := q["reward"].(map[string]any)
	if q["subtotal"] != 56.75 || reward["id"] != "crew-pack" || reward["discount"] != 8.51 || q["total"] != 48.24 || q["earns"] != 482.0 {
		t.Fatalf("unexpected quote: subtotal=%v reward=%v total=%v earns=%v", q["subtotal"], reward, q["total"], q["earns"])
	}
	mocha := q["lines"].([]any)[2].(map[string]any)
	if mocha["vegan"] != true {
		t.Fatalf("almond, no-cream mocha should be vegan: %v", mocha)
	}
}

func TestQuoteRefusesWhatTheCafeIsOutOf(t *testing.T) {
	srv, _ := newTestServer(t)
	cart := crewCart
	cart.Lines = []Line{{ItemID: "nebula-latte", Qty: 1, Options: []string{"milk:oat"}}, {ItemID: "egg-stardust-wrap", Qty: 1, Options: []string{"no-cream"}}}
	resp, body := call(t, srv, "POST", Prefix+"/quotes", cart)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", resp.StatusCode)
	}
	errs := body["errors"].([]any)
	if len(errs) != 2 || !strings.Contains(errs[0].(string), "out of oat-milk") || !strings.Contains(errs[1].(string), "does not offer option") {
		t.Fatalf("want both problems listed, got %v", errs)
	}
}

func TestPaymentIsTakenOncePerKey(t *testing.T) {
	srv, ledger := newTestServer(t)
	_, q := call(t, srv, "POST", Prefix+"/quotes", crewCart)
	pay := map[string]any{"quoteId": q["quoteId"]}

	first, p1 := call(t, srv, "POST", Prefix+"/payments", pay, "Idempotency-Key", "crew-order-abc-01")
	again, p2 := call(t, srv, "POST", Prefix+"/payments", pay, "Idempotency-Key", "crew-order-abc-01")
	if first.StatusCode != 201 || again.StatusCode != 200 || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("statuses %d then %d", first.StatusCode, again.StatusCode)
	}
	if p1["paymentId"] != p2["paymentId"] || p1["amount"] != 48.24 {
		t.Fatalf("replay returned a different payment: %v vs %v", p1, p2)
	}
	entries, _ := ledger.List(t.Context(), "crew-order-abc-01")
	if len(entries) != 1 || entries[0].Replays != 1 {
		t.Fatalf("want one payment with one replay, got %+v", entries)
	}

	// The same key for a different cart is a caller bug, not a replay.
	cart := crewCart
	cart.Lines = crewCart.Lines[:1]
	cart.RewardID = ""
	_, other := call(t, srv, "POST", Prefix+"/quotes", cart)
	resp, _ := call(t, srv, "POST", Prefix+"/payments", map[string]any{"quoteId": other["quoteId"]}, "Idempotency-Key", "crew-order-abc-01")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse: status %d", resp.StatusCode)
	}
}

func TestRefusesForgedQuotesAndMissingAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, _ := call(t, srv, "POST", Prefix+"/payments", map[string]any{"quoteId": "q1.e30.AAAA"}, "Idempotency-Key", "crew-order-abc-02")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("forged quote: status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+Prefix+"/cafes", nil)
	anon, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d", anon.StatusCode)
	}
}

func TestRefundIsCappedAtThePaymentAndTakenOnce(t *testing.T) {
	srv, ledger := newTestServer(t)
	_, q := call(t, srv, "POST", Prefix+"/quotes", crewCart)
	_, paid := call(t, srv, "POST", Prefix+"/payments", map[string]any{"quoteId": q["quoteId"]}, "Idempotency-Key", "crew-order-abc-03")
	refund := func(amount float64) (*http.Response, map[string]any) {
		return call(t, srv, "POST", Prefix+"/refunds", map[string]any{"orderKey": "crew-order-abc-03", "paymentId": paid["paymentId"], "amount": amount, "reason": "missing wrap"}, "Idempotency-Key", "crew-support-abc-03")
	}
	if resp, _ := refund(48.25); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("more than paid: status %d", resp.StatusCode)
	}
	first, r1 := refund(6.4)
	again, _ := refund(6.4)
	if first.StatusCode != 201 || again.StatusCode != 200 || r1["amount"] != 6.40 {
		t.Fatalf("statuses %d then %d, refund %v", first.StatusCode, again.StatusCode, r1)
	}
	entries, _ := ledger.List(t.Context(), "crew-support-abc-03")
	if len(entries) != 1 || entries[0].Kind != "refund" || entries[0].Replays != 1 {
		t.Fatalf("want one refund with one replay, got %+v", entries)
	}
}

func TestCompIsPricedAndChargesNothing(t *testing.T) {
	srv, _ := newTestServer(t)
	comp := map[string]any{"cafeId": "titan-ring", "memberId": "zora-vex", "lines": []Line{{ItemID: "ceres-veggie-wrap", Qty: 1, Note: "Mo"}}, "forOrder": "OG-4242", "orderKey": "crew-order-abc-04", "reason": "missing wrap"}
	resp, c := call(t, srv, "POST", Prefix+"/comps", comp, "Idempotency-Key", "crew-support-abc-04")
	if resp.StatusCode != 201 || c["value"] != 6.40 || c["total"] != 0.0 || c["forOrder"] != "OG-4242" {
		t.Fatalf("status %d, comp %v", resp.StatusCode, c)
	}
	comp["lines"] = []Line{{ItemID: "nebula-latte", Qty: 1, Options: []string{"milk:oat"}}}
	if resp, _ := call(t, srv, "POST", Prefix+"/comps", comp, "Idempotency-Key", "crew-support-abc-05"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("out of stock comp: status %d", resp.StatusCode)
	}
}

func TestAnOrdersRefundsFromEveryChatStayWithinThePayment(t *testing.T) {
	srv, _ := newTestServer(t)
	_, q := call(t, srv, "POST", Prefix+"/quotes", crewCart)
	_, paid := call(t, srv, "POST", Prefix+"/payments", map[string]any{"quoteId": q["quoteId"]}, "Idempotency-Key", "crew-order-abc-06")
	refund := func(chat string, amount float64) *http.Response {
		resp, _ := call(t, srv, "POST", Prefix+"/refunds", map[string]any{"orderKey": "crew-order-abc-06", "paymentId": paid["paymentId"], "amount": amount, "items": []string{"ceres-veggie-wrap"}, "reason": "missing wrap"}, "Idempotency-Key", chat)
		return resp
	}
	if resp := refund("crew-order-abc-06-support-1", 6.40); resp.StatusCode != 201 {
		t.Fatalf("first refund: status %d", resp.StatusCode)
	}
	total := paid["amount"].(float64)
	if resp := refund("crew-order-abc-06-support-2", total); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("the full payment again from a second chat: status %d", resp.StatusCode)
	}
	if resp := refund("crew-order-abc-06-support-1", 6.40); resp.StatusCode != 200 {
		t.Fatalf("a retry of the first: status %d", resp.StatusCode)
	}
	_, rec := call(t, srv, "GET", Prefix+"/orders/crew-order-abc-06/support", nil)
	rems := rec["remediations"].([]any)
	if rec["refunded"] != 6.40 || len(rems) != 1 || rems[0].(map[string]any)["items"].([]any)[0] != "ceres-veggie-wrap" {
		t.Fatalf("record %v", rec)
	}
	if rec["refundable"].(float64) != float64(int(total*100)-640)/100 {
		t.Fatalf("refundable %v of %v", rec["refundable"], total)
	}
}
