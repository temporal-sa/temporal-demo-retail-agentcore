package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Prefix is where the store serves. On the platform the demo host routes
// /store here as a public route (no sign-in), because the agent calls it from
// AgentCore, outside the cluster; the bearer token is the store's own check.
const Prefix = "/store/v1"

// Server is the store's REST API. It keeps no state in the process: menus
// and rewards come from the skin, quotes carry their own figures, and writes
// go to the Ledger.
type Server struct {
	skin   *Skin
	signer Signer
	ledger Ledger
	token  string
	now    func() time.Time
	log    *slog.Logger
}

func NewServer(skin *Skin, ledger Ledger, token string, now func() time.Time, logger *slog.Logger) http.Handler {
	s := &Server{skin: skin, signer: NewSigner([]byte("quote-signing:" + token)), ledger: ledger, token: token, now: now, log: logger}
	return s.routes()
}

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET "+Prefix+"/cafes", s.handleCafes)
	api.HandleFunc("GET "+Prefix+"/cafes/{cafe}/menu", s.handleMenu)
	api.HandleFunc("GET "+Prefix+"/cafes/{cafe}/stock", s.handleStock)
	api.HandleFunc("GET "+Prefix+"/members/{member}/orders", s.handleOrders)
	api.HandleFunc("GET "+Prefix+"/members/{member}/rewards", s.handleRewards)
	api.HandleFunc("POST "+Prefix+"/quotes", s.handleQuote)
	api.HandleFunc("POST "+Prefix+"/payments", s.handlePayment)
	api.HandleFunc("POST "+Prefix+"/orders", s.handlePlaceOrder)
	api.HandleFunc("POST "+Prefix+"/members/{member}/stardust", s.handleStardust)
	api.HandleFunc("GET "+Prefix+"/ledger/{key}", s.handleLedger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle(Prefix+"/", s.authenticated(api))
	return mux
}

// authenticated admits requests that carry the store's bearer token.
func (s *Server) authenticated(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="store"`)
			problem(w, http.StatusUnauthorized, "Missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleCafes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"brand": s.skin.Brand, "cafes": s.skin.Cafes})
}

type menuItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Price       Money    `json:"price"`
	Ingredients []string `json:"ingredients"`
	Options     []string `json:"options"`
	Vegan       bool     `json:"veganAsServed"`
}

func (s *Server) handleMenu(w http.ResponseWriter, r *http.Request) {
	cafe := s.skin.cafe(r.PathValue("cafe"))
	if cafe == nil {
		problem(w, http.StatusNotFound, "No such cafe", fmt.Sprintf("Known cafes: %s.", s.cafeIDs()))
		return
	}
	items := make([]menuItem, 0, len(s.skin.Menu))
	for _, m := range s.skin.Menu {
		items = append(items, menuItem{ID: m.ID, Name: m.Name, Category: m.Category, Price: Money(m.cents()), Ingredients: m.Ingredients, Options: m.Options, Vegan: s.skin.vegan(m.Ingredients)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cafeId": cafe.ID, "cafeName": cafe.Name, "currency": s.skin.Brand.Currency,
		"items": items, "options": s.skin.Options, "nonVeganIngredients": s.skin.NonVegan,
	})
}

func (s *Server) handleStock(w http.ResponseWriter, r *http.Request) {
	cafe := s.skin.cafe(r.PathValue("cafe"))
	if cafe == nil {
		problem(w, http.StatusNotFound, "No such cafe", fmt.Sprintf("Known cafes: %s.", s.cafeIDs()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cafeId": cafe.ID, "cafeName": cafe.Name, "outOfStock": cafe.OutOfStock, "pickupMinutes": cafe.PickupMinutes})
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	m := s.skin.member(r.PathValue("member"))
	if m == nil {
		problem(w, http.StatusNotFound, "No such member")
		return
	}
	limit := 5
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 20 {
			problem(w, http.StatusBadRequest, "Invalid limit", "limit is 1 to 20.")
			return
		}
		limit = n
	}
	orders := m.Orders[:min(limit, len(m.Orders))]
	writeJSON(w, http.StatusOK, map[string]any{"memberId": m.ID, "memberName": m.Name, "orders": orders})
}

func (s *Server) handleRewards(w http.ResponseWriter, r *http.Request) {
	m := s.skin.member(r.PathValue("member"))
	if m == nil {
		problem(w, http.StatusNotFound, "No such member")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memberId": m.ID, "memberName": m.Name, "stardust": m.Stardust, "rewards": m.Rewards})
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	var req QuoteRequest
	if !decode(w, r, &req) {
		return
	}
	q, err := s.skin.PriceCart(req, s.now())
	var invalid *ValidationError
	if errors.As(err, &invalid) {
		problem(w, http.StatusUnprocessableEntity, "The cart can't be priced as it is", invalid.Problems...)
		return
	}
	if err == nil {
		err = s.signer.Sign(q)
	}
	if err != nil {
		s.log.Error("quote failed", "error", err)
		problem(w, http.StatusInternalServerError, "Quote failed")
		return
	}
	s.log.Info("quoted", "cafe", q.CafeID, "member", q.MemberID, "items", q.ItemCount, "total", q.Total)
	writeJSON(w, http.StatusOK, q)
}

// idempotencyKey is the caller's name for one write. The agent sends its
// Workflow ID, so a retried Activity, or a new Worker replaying the order,
// names the same write and gets the stored result back.
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,200}$`)

type paymentRequest struct {
	QuoteID string `json:"quoteId"`
}

type Payment struct {
	PaymentID string `json:"paymentId"`
	MemberID  string `json:"memberId"`
	Amount    Money  `json:"amount"`
}

func (s *Server) handlePayment(w http.ResponseWriter, r *http.Request) {
	var req paymentRequest
	key, ok := s.idempotent(w, r, &req)
	if !ok {
		return
	}
	q, ok := s.quote(w, req.QuoteID)
	if !ok {
		return
	}
	s.write(w, r, "payment", key, req, Payment{PaymentID: "ch_" + digest(key)[:12], MemberID: q.MemberID, Amount: q.Total})
}

type placeOrderRequest struct {
	QuoteID   string `json:"quoteId"`
	PaymentID string `json:"paymentId"`
}

type Order struct {
	OrderNumber string    `json:"orderNumber"`
	CafeName    string    `json:"cafeName"`
	ItemCount   int       `json:"itemCount"`
	Total       Money     `json:"total"`
	ReadyAt     time.Time `json:"readyAt"`
	PaymentID   string    `json:"paymentId"`
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req placeOrderRequest
	key, ok := s.idempotent(w, r, &req)
	if !ok {
		return
	}
	q, ok := s.quote(w, req.QuoteID)
	if !ok {
		return
	}
	if !strings.HasPrefix(req.PaymentID, "ch_") {
		problem(w, http.StatusUnprocessableEntity, "An order needs a payment", "paymentId must be a payment this store took.")
		return
	}
	number := fmt.Sprintf("%s-%04d", s.skin.Brand.OrderPrefix, 2200+binary.BigEndian.Uint32(digestBytes(key))%7800)
	s.write(w, r, "order", key, req, Order{OrderNumber: number, CafeName: q.CafeName, ItemCount: q.ItemCount, Total: q.Total, ReadyAt: q.ReadyAt, PaymentID: req.PaymentID})
}

type stardustRequest struct {
	Points      int    `json:"points"`
	OrderNumber string `json:"orderNumber"`
}

type Award struct {
	MemberID    string `json:"memberId"`
	Points      int    `json:"points"`
	OrderNumber string `json:"orderNumber"`
	Balance     int    `json:"balance"`
}

func (s *Server) handleStardust(w http.ResponseWriter, r *http.Request) {
	m := s.skin.member(r.PathValue("member"))
	if m == nil {
		problem(w, http.StatusNotFound, "No such member")
		return
	}
	var req stardustRequest
	key, ok := s.idempotent(w, r, &req)
	if !ok {
		return
	}
	if req.Points < 1 || req.Points > 100000 || req.OrderNumber == "" {
		problem(w, http.StatusUnprocessableEntity, "Invalid award", "points is 1 to 100000 and orderNumber is required.")
		return
	}
	s.write(w, r, "stardust", key, req, Award{MemberID: m.ID, Points: req.Points, OrderNumber: req.OrderNumber, Balance: m.Stardust + req.Points})
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !idempotencyKey.MatchString(key) {
		problem(w, http.StatusBadRequest, "Invalid key")
		return
	}
	entries, err := s.ledger.List(r.Context(), key)
	if err != nil {
		s.log.Error("ledger read failed", "key", key, "error", err)
		problem(w, http.StatusServiceUnavailable, "Ledger unavailable")
		return
	}
	replays := 0
	for _, e := range entries {
		replays += e.Replays
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "entries": entries, "replays": replays})
}

// idempotent reads the Idempotency-Key header and the JSON body.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, body any) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		problem(w, http.StatusBadRequest, "Idempotency-Key required", "Send an Idempotency-Key header of 8 to 200 letters, digits, '.', '_', ':' or '-'.")
		return "", false
	}
	return key, decode(w, r, body)
}

func (s *Server) quote(w http.ResponseWriter, id string) (*Quote, bool) {
	q, err := s.signer.Verify(id, s.now())
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "Invalid quote", err.Error())
		return nil, false
	}
	return q, true
}

// write records one write under its key, or returns the one already recorded.
func (s *Server) write(w http.ResponseWriter, r *http.Request, kind, key string, req, result any) {
	reqJSON, _ := json.Marshal(req)
	resJSON, err := json.Marshal(result)
	if err != nil {
		problem(w, http.StatusInternalServerError, "Write failed")
		return
	}
	stored, created, err := s.ledger.Record(r.Context(), Entry{Kind: kind, Key: key, RequestHash: digest(string(reqJSON)), Result: resJSON, CreatedAt: s.now()})
	if err != nil {
		s.log.Error("ledger write failed", "kind", kind, "key", key, "error", err)
		problem(w, http.StatusServiceUnavailable, "Ledger unavailable", "Retry with the same Idempotency-Key.")
		return
	}
	if !created && stored.RequestHash != digest(string(reqJSON)) {
		problem(w, http.StatusUnprocessableEntity, "Idempotency-Key reused", "This key already names a different "+kind+".")
		return
	}
	status := http.StatusCreated
	if !created {
		// The same request again: answer with what the first one did.
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	s.log.Info("write", "kind", kind, "key", key, "replayed", !created, "replays", stored.Replays)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(stored.Result)
}

func (s *Server) cafeIDs() string {
	ids := make([]string, 0, len(s.skin.Cafes))
	for _, c := range s.skin.Cafes {
		ids = append(ids, c.ID)
	}
	return strings.Join(ids, ", ")
}

func digestBytes(v string) []byte { sum := sha256.Sum256([]byte(v)); return sum[:] }

func digest(v string) string { return hex.EncodeToString(digestBytes(v)) }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		problem(w, http.StatusBadRequest, "Invalid JSON body", err.Error())
		return false
	}
	return true
}

// problem writes an RFC 9457 problem details response, listing every problem.
func problem(w http.ResponseWriter, status int, title string, details ...string) {
	body := map[string]any{"type": "about:blank", "title": title, "status": status}
	if len(details) == 1 {
		body["detail"] = details[0]
	} else if len(details) > 1 {
		body["errors"] = details
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
