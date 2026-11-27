package crew

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"golang.org/x/sync/errgroup"
)

const (
	workflowType  = "CrewOrderWorkflow"
	signalPay     = "pay"
	signalMessage = "message"
	signalReady   = "kitchen_ready"
)

// Server holds the API's dependencies. NewServer returns its handler.
type Server struct {
	cfg       Config
	namespace string
	tc        client.Client
	store     *StoreClient
	log       *slog.Logger
}

// NewServer returns the API as one handler. Every route is in routes(). The
// web component (nginx) serves the page and proxies /api and /healthz here.
func NewServer(cfg Config, namespace string, tc client.Client, store *StoreClient, logger *slog.Logger) http.Handler {
	s := &Server{cfg: cfg, namespace: namespace, tc: tc, store: store, log: logger}
	return withSession(s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/view", s.handleView)
	mux.HandleFunc("POST /api/orders", s.handleStart)
	mux.HandleFunc("POST /api/orders/{id}/pay", s.handlePay)
	mux.HandleFunc("POST /api/orders/{id}/messages", s.handleMessage)
	mux.HandleFunc("POST /api/orders/{id}/ready", s.handleReady)
	return mux
}

// handleHealth is the smoke check: the API is up and Temporal answers.
// It changes nothing and wakes no Worker.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.tc.CheckHealth(ctx, &client.CheckHealthRequest{}); err != nil {
		s.problem(w, http.StatusServiceUnavailable, "Temporal is not reachable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

// handleView is the one read the UI polls. Its calls run together and none
// of them needs a Worker: Describe (memo and pending tasks), the Event
// History, the Worker list, and the store's ledger for this order. None is a
// billable Action, and none wakes the AgentCore Worker.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	orderID := r.URL.Query().Get("order")
	if orderID != "" && !orderPattern.MatchString(orderID) {
		s.problem(w, http.StatusBadRequest, "Invalid order ID", "An order ID is 8 lowercase hex characters.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	view := View{ServerTime: time.Now().UTC(), WorkerLocation: s.cfg.WorkerLocation}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		view.Worker = s.workerStatus(gctx)
		return nil
	})
	g.Go(func() error {
		// The phone's header. Without the store it shows placeholders.
		if p, err := s.store.Profile(gctx, s.cfg.MemberID, s.cfg.CafeID); err == nil {
			view.Profile = p
		} else {
			s.log.Warn("store profile unavailable", "error", err)
		}
		return nil
	})
	if orderID == "" {
		_ = g.Wait()
		writeJSON(w, http.StatusOK, view)
		return
	}
	wid := s.workflowID(ctx, orderID)
	var desc *describeResult
	var proof *Proof
	var ledger *Ledger
	g.Go(func() (err error) {
		desc, err = s.describe(gctx, wid)
		return err
	})
	g.Go(func() (err error) {
		proof, err = s.proof(gctx, wid)
		return err
	})
	g.Go(func() error {
		var err error
		if ledger, err = s.store.Ledger(gctx, wid); err != nil {
			s.log.Warn("store ledger unavailable", "workflow_id", wid, "error", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		s.temporalError(w, err)
		return
	}
	view.Order = s.orderView(orderID, wid, desc, proof, ledger)
	// A Worker that ran a task for this order within the heartbeat window is
	// running, even if its first heartbeat (every 10 s) hasn't arrived yet.
	if !view.Worker.Running && proof.LastWorker != "" && view.ServerTime.Sub(proof.LastWorkerAt) < heartbeatFresh {
		view.Worker = WorkerStatus{Known: true, Running: true, Identity: proof.LastWorker}
	}
	writeJSON(w, http.StatusOK, view)
}

type startRequest struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// handleStart starts an order. The browser chooses the ID, so it can show
// the order before this returns and retry safely: a repeat of the same ID
// gets 409, which the browser treats as its own earlier start.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if !s.decode(w, r, &req) {
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	var problems []string
	if !orderPattern.MatchString(req.ID) {
		problems = append(problems, "id must be 8 lowercase hex characters")
	}
	if req.Text == "" || len(req.Text) > 500 {
		problems = append(problems, "text must be 1 to 500 characters")
	}
	if len(problems) > 0 {
		s.problem(w, http.StatusBadRequest, "Invalid order", problems...)
		return
	}

	wid := s.workflowID(r.Context(), req.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Starting records the Workflow and its first task with the server. No
	// Worker needs to be running: on AgentCore, this task is what makes
	// Temporal Cloud invoke one.
	now := time.Now().UTC()
	first := []any{map[string]any{"from": "member", "text": req.Text, "at": now}}
	run, err := s.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       wid,
		TaskQueue:                s.cfg.TaskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		// Set at start, the memo costs no extra Action, and the phone shows
		// the member's message before a Worker has run.
		Memo: map[string]any{"phase": "planning", "messages": first},
	}, workflowType, map[string]string{"member_id": s.cfg.MemberID, "cafe_id": s.cfg.CafeID, "text": req.Text})
	if err != nil {
		var started *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &started) {
			s.problem(w, http.StatusConflict, "Order already started", "An order with this ID exists in this session.")
			return
		}
		s.temporalError(w, err)
		return
	}
	s.log.Info("order started", "workflow_id", wid)
	writeJSON(w, http.StatusCreated, View{
		ServerTime:     now,
		WorkerLocation: s.cfg.WorkerLocation,
		Order: &OrderView{
			ID: req.ID, WorkflowID: wid, RunID: run.GetRunID(),
			Status: "Running", Phase: "planning", Messages: first, Work: []any{}, StartedAt: &now,
			Waiting:     &Waiting{Since: now, Task: "Workflow Task", Attempt: 1},
			TemporalURL: s.temporalURL(wid),
			Proof:       &Proof{Rows: []ProofRow{}, Workers: []string{}},
		},
	})
}

type payRequest struct {
	QuoteID string `json:"quoteId"`
}

// handlePay sends the member's payment as a Signal. The server records it at
// once, with no Worker running; recording it is what makes Temporal start
// one. An Update would hold this request open until that Worker had
// cold-started.
func (s *Server) handlePay(w http.ResponseWriter, r *http.Request) {
	var req payRequest
	if !s.orderSignal(w, r, &req) {
		return
	}
	if req.QuoteID == "" || len(req.QuoteID) > 8192 {
		s.problem(w, http.StatusBadRequest, "Invalid payment", "quoteId is the cart's quote ID.")
		return
	}
	s.signal(w, r, signalPay, map[string]any{"quote_id": req.QuoteID})
}

type messageRequest struct {
	Text string `json:"text"`
}

// handleMessage sends a change request ("add a donut") as a Signal.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req messageRequest
	if !s.orderSignal(w, r, &req) {
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || len(req.Text) > 500 {
		s.problem(w, http.StatusBadRequest, "Invalid message", "text must be 1 to 500 characters.")
		return
	}
	s.signal(w, r, signalMessage, map[string]any{"text": req.Text})
}

// handleReady is the kitchen marking a paid order ready for pickup, as a
// Signal for the same reason as handlePay: no Worker needs to be running.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if !orderPattern.MatchString(r.PathValue("id")) {
		s.problem(w, http.StatusBadRequest, "Invalid order ID", "An order ID is 8 lowercase hex characters.")
		return
	}
	s.signal(w, r, signalReady, map[string]any{})
}

func (s *Server) orderSignal(w http.ResponseWriter, r *http.Request, body any) bool {
	if !orderPattern.MatchString(r.PathValue("id")) {
		s.problem(w, http.StatusBadRequest, "Invalid order ID", "An order ID is 8 lowercase hex characters.")
		return false
	}
	return s.decode(w, r, body)
}

func (s *Server) signal(w http.ResponseWriter, r *http.Request, name string, arg any) {
	wid := s.workflowID(r.Context(), r.PathValue("id"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.tc.SignalWorkflow(ctx, wid, "", name, arg); err != nil {
		s.temporalError(w, err)
		return
	}
	s.log.Info("signal sent", "workflow_id", wid, "signal", name)
	writeJSON(w, http.StatusAccepted, map[string]any{"signal": name, "sentAt": time.Now().UTC()})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.problem(w, http.StatusBadRequest, "Invalid JSON body", err.Error())
		return false
	}
	return true
}

// temporalError maps a Temporal error to a response without leaking its text
// for anything but the cases the UI handles.
func (s *Server) temporalError(w http.ResponseWriter, err error) {
	var notFound *serviceerror.NotFound
	switch {
	case errors.As(err, &notFound):
		s.problem(w, http.StatusNotFound, "No such order", "This session has no order with that ID.")
	case errors.Is(err, context.DeadlineExceeded):
		s.problem(w, http.StatusGatewayTimeout, "Temporal did not answer in time", "Try again in a moment.")
	default:
		if errors.As(err, new(*serviceerror.Unavailable)) {
			s.problem(w, http.StatusServiceUnavailable, "Temporal is not reachable", "The API will reconnect on its own.")
			return
		}
		s.log.Error("temporal call failed", "error", err)
		s.problem(w, http.StatusBadGateway, "Temporal call failed", "See the API log for details.")
	}
}

// problem writes an RFC 9457 problem details response.
func (s *Server) problem(w http.ResponseWriter, status int, title string, details ...string) {
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
