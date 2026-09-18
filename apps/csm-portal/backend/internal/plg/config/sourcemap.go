// Package config loads runtime configuration for the PLG CS portal backend.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/domain"
)

// PortalFields are the fields the portal can fill from an incoming record.
//
// A source map may name any of these on the left; anything else is a typo and
// fails at startup rather than silently never filling a column.
var PortalFields = []string{
	"organizationName",
	"registeredEmail",
	"createdOn",
	"initiatedPlatform",
	"countryName",
	"companyNameFromDomain",
	"companyId",
	"firstName",
	"lastName",
}

// RequiredPortalFields cannot be left unmapped: without them there is no
// organisation to create and no person to attribute it to.
var RequiredPortalFields = []string{"organizationName", "registeredEmail"}

// AttributeMapping is one extra field worth storing, beyond the portal's own
// columns.
type AttributeMapping struct {
	// From is the key on the incoming record.
	From string `json:"from"`
	// Label is a human-readable name. Nothing renders it yet; it is here so a
	// later screen does not have to invent one.
	Label string `json:"label"`
	// Scope decides whether the value attaches to the customer or to one
	// platform under it. The ingest cannot infer this.
	Scope domain.AttributeScope `json:"scope"`
	// Name is the portal-side name, filled in from the map's key.
	Name string `json:"-"`
}

// SourceMap is everything about how the upstream source words things.
//
// Every section reads portal-side on the left, source-side on the right, so the
// file answers one question consistently: "where does each of our own things
// come from?". That direction also makes the startup checks trivial — each of
// our fields is claimed at most once, and "is everything required mapped?" is a
// lookup rather than a reverse scan.
//
// Changing source is then a config change. When body.account_name becomes
// account_name, one line moves and nothing is rebuilt.
type SourceMap struct {
	recordsPath string
	fields      map[string]string // portal field  -> source key
	claimed     map[string]string // source key    -> portal field
	platforms   map[string]platformMapping
	attributes  map[string]AttributeMapping // source key -> mapping
}

// platformMapping keeps the source value as written alongside the product code,
// so an error can quote what the author typed.
type platformMapping struct {
	Source string
	Code   string
}

type sourceMapFile struct {
	RecordsPath string                      `json:"recordsPath"`
	Fields      map[string]string           `json:"fields"`
	Platforms   map[string]json.RawMessage  `json:"platforms"`
	Attributes  map[string]AttributeMapping `json:"attributes"`
}

var (
	attributeNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)
	productCodeRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

func emptySourceMap() *SourceMap {
	return &SourceMap{
		fields:     map[string]string{},
		claimed:    map[string]string{},
		platforms:  map[string]platformMapping{},
		attributes: map[string]AttributeMapping{},
	}
}

// LoadSourceMap reads the source map from path.
//
// Unlike the rest of the configuration this file is required whenever ingestion
// is expected to work: without a `fields` section the portal has no idea which
// key holds an organisation name. A missing file therefore loads empty and lets
// Validate refuse it, rather than pretending ingestion is configured.
func LoadSourceMap(path string) (*SourceMap, error) {
	if strings.TrimSpace(path) == "" {
		return emptySourceMap(), nil
	}

	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		if os.IsNotExist(err) {
			return emptySourceMap(), nil
		}
		return nil, fmt.Errorf("read source map %s: %w", path, err)
	}

	var file sourceMapFile
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse source map %s: %w", path, err)
	}
	return buildSourceMap(file)
}

func buildSourceMap(file sourceMapFile) (*SourceMap, error) {
	m := emptySourceMap()
	m.recordsPath = strings.TrimSpace(file.RecordsPath)

	if err := m.buildFields(file.Fields); err != nil {
		return nil, err
	}
	if err := m.buildPlatforms(file.Platforms); err != nil {
		return nil, err
	}
	if err := m.buildAttributes(file.Attributes); err != nil {
		return nil, err
	}
	return m, nil
}

// buildFields indexes the portal-field mappings.
func (m *SourceMap) buildFields(raw map[string]string) error {
	known := make(map[string]bool, len(PortalFields))
	for _, f := range PortalFields {
		known[f] = true
	}

	for portal, key := range raw {
		portal, key = strings.TrimSpace(portal), strings.TrimSpace(key)
		if !known[portal] {
			return fmt.Errorf("source map: %q is not a field the portal can fill (it knows: %s)",
				portal, strings.Join(PortalFields, ", "))
		}
		if key == "" {
			return fmt.Errorf("source map: field %q maps to nothing", portal)
		}
		if other, dup := m.claimed[key]; dup {
			return fmt.Errorf("source map: %q and %q both read the key %q", other, portal, key)
		}
		m.fields[portal] = key
		m.claimed[key] = portal
	}

	var missing []string
	for _, required := range RequiredPortalFields {
		if m.fields[required] == "" {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("source map: %s must be mapped — without them there is no "+
			"organisation to create and nobody to attribute it to", strings.Join(missing, " and "))
	}
	return nil
}

// buildPlatforms indexes the platform vocabulary.
//
// The right-hand side takes a string or a list, because several source values
// can mean one product — "choreo" and "wso2 api manager" are both the API
// Platform — and JSON cannot repeat a key.
//
// An empty value means "this product exists, and nobody has learned what the
// source calls it yet". That is what lets the file list every product as a
// template with blanks to fill in, instead of only the ones already known. It
// is safe to leave blank: an unmapped incoming value is still upper-cased and
// tried as a product code, and one that matches nothing is a 400 naming it, so
// a missing mapping is loud rather than silent.
func (m *SourceMap) buildPlatforms(raw map[string]json.RawMessage) error {
	for code, encoded := range raw {
		code = strings.ToUpper(strings.TrimSpace(code))
		if !productCodeRe.MatchString(code) {
			return fmt.Errorf("source map: platform key %q is not a product code "+
				"(UPPER_SNAKE_CASE, 2-64 characters)", code)
		}

		values, err := decodeStringOrList(encoded)
		if err != nil {
			return fmt.Errorf("source map: platform %s must map to a string or a list of strings", code)
		}
		// [] or "" — not mapped yet. Nothing to index, nothing to complain about.
		if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
			continue
		}

		for _, value := range values {
			value = strings.TrimSpace(value)
			// Blank inside a populated list is a slip, not a placeholder: the
			// author meant to write something and did not.
			if value == "" {
				return fmt.Errorf("source map: platform %s has an empty value in its list", code)
			}
			key := strings.ToLower(value)
			if other, dup := m.platforms[key]; dup {
				return fmt.Errorf("source map: source value %q is claimed by both %s and %s",
					value, other.Code, code)
			}
			m.platforms[key] = platformMapping{Source: value, Code: code}
		}
	}
	return nil
}

// buildAttributes indexes the extra fields worth storing.
func (m *SourceMap) buildAttributes(raw map[string]AttributeMapping) error {
	for name, entry := range raw {
		name = strings.ToLower(strings.TrimSpace(name))
		entry.Name = name
		entry.From = strings.TrimSpace(entry.From)
		entry.Label = strings.TrimSpace(entry.Label)
		if entry.Scope == "" {
			entry.Scope = domain.ScopeOrganization
		}

		switch {
		case !attributeNameRe.MatchString(name):
			return fmt.Errorf("source map: attribute name %q must be lower_snake_case, "+
				"2-63 characters, starting with a letter", name)
		case entry.From == "":
			return fmt.Errorf("source map: attribute %q reads from nothing", name)
		case entry.Scope != domain.ScopeOrganization && entry.Scope != domain.ScopePlatform:
			return fmt.Errorf("source map: attribute %q has scope %q (expected organization or platform)",
				name, entry.Scope)
		}

		// A key that fills one of the portal's own columns must not also become a
		// loose attribute: the same value would land in two places and which one
		// anybody reads would be a coin toss.
		if portal, taken := m.claimed[entry.From]; taken {
			return fmt.Errorf("source map: key %q already fills %q, so it cannot also be "+
				"stored as the attribute %q", entry.From, portal, name)
		}
		if other, dup := m.attributes[entry.From]; dup {
			return fmt.Errorf("source map: attributes %q and %q both read the key %q",
				other.Name, name, entry.From)
		}
		m.attributes[entry.From] = entry
	}
	return nil
}

// decodeStringOrList accepts "asgardeo" or ["choreo", "wso2 api manager"].
func decodeStringOrList(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, fmt.Errorf("neither a string nor a list of strings")
}

// RecordsPath is the envelope key holding the array of records, or "" when the
// payload is a single record.
func (m *SourceMap) RecordsPath() string {
	if m == nil {
		return ""
	}
	return m.recordsPath
}

// FieldKey returns the source key a portal field reads, or "" when unmapped.
//
// Unmapped is a normal state, not an error: it means the source does not send
// that field. When it starts to, one line in the map is the whole change.
func (m *SourceMap) FieldKey(portalField string) string {
	if m == nil {
		return ""
	}
	return m.fields[portalField]
}

// IsFieldKey reports whether a source key fills one of the portal's own columns.
func (m *SourceMap) IsFieldKey(sourceKey string) bool {
	if m == nil {
		return false
	}
	_, ok := m.claimed[sourceKey]
	return ok
}

// ResolvePlatform translates a source platform value into a product code.
//
// An unmapped value is upper-cased and returned as-is, so a source already
// sending product codes needs no entries at all. Either way the code is only a
// candidate: the ingest still has to find a product with it.
func (m *SourceMap) ResolvePlatform(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if m != nil {
		if mapping, ok := m.platforms[strings.ToLower(trimmed)]; ok {
			return mapping.Code
		}
	}
	return strings.ToUpper(trimmed)
}

// PlatformTargets lists the product codes the map points at, keyed by the source
// value as written, so startup can check each code exists.
func (m *SourceMap) PlatformTargets() map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m.platforms))
	for _, mapping := range m.platforms {
		out[mapping.Source] = mapping.Code
	}
	return out
}

// LookupAttribute returns the mapping for a source key, if it has one.
func (m *SourceMap) LookupAttribute(sourceKey string) (AttributeMapping, bool) {
	if m == nil {
		return AttributeMapping{}, false
	}
	entry, ok := m.attributes[sourceKey]
	return entry, ok
}

// Counts reports what was loaded, for the startup log.
func (m *SourceMap) Counts() (fields, platforms, attributes int) {
	if m == nil {
		return 0, 0, 0
	}
	return len(m.fields), len(m.platforms), len(m.attributes)
}

// AttributeByName finds a mapping by its portal-side name.
func (m *SourceMap) AttributeByName(name string) (AttributeMapping, bool) {
	if m == nil {
		return AttributeMapping{}, false
	}
	for _, entry := range m.attributes {
		if entry.Name == name {
			return entry, true
		}
	}
	return AttributeMapping{}, false
}
