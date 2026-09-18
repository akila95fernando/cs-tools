package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/apierror"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/config"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/domain"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/repository"
)

// IngestService accepts the registration feed.
//
// The portal treats the feed as authoritative about what the source knows and
// silent about everything else. It stores exactly the fields it was sent — the
// gaps in a sparse second registration are filled at read time from the
// registrant's other organisations, never by copying values in at write time.
// Storage stays a faithful record of what arrived.
//
// Nothing here knows the source's vocabulary. Which key holds which field is
// configuration, so changing source is a config change.
type IngestService interface {
	// Split pulls the individual records out of one delivered payload. One
	// delivery can carry several registrations.
	Split(raw json.RawMessage) ([]json.RawMessage, error)
	// RegisterRecord lands one record.
	RegisterRecord(ctx context.Context, raw json.RawMessage) (*domain.IngestResult, error)
	// RegisterPayload splits and lands a whole delivery, reporting per record.
	RegisterPayload(ctx context.Context, raw json.RawMessage) (*domain.IngestBatchResult, error)
}

type ingestService struct {
	repo      repository.IngestRepository
	sourceMap *config.SourceMap
}

// NewIngestService wires an IngestService over its repository and the loaded
// source map.
func NewIngestService(repo repository.IngestRepository, sourceMap *config.SourceMap) IngestService {
	return &ingestService{repo: repo, sourceMap: sourceMap}
}

func (s *ingestService) Split(raw json.RawMessage) ([]json.RawMessage, error) {
	return splitRecords(raw, s.sourceMap)
}

// RegisterRecord resolves one record through the source map and lands it.
//
// The two mandatory fields are checked here rather than in the resolver, because
// "the key was missing from the payload" and "the key was there but empty" are
// different problems and deserve different messages.
func (s *ingestService) RegisterRecord(ctx context.Context, raw json.RawMessage) (*domain.IngestResult, error) {
	reg, err := resolveRecord(raw, s.sourceMap)
	if err != nil {
		return nil, err
	}

	reg.RegisteredEmail = strings.ToLower(strings.TrimSpace(reg.RegisteredEmail))
	reg.OrganizationName = strings.TrimSpace(reg.OrganizationName)

	if reg.RegisteredEmail == "" {
		return nil, apierror.Validation("the registrant's email is empty (source key: " +
			s.sourceMap.FieldKey("registeredEmail") + ")")
	}
	if !strings.Contains(reg.RegisteredEmail, "@") {
		return nil, apierror.Validation("the registrant's email is not an address: " + reg.RegisteredEmail)
	}
	if reg.OrganizationName == "" {
		return nil, apierror.Validation("the organisation name is empty (source key: " +
			s.sourceMap.FieldKey("organizationName") + ")")
	}

	// The source may call a platform something other than the portal's product
	// code. The source map holds that vocabulary; an unmapped value is
	// upper-cased and used as-is, so a source already sending codes needs no
	// entries.
	reg.InitiatedPlatform = s.sourceMap.ResolvePlatform(reg.InitiatedPlatform)

	return s.repo.Register(ctx, reg, attributesOf(reg.Extra, s.sourceMap))
}

// RegisterPayload lands a whole delivery, record by record.
//
// One bad record must not reject the delivery: the queue deletes on consume, so
// a rejection would lose the good records with it. Each result carries its own
// status instead, and the caller decides what to do with the failures.
func (s *ingestService) RegisterPayload(ctx context.Context, raw json.RawMessage) (*domain.IngestBatchResult, error) {
	records, err := s.Split(raw)
	if err != nil {
		return nil, err
	}

	out := &domain.IngestBatchResult{Results: make([]domain.IngestResult, 0, len(records))}
	for _, rec := range records {
		res, err := s.RegisterRecord(ctx, rec)
		if err != nil {
			out.Failed++
			out.Results = append(out.Results, domain.IngestResult{
				Status:  "rejected",
				Message: err.Error(),
			})
			continue
		}
		out.Accepted++
		out.Results = append(out.Results, *res)
	}
	return out, nil
}
