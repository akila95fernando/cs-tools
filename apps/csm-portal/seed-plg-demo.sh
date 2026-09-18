#!/usr/bin/env bash
#
# Demo data for PLG inside the merged csm-portal stack.
#
# The caller is identified by a token, not a header — `x-jwt-assertion`, the
# same validated claim csm-portal's own requests carry.
#
# The token below is unsigned, which works only because the local backend runs
# with AUTH_TOKEN_VALIDATOR_ENABLED=false. Against a real deployment this script
# needs a real token — which is the correct amount of friction for something
# that creates data as a named engineer.
#
# Everything goes through the public API rather than SQL, so the seed exercises
# the same validation a user would hit: mandatory reasons, the forward-only
# stage rule, the option lists a SINGLE_SELECT must carry.
#
# Usage:  ./seed-plg-demo.sh [email]
set -euo pipefail

BFF="${BFF:-http://localhost:8085}"
AS="${1:-akilaf@wso2.com}"

# Trial dates are computed relative to today so the "trials ending soon" tile
# means something whatever day this is run. A fixed date stops exercising the
# 14-day window the moment it passes.
day() { python3 -c "import datetime,sys; print(datetime.date.today()+datetime.timedelta(days=int(sys.argv[1])))" "$1"; }

command -v jq >/dev/null || { echo "needs jq"; exit 1; }
curl -sf -o /dev/null "$BFF/health" || { echo "backend not reachable at $BFF"; exit 1; }

# An unsigned assertion carrying the two claims csm-portal's Auth middleware
# requires. PLG's resolver then turns the email into a "user".id.
JWT=$(python3 -c "
import base64, json
h = base64.urlsafe_b64encode(json.dumps({'alg':'none','typ':'JWT'}).encode()).rstrip(b'=')
p = base64.urlsafe_b64encode(json.dumps({'email':'$AS','userid':'seed-$AS'}).encode()).rstrip(b'=')
print((h + b'.' + p + b'.').decode())")

api() {   # api METHOD PATH [JSON]
    local method="$1" route="/plg$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -sS -X "$method" "$BFF$route" -H "x-jwt-assertion: $JWT" \
             -H 'Content-Type: application/json' -d "$body"
    else
        curl -sS -X "$method" "$BFF$route" -H "x-jwt-assertion: $JWT"
    fi
}

# Fail early and loudly if the caller is not PLG staff — a 403 here means the
# email has no `internal` role, and every request below would fail the same way.
me=$(api GET /me)
if ! jq -e '.id' >/dev/null 2>&1 <<<"$me"; then
    echo "cannot act as $AS: $me"
    echo "  the user needs a role named 'internal' — user_type is derived, not written"
    exit 1
fi
echo "→ acting as $(jq -r '.email' <<<"$me")"

echo "→ authoring playbooks"
TASKS_QUALIFY='[
 {"name":"Company profile","valueType":"STRING"},
 {"name":"Fit score","valueType":"NUMBER"},
 {"name":"Intent","valueType":"SINGLE_SELECT",
  "options":[{"code":"EVAL","label":"Evaluating"},
             {"code":"BUILD","label":"Building now"},
             {"code":"BROWSE","label":"Just browsing"}]}]'
TASKS_ONBOARD='[
 {"name":"Welcome call held","valueType":"BOOLEAN"},
 {"name":"Setup steps","valueType":"CHECKLIST",
  "options":[{"code":"KEYS","label":"API keys issued"},
             {"code":"SANDBOX","label":"Sandbox verified"},
             {"code":"DOCS","label":"Docs walked through"}]}]'
TASKS_RESCUE='[
 {"name":"Root cause","valueType":"STRING"},
 {"name":"Why did it stall?","valueType":"SINGLE_SELECT",
  "options":[{"code":"PRICE","label":"Pricing"},
             {"code":"TECH","label":"Technical blocker"},
             {"code":"QUIET","label":"Went quiet"}]},
 {"name":"Recovery call held","valueType":"BOOLEAN"}]'
TASKS_HOLD='[
 {"name":"Review date","valueType":"STRING"},
 {"name":"Health signals checked","valueType":"CHECKLIST",
  "options":[{"code":"USAGE","label":"Usage trend"},
             {"code":"TICKETS","label":"Open tickets"},
             {"code":"SPONSOR","label":"Sponsor still in role"}]}]'

playbook() {  # playbook PRODUCT NAME KIND STAGE TASKS
    api POST "/products/$1/playbooks" "$(jq -nc \
        --arg n "$2" --arg k "$3" --arg s "$4" --argjson t "$5" \
        '{name:$n, playbookType:$k, lifecycleStage:$s, tasks:$t}')" \
      | jq -r 'if .id then "  ok  " + .name else "  !!  " + (.message // "failed") end'
}

playbook IAM 'Qualify the registration'   PROGRESSIVE REGISTRATION         "$TASKS_QUALIFY"
playbook IAM 'First integration'          PROGRESSIVE PLG_CS_ELIGIBLE      "$TASKS_ONBOARD"
playbook IAM 'Drive to activation'        PROGRESSIVE FIRST_VALUE_ACHIEVED "$TASKS_ONBOARD"
playbook IAM 'Commercial conversation'    PROGRESSIVE ACTIVATED            "$TASKS_QUALIFY"
playbook IAM 'Win it back'                RECOVERY    PLG_CS_ELIGIBLE      "$TASKS_RESCUE"
playbook IAM 'Re-engage a quiet account'  RECOVERY    FIRST_VALUE_ACHIEVED "$TASKS_RESCUE"
playbook IAM 'Recover the relationship'   RECOVERY    ACTIVATED            "$TASKS_RESCUE"
playbook IAM 'Quarterly check-in'         SUSTAINING  ACTIVATED            "$TASKS_HOLD"
playbook IAM 'Keep the account warm'      SUSTAINING  COMMERCIAL           "$TASKS_HOLD"

echo "→ landing registrations"
# Straight to the ingest endpoint rather than through the queue: the poller is
# off in the merged local stack, and the registration path is the same either
# way — the queue only decides *when* a record arrives.
register() {  # register NAME EMAIL DOMAIN ID
    api POST /webhooks/registrations "$(jq -nc --arg n "$1" --arg e "$2" --arg d "$3" --arg i "$4" \
      '{added_companies:[{"body.account_name":$n,"body.account_owner_email":$e,
        "body.initiated_platform":"asgardeo","body.country":"Sri Lanka",
        "company_domain":$d,"company_id":$i,"created":"2026-09-01T09:00:00Z"}]}')" \
      | jq -r '"  " + (.status // "?") + "  " + (.organizationName // "")' 2>/dev/null || true
}
register 'Northwind Logistics' 'ops@northwind.example' northwind.example NW-001
register 'Fabrikam Robotics'   'dev@fabrikam.example'  fabrikam.example  FB-002
register 'Contoso Health'      'eng@contoso.example'   contoso.example   CT-003
register 'Tailspin Analytics'  'data@tailspin.example' tailspin.example  TS-004
register 'Litware Media'       'admin@litware.example' litware.example   LW-005

echo "→ acknowledging and moving pairings"
IDS="$(api POST /registrations/search '{"pagination":{"limit":50,"offset":0}}')"
orgid()  { jq -r --arg n "$1" '.registrations[] | select(.organizationName==$n) | .organizationId' <<<"$IDS" | head -1; }
pairid() { jq -r --arg n "$1" '.registrations[] | select(.organizationName==$n) | .orgPlatformId'  <<<"$IDS" | head -1; }
ack()    { api POST "/registrations/$(pairid "$1")/acknowledge" '{}' > /dev/null; }
patch()  { api PATCH "/organizations/$(orgid "$1")/products/IAM" "$2" | jq -r 'if .message then "  ! " + .message else empty end'; }

for org in 'Northwind Logistics' 'Fabrikam Robotics' 'Contoso Health' 'Tailspin Analytics' 'Litware Media'; do
    ack "$org"
done

# Every axis change carries a reason — the BFF, the slice and a NOT NULL all
# refuse it otherwise.
patch 'Northwind Logistics' '{"lifecycleStage":"PLG_CS_ELIGIBLE","reason":"Real org, clear API use case"}'
patch 'Northwind Logistics' '{"lifecycleStage":"FIRST_VALUE_ACHIEVED","reason":"First successful API call in production"}'
patch 'Northwind Logistics' '{"lifecycleStage":"ACTIVATED","reason":"Steady daily traffic for three weeks"}'
patch 'Northwind Logistics' '{"lifecycleStage":"COMMERCIAL","reason":"Signed a PayG agreement"}'
patch 'Northwind Logistics' '{"subscriptionTier":"PAYG","reason":"Converted from trial after the pilot"}'

patch 'Fabrikam Robotics' '{"lifecycleStage":"PLG_CS_ELIGIBLE","reason":"Strong fit for integration"}'
patch 'Fabrikam Robotics' '{"lifecycleStage":"FIRST_VALUE_ACHIEVED","reason":"Completed their first workflow"}'
patch 'Fabrikam Robotics' '{"subscriptionTier":"FREE","reason":"On the free tier while evaluating"}'

patch 'Contoso Health' '{"lifecycleStage":"PLG_CS_ELIGIBLE","reason":"Genuine evaluation, mid-size team"}'
patch 'Contoso Health' '{"subscriptionTier":"TRIAL","reason":"Started a 30-day trial"}'
patch 'Contoso Health' "{\"trialEndDate\":\"$(day 10)\"}"
patch 'Contoso Health' '{"healthState":"AT_RISK","reason":"No logins for eleven days and the sponsor has gone quiet"}'

patch 'Tailspin Analytics' '{"lifecycleStage":"PLG_CS_ELIGIBLE","reason":"Data team evaluating in earnest"}'
patch 'Tailspin Analytics' '{"lifecycleStage":"FIRST_VALUE_ACHIEVED","reason":"Ingested their first dataset"}'
patch 'Tailspin Analytics' '{"lifecycleStage":"ACTIVATED","reason":"Daily pipeline runs for a month"}'
# The extended trial exercises the second date and the tier that replaced
# ENTERPRISE. Both dates are set deliberately: the original trial date stays on
# the record after the extension is granted, which is the point of keeping two
# columns, and the UI offers only the extended one while this tier is current.
# The original trial is already in the PAST and the extension is a week out.
# That combination is the test: counted by the original date this pairing would
# be overdue and invisible, counted by the tier's own date it is due next week.
patch 'Tailspin Analytics' "{\"trialEndDate\":\"$(day -12)\"}"
patch 'Tailspin Analytics' '{"subscriptionTier":"TRIAL_EXTENDED","reason":"Granted four more weeks to finish the pilot"}'
patch 'Tailspin Analytics' "{\"trialExtendedDate\":\"$(day 7)\"}"

patch 'Litware Media' '{"lifecycleStage":"ABANDONED","reason":"Disposable email domain, no real organisation"}'

echo
echo "✓ seeded"
api GET /analytics/dashboard | jq -c '.summary'
