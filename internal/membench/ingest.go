package membench

import (
	"os"
	"path/filepath"
	"time"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// Ingest writes each benchmark session as a real .memcode session log:
// .memcode/sessions/<SessionDoc.ID>/events.jsonl via the production
// sessionlog.Writer, with Record.TS carrying the benchmark timestamps and the
// file mtime pinned to the session time (recency ordering in sessionlog is
// mtime-based). Turn identity rides Record.Slug — unused by real chat
// sessions and excluded from Search's match haystack, so it labels without
// polluting retrieval.
func Ingest(root string, docs []SessionDoc) error {
	for _, doc := range docs {
		w, err := sessionlog.Open(root, doc.ID)
		if err != nil {
			return err
		}
		ts := doc.TS
		if ts.IsZero() {
			ts = time.Unix(0, 0).UTC()
		}
		w.Append(sessionlog.Record{TS: ts, Kind: sessionlog.KindSessionStarted, Mode: "membench"})
		for i, t := range doc.Turns {
			kind := sessionlog.KindUserMessage
			if t.Role == "assistant" {
				kind = sessionlog.KindAssistantMessage
			}
			w.Append(sessionlog.Record{
				// Spread turns a second apart so within-session order is
				// preserved even for readers that sort by timestamp.
				TS:   ts.Add(time.Duration(i) * time.Second),
				Kind: kind,
				Text: t.Text,
				Slug: t.ID,
			})
		}
		// Facts ride after the dialogue, as the cognition loop appends them.
		// Slug carries the fact's cited source turn so a fact hit surfaces
		// real evidence at either granularity.
		for i, f := range doc.Facts {
			w.Append(sessionlog.Record{
				TS:       ts.Add(time.Duration(len(doc.Turns)+i) * time.Second),
				Kind:     sessionlog.KindFacts,
				Text:     f.Fact,
				Entities: f.Entities,
				Slug:     f.Source,
			})
		}
		if err := w.Close(); err != nil {
			return err
		}
		if !doc.TS.IsZero() {
			ev := filepath.Join(root, config.DirName, "sessions", doc.ID, "events.jsonl")
			_ = os.Chtimes(ev, doc.TS, doc.TS)
		}
	}
	return nil
}
