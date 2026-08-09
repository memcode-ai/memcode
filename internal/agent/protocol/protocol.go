// Package protocol drives an interactive memcode session over the stream-json
// control protocol (newline-delimited JSON on stdio) — the machine-facing twin of
// the TUI. It binds the SAME runtime seams the TUI uses (SetOutput / SetApprover /
// SetAsker / SetObserver) to a JSON transport, so the agent loop is untouched. This
// is what the sdk/agent wrapper speaks to a `memcode agent --protocol stream-json`
// subprocess.
//
// stdout carries machine events ONLY (one wire.Envelope per line); diagnostics go
// to stderr. A turn-runner goroutine executes turns so the stdin reader stays free to
// service permission/ask responses and cancels mid-turn.
package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/wire"
)

type driver struct {
	sess *runtime.Session

	mu  sync.Mutex // serializes stdout writes — one Envelope per line
	out *json.Encoder

	turnSeq int
	turnID  string // id of the in-flight turn (for correlation)

	perm chan permReply // approval answers from the client (carrying the request's correlation id)
	ask  chan askReply  // ask_user answers from the client (carrying the request's correlation id)

	idMu  sync.Mutex
	idSeq int

	cancelMu sync.Mutex
	cancel   context.CancelFunc // cancels the in-flight turn; nil when idle
}

// permReply / askReply pair a control response with the request id it answers, so the
// blocking handler ignores a stale/mismatched reply instead of resolving on the wrong one.
type permReply struct {
	id   string
	data wire.PermissionResponseData
}
type askReply struct {
	id   string
	data wire.AskResponseData
}

func (d *driver) nextID() string {
	d.idMu.Lock()
	defer d.idMu.Unlock()
	d.idSeq++
	return "r" + strconv.Itoa(d.idSeq)
}

// Run drives one stream-json session over (in, out) until in closes (or ctx ends).
func Run(ctx context.Context, sess *runtime.Session, in io.Reader, out io.Writer) error {
	d := &driver{
		sess: sess,
		out:  json.NewEncoder(out),
		perm: make(chan permReply, 1),
		ask:  make(chan askReply, 1),
	}

	// Bind the runtime seams to the protocol (the same hooks the TUI uses).
	sess.SetOutput(writerFunc(func(p []byte) (int, error) {
		d.emit("", wire.MsgAssistantDelta, wire.AssistantDeltaData{Text: string(p)})
		return len(p), nil
	}))
	sess.SetApprover(d.approve)
	sess.SetAsker(d.askUser)
	sess.SetObserver(d)

	d.emit("", wire.MsgInitialized, wire.InitializedData{
		SessionID: sess.SessionID(), Protocol: wire.StreamJSONVersion,
	})

	st := sess.StartChat(ctx)
	defer sess.EndChat(ctx)

	// turn-runner: executes user turns sequentially so a long turn never blocks the
	// stdin reader (which must keep servicing permission/ask responses + cancels).
	turns := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for text := range turns {
			d.runTurn(ctx, st, text)
		}
	}()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var env wire.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			continue // skip a malformed line rather than killing the session
		}
		switch env.Type {
		case wire.MsgInitialize:
			var in wire.InitializeData
			_ = json.Unmarshal(env.Data, &in)
			switch permissions.Mode(in.Mode) {
			case permissions.ModeAsk, permissions.ModeAuto, permissions.ModeAllowAll:
				sess.SetMode(permissions.Mode(in.Mode))
			}
			// A model pin rides the handshake (headless clients have no /model
			// picker). The window comes from the SDK catalog — the same source the
			// picker's list is built from. An unknown label is skipped, not sent:
			// the gateway would silently serve Automatic anyway, so surface it on
			// stderr (the diagnostic lane) instead of pinning a lie.
			if in.Pin != "" {
				if m, ok := catalog.LookupModel(in.Pin); ok {
					sess.SetPin(in.Pin, m.Window)
				} else {
					fmt.Fprintf(os.Stderr, "protocol: unknown model pin %q — staying on Automatic\n", in.Pin)
				}
			}
		case wire.MsgUserTurn:
			var u wire.UserTurnData
			if json.Unmarshal(env.Data, &u) == nil && u.Text != "" {
				turns <- u.Text
			}
		case wire.MsgPermissionResponse:
			var r wire.PermissionResponseData
			if json.Unmarshal(env.Data, &r) == nil {
				select {
				case d.perm <- permReply{id: env.ID, data: r}:
				default: // no approval pending — drop
				}
			}
		case wire.MsgAskResponse:
			var a wire.AskResponseData
			if json.Unmarshal(env.Data, &a) == nil {
				select {
				case d.ask <- askReply{id: env.ID, data: a}:
				default:
				}
			}
		case wire.MsgCancel:
			d.cancelTurn()
		}
	}
	close(turns)
	<-done
	return sc.Err()
}

// runTurn executes one turn under a cancelable context (so MsgCancel can interrupt it)
// and emits a result/error envelope when it finishes.
func (d *driver) runTurn(ctx context.Context, st *runtime.ChatState, text string) {
	turnCtx, cancel := context.WithCancel(ctx)
	d.cancelMu.Lock()
	d.turnSeq++
	d.turnID = "t" + strconv.Itoa(d.turnSeq)
	d.cancel = cancel
	d.cancelMu.Unlock()

	d.sess.Submit(turnCtx, st, text)

	// Capture completion BEFORE our own cancel(): otherwise turnCtx.Err() always reads
	// context.Canceled and every result reports Completed:false even on success. The turn
	// completed iff it was not interrupted (by MsgCancel or a parent-ctx cancel) mid-run.
	completed := turnCtx.Err() == nil

	d.cancelMu.Lock()
	cancel()
	d.cancel = nil
	tid := d.turnID
	d.cancelMu.Unlock()

	d.emit(tid, wire.MsgResult, wire.ResultData{Text: d.sess.LastText(), Completed: completed})
}

func (d *driver) cancelTurn() {
	d.cancelMu.Lock()
	c := d.cancel
	d.cancelMu.Unlock()
	if c != nil {
		c()
	}
}

// approve emits a permission_request and blocks until the client answers (matched by
// the turn-blocking nature of the agent loop — one approval pending at a time) or the
// turn is cancelled.
func (d *driver) approve(ctx context.Context, req runtime.ApprovalRequest) runtime.ApprovalDecision {
	drain(d.perm)
	id := d.nextID()
	d.emitID(d.currentTurn(), id, wire.MsgPermissionRequest, wire.PermissionRequestData{
		Title: req.Title, Label: req.Label, Detail: req.Detail,
		Command: req.Command, Cwd: req.Cwd, Risk: req.Risk, Editable: req.Editable,
	})
	for {
		select {
		case r := <-d.perm:
			if r.id != "" && r.id != id {
				continue // a stale/mismatched response — keep awaiting this request's
			}
			return runtime.ApprovalDecision{Allow: r.data.Allow, Command: r.data.Command, Reason: r.data.Reason, Interrupt: r.data.Interrupt}
		case <-ctx.Done():
			return runtime.ApprovalDecision{Interrupt: true}
		}
	}
}

func (d *driver) askUser(ctx context.Context, req runtime.AskRequest) runtime.AskResponse {
	drain(d.ask)
	id := d.nextID()
	// The stream-json protocol carries plain option labels (descriptions are a TUI/stdin
	// affordance); map to labels at this boundary so that protocol contract is unchanged.
	labels := make([]string, len(req.Options))
	for i, o := range req.Options {
		labels[i] = o.Label
	}
	d.emitID(d.currentTurn(), id, wire.MsgAskRequest, wire.AskRequestData{Question: req.Question, Options: labels})
	for {
		select {
		case a := <-d.ask:
			if a.id != "" && a.id != id {
				continue
			}
			return runtime.AskResponse{Answer: a.data.Answer}
		case <-ctx.Done():
			return runtime.AskResponse{}
		}
	}
}

// drain clears any buffered (stale) reply so a handler awaits THIS request's response.
func drain[T any](ch chan T) {
	select {
	case <-ch:
	default:
	}
}

func (d *driver) currentTurn() string {
	d.cancelMu.Lock()
	defer d.cancelMu.Unlock()
	return d.turnID
}

// emit writes one event Envelope (no correlation id) as a single JSON line.
func (d *driver) emit(turnID, typ string, data any) { d.emitID(turnID, "", typ, data) }

// emitID writes one Envelope as a single JSON line (stdout). Concurrency-safe. id
// correlates a request with its response (permission/ask); events leave it empty.
func (d *driver) emitID(turnID, id, typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.out.Encode(wire.Envelope{
		Version: wire.StreamJSONVersion, Type: typ, ID: id, TurnID: turnID, Data: raw,
	})
}

// writerFunc adapts a func to io.Writer (for SetOutput).
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
