package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/config"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/repository"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/service"
)

// backoffCeiling bounds the wait after repeated queue failures. A queue that is
// down should not be hammered every interval, and should not take an hour to be
// noticed once it returns.
const backoffCeiling = 5 * time.Minute

// Poller consumes registration events from the queue and feeds them into the
// same ingest path the webhook endpoint uses.
//
// The two share a service deliberately: however a registration reaches the
// portal, the person, organisation and pairing are written by exactly the same
// code, under the same rules.
type Poller struct {
	client    *Client
	ingest    service.IngestService
	failures  repository.FailureRepository
	interval  time.Duration
	maxDrain  int
	accepted  map[string]bool
	failCount int
}

// NewPoller wires a Poller. It does nothing until Run is called.
func NewPoller(
	cfg config.QueueConfig,
	ingest service.IngestService,
	failures repository.FailureRepository,
) *Poller {
	accepted := make(map[string]bool, len(cfg.EventTypes))
	for _, t := range cfg.EventTypes {
		if trimmed := strings.TrimSpace(t); trimmed != "" {
			accepted[strings.ToLower(trimmed)] = true
		}
	}

	return &Poller{
		client:   NewClient(cfg),
		ingest:   ingest,
		failures: failures,
		interval: time.Duration(cfg.PollIntervalSeconds) * time.Second,
		maxDrain: cfg.DrainMaxBatches,
		accepted: accepted,
	}
}

// Run polls until ctx is cancelled.
//
// It polls once immediately rather than waiting out the first interval, so a
// restart picks up a waiting backlog straight away.
func (p *Poller) Run(ctx context.Context) {
	slog.Info("queue poller started", "interval", p.interval.String())

	for {
		p.pollOnce(ctx)

		wait := p.interval
		if p.failCount > 0 {
			wait = p.backoff()
		}

		select {
		case <-ctx.Done():
			slog.Info("queue poller stopped")
			return
		case <-time.After(wait):
		}
	}
}

// backoff grows the wait while the queue is unreachable, doubling each failure
// up to the ceiling.
func (p *Poller) backoff() time.Duration {
	wait := p.interval
	for i := 1; i < p.failCount && wait < backoffCeiling; i++ {
		wait *= 2
	}
	if wait > backoffCeiling {
		wait = backoffCeiling
	}
	return wait
}

// pollOnce drains the queue, up to maxDrain batches.
//
// Draining matters because the interval is chosen for an idle queue. Without
// it, a backlog of 500 events at a batch size of 50 would need ten intervals to
// clear, and the portal would look like it was losing registrations when it was
// only being slow.
func (p *Poller) pollOnce(ctx context.Context) {
	for batch := 0; batch < p.maxDrain; batch++ {
		if ctx.Err() != nil {
			return
		}

		resp, err := p.client.Consume(ctx)
		if err != nil {
			// A cancelled context during shutdown is not a queue failure.
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			p.failCount++
			slog.Error("queue consume failed", "attempt", p.failCount, "err", err)
			return
		}
		if p.failCount > 0 {
			slog.Info("queue reachable again", "afterFailedAttempts", p.failCount)
			p.failCount = 0
		}

		if resp.Count == 0 {
			return
		}

		processed, failed := p.handle(ctx, resp.Events)
		slog.Info("queue batch consumed",
			"events", resp.Count, "processed", processed,
			"failed", failed, "remaining", resp.Remaining)

		// The queue says nothing is waiting, so stop and let the interval run.
		//
		// A long poll does not change this: it only blocks while the queue is
		// empty, and an empty queue is exactly the case above. When events
		// remain, the next call returns immediately.
		if resp.Remaining == 0 {
			return
		}
	}
	slog.Warn("queue drain capped, more may be waiting", "batches", p.maxDrain)
}

// handle processes one batch, record by record.
//
// One queue event can carry several registrations — a cohort notification
// carries everything added since it last fired — so the unit of success is the
// record, not the event. A record the portal cannot use must not stop the ones
// behind it, and each is written independently of the others.
func (p *Poller) handle(ctx context.Context, events []Event) (processed, failed int) {
	for _, event := range events {
		if len(p.accepted) > 0 && !p.accepted[strings.ToLower(event.EventType)] {
			p.recordFailure(ctx, event, event.Payload,
				errors.New("event type is not one this portal handles: "+event.EventType))
			failed++
			continue
		}

		records, err := p.ingest.Split(event.Payload)
		if err != nil {
			p.recordFailure(ctx, event, event.Payload, err)
			failed++
			continue
		}
		// A delivery with nothing to add is normal, not a failure: a cohort can
		// fire having gained no companies.
		if len(records) == 0 {
			continue
		}

		for _, rec := range records {
			if _, err := p.ingest.RegisterRecord(ctx, rec); err != nil {
				// The failure row keeps the individual record, not the whole
				// envelope, so it can be replayed on its own once fixed.
				p.recordFailure(ctx, event, rec, err)
				failed++
				continue
			}
			processed++
		}
	}
	return processed, failed
}

// recordFailure keeps an event that could not be processed.
//
// This is the last chance to hold on to it: the queue deleted it the moment it
// was handed over, so if this write is skipped the registration is gone. A
// failure to record the failure is therefore logged with the whole payload,
// which is the only remaining place it could be recovered from.
func (p *Poller) recordFailure(ctx context.Context, event Event, payload json.RawMessage, cause error) {
	var receivedAt *string
	if !event.ReceivedAt.IsZero() {
		formatted := event.ReceivedAt.Format(time.RFC3339)
		receivedAt = &formatted
	}

	err := p.failures.Record(ctx, repository.IngestFailure{
		EventID:    event.ID,
		EventType:  event.EventType,
		ReceivedAt: receivedAt,
		Payload:    payload,
		Failure:    cause.Error(),
	})
	if err != nil {
		// The payload is logged only here, and only because the alternative is
		// losing the record entirely: the queue has already deleted it and the
		// database would not take the failure row. It carries customer data, so
		// this is the one line in the service that puts that in a log.
		slog.Error("EVENT LOST — could not record ingest failure",
			"eventID", event.ID, "err", err, "cause", cause, "payload", string(payload))
		return
	}
	slog.Warn("event not processed, recorded in plg_ingest_failure",
		"eventID", event.ID, "cause", cause)
}
