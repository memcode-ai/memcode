package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/events"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// pairAlphabet omits lookalike characters (0/O, 1/I/L) so a code read off a
// phone screen survives being retyped.
const pairAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// newPairCode mints a 6-character pairing code.
func newPairCode() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = pairAlphabet[int(b[i])%len(pairAlphabet)]
	}
	return string(b), nil
}

// offerPairing hands an unknown DIRECT sender a one-time pairing code the
// operator can approve (`memcode gateway pair approve <code>`). The reply is
// sent only when THIS message minted the request — repeats from the same
// sender, and anything past the pending cap, stay silent so the flow can't be
// used to spam through the bot.
func (r *runtime) offerPairing(ctx context.Context, inb channels.Inbound) {
	code, err := newPairCode()
	if err != nil {
		return
	}
	code, created, err := r.gw.CreatePairing(ctx, inb.Channel, inb.Principal, inb.Conversation, code, time.Now())
	if err != nil {
		fmt.Fprintf(r.out, "gateway: pairing request from %s %q not recorded: %v\n", inb.Channel, inb.Principal, err)
		return
	}
	if !created {
		return // request already pending; the sender already has a code
	}
	fmt.Fprintf(r.out, "gateway: pairing request %s from %s %q — approve with `memcode gateway pair approve %s`\n",
		code, inb.Channel, inb.Principal, code)
	r.event(ctx, events.KindGatewayPairingOffered, eventPayload{Channel: inb.Channel, Conversation: inb.Conversation, PrincipalID: inb.Principal, MessageID: inb.MessageID, Reason: code})
	if ch := r.byName[inb.Channel]; ch != nil {
		msg := fmt.Sprintf("This bot doesn't know you yet. Your pairing code is %s — the bot's owner can approve you with:\n\nmemcode gateway pair approve %s", code, code)
		_ = ch.Send(ctx, inb.Conversation, channels.Outbound{Text: msg})
	}
}

// maybeReload re-reads gateway.yaml when its mtime changes and swaps the
// runtime's settings, so a pairing approval (or any policy edit) takes effect
// without a restart. Only POLICY hot-reloads — allow-lists, projects, agents,
// per-channel knobs. Channel connections and schedules are wired at startup.
// Returns the mtime to carry to the next check.
func (r *runtime) maybeReload(last time.Time) time.Time {
	path, err := gwconfig.Path()
	if err != nil {
		return last
	}
	fi, err := os.Stat(path)
	if err != nil {
		return last // no file (or unreadable): keep what we have
	}
	if !fi.ModTime().After(last) {
		return last
	}
	s, err := gwconfig.Load()
	if err != nil {
		fmt.Fprintf(r.out, "gateway: gateway.yaml changed but did not parse (%v) — keeping previous settings\n", err)
		return fi.ModTime() // don't retry a broken file every tick
	}
	r.mu.Lock()
	r.settings = s
	r.mu.Unlock()
	if !last.IsZero() {
		fmt.Fprintf(r.out, "gateway: settings reloaded from gateway.yaml\n")
	}
	return fi.ModTime()
}
