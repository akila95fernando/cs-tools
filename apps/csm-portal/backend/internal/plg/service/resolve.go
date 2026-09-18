package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/apierror"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/config"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/domain"
)

// record is one incoming registration, still in the source's own vocabulary.
type record map[string]json.RawMessage

// splitRecords pulls the individual registrations out of one delivered payload.
//
// Three shapes are accepted, because all three occur:
//
//   - an envelope holding an array at the configured recordsPath — what Moesif
//     sends, and why one queue event can carry several registrations
//   - a bare array of records — a batch replayed by hand
//   - a single record — one row replayed from plg_ingest_failure
//
// An envelope whose array is absent or empty yields nothing at all, with no
// error. That is the normal case for a cohort notification that carries no
// additions, and treating it as a failure would fill the failure table with
// events that were never wrong.
func splitRecords(raw json.RawMessage, sourceMap *config.SourceMap) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, apierror.Validation("empty payload")
	}

	if strings.HasPrefix(trimmed, "[") {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, apierror.Validation("payload is not a list of records: " + err.Error())
		}
		return list, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, apierror.Validation("payload is not a JSON object: " + err.Error())
	}

	path := sourceMap.RecordsPath()
	if path == "" {
		return []json.RawMessage{raw}, nil
	}

	nested, present := envelope[path]
	if !present {
		// Not an envelope of the configured shape. It may still be a single
		// record — which is what a replayed failure row looks like — so fall
		// back to treating it as one rather than rejecting it.
		return []json.RawMessage{raw}, nil
	}

	var list []json.RawMessage
	if err := json.Unmarshal(nested, &list); err != nil {
		return nil, apierror.Validation(fmt.Sprintf("%q is not a list of records: %s", path, err.Error()))
	}
	return list, nil
}

// resolveRecord turns one source record into the portal's own terms.
//
// Every field is looked up by the key the source map names. A required field
// whose key is missing from the payload is reported with the key it looked for —
// which is the whole point of the map being configuration: when the source
// renames a field, the error says exactly which key stopped matching instead of
// complaining that some unrelated field is empty.
func resolveRecord(raw json.RawMessage, sourceMap *config.SourceMap) (domain.Registration, error) {
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return domain.Registration{}, apierror.Validation("record is not a JSON object: " + err.Error())
	}

	var reg domain.Registration
	get := func(portalField string) (string, error) {
		key := sourceMap.FieldKey(portalField)
		if key == "" {
			return "", nil // the source does not send this field
		}
		value, present := rec[key]
		if !present {
			for _, required := range config.RequiredPortalFields {
				if portalField == required {
					return "", apierror.Validation(fmt.Sprintf(
						"expected %s at key %q, which this record does not contain "+
							"— check the fields section of the source map", portalField, key))
				}
			}
			return "", nil
		}
		text, ok := jsonScalarToText(value)
		if !ok {
			return "", apierror.Validation(fmt.Sprintf(
				"%s at key %q is not a value this portal can store", portalField, key))
		}
		return text, nil
	}

	var err error
	if reg.OrganizationName, err = get("organizationName"); err != nil {
		return reg, err
	}
	if reg.RegisteredEmail, err = get("registeredEmail"); err != nil {
		return reg, err
	}
	if reg.InitiatedPlatform, err = get("initiatedPlatform"); err != nil {
		return reg, err
	}
	if reg.CountryName, err = get("countryName"); err != nil {
		return reg, err
	}
	if reg.CompanyNameFromDomain, err = get("companyNameFromDomain"); err != nil {
		return reg, err
	}
	if reg.CompanyID, err = get("companyId"); err != nil {
		return reg, err
	}
	if reg.FirstName, err = get("firstName"); err != nil {
		return reg, err
	}
	if reg.LastName, err = get("lastName"); err != nil {
		return reg, err
	}

	createdRaw, err := get("createdOn")
	if err != nil {
		return reg, err
	}
	if reg.CreatedOn, err = domain.ParseSourceTime(createdRaw); err != nil {
		return reg, err
	}

	reg.Extra = resolveExtras(rec, sourceMap)
	return reg, nil
}

// resolveExtras keeps the extra fields the source map names.
//
// Keys that fill one of the portal's own columns are skipped — the map refuses
// to let a key do both jobs, so this is only belt and braces — and everything
// unmapped is ignored, which is what stops a new source field from breaking a
// delivery.
func resolveExtras(rec record, sourceMap *config.SourceMap) map[string]string {
	var extras map[string]string
	for key, value := range rec {
		if sourceMap.IsFieldKey(key) {
			continue
		}
		mapping, mapped := sourceMap.LookupAttribute(key)
		if !mapped {
			continue
		}
		text, ok := jsonScalarToText(value)
		if !ok {
			continue
		}
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		if extras == nil {
			extras = make(map[string]string, 4)
		}
		extras[mapping.Name] = text
	}
	return extras
}

// jsonScalarToText renders one JSON value as the text that will be stored.
//
// Objects and arrays are refused rather than flattened into their JSON source: a
// nested structure squeezed into a text column is not something anyone can
// analyse later, so it is better to notice it is missing than to find it
// unusable. A JSON null reads as absent.
func jsonScalarToText(value json.RawMessage) (string, bool) {
	if strings.TrimSpace(string(value)) == "null" {
		return "", true
	}
	var probe any
	if err := json.Unmarshal(value, &probe); err != nil {
		return "", false
	}
	switch v := probe.(type) {
	case string:
		return strings.TrimSpace(v), true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
}

// attributesOf turns the resolved extras into rows to store, in a stable order
// so a replayed delivery writes them the same way twice.
func attributesOf(extras map[string]string, sourceMap *config.SourceMap) []domain.OrganizationAttribute {
	out := make([]domain.OrganizationAttribute, 0, len(extras))
	for _, name := range sortedKeys(extras) {
		mapping, ok := sourceMap.AttributeByName(name)
		if !ok {
			continue
		}
		out = append(out, domain.OrganizationAttribute{
			Name:        name,
			Value:       extras[name],
			SourceField: mapping.From,
			Scope:       mapping.Scope,
		})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
