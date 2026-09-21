DROP VIEW IF EXISTS plg_run_task_reason_v;
DROP VIEW IF EXISTS plg_work_queue_v;
DROP VIEW IF EXISTS plg_org_platform_v;
DROP VIEW IF EXISTS plg_organization_v;
DROP VIEW IF EXISTS plg_playbook_run_v;
DROP VIEW IF EXISTS plg_user_v;

DROP TABLE IF EXISTS plg_note_revision;
DROP TABLE IF EXISTS plg_ingest_failure;
DROP TABLE IF EXISTS plg_organization_attribute;
DROP TABLE IF EXISTS plg_note;
DROP TABLE IF EXISTS plg_playbook_run_task;
DROP TABLE IF EXISTS plg_playbook_run;
DROP TABLE IF EXISTS plg_playbook_task;
DROP TABLE IF EXISTS plg_playbook;
DROP TABLE IF EXISTS plg_lifecycle_history;
DROP TABLE IF EXISTS plg_org_platform;
DROP TABLE IF EXISTS plg_organization;
DROP TABLE IF EXISTS plg_person;
DROP TABLE IF EXISTS plg_lifecycle_stage;
DROP TABLE IF EXISTS plg_product;

DROP FUNCTION IF EXISTS plg_check_stage_move();
DROP FUNCTION IF EXISTS plg_check_run_product();
DROP FUNCTION IF EXISTS plg_set_updated_at();

-- Before the enum: the function's return type names it.
DROP FUNCTION IF EXISTS plg_applicable_playbook_types(plg_health_enum);
DROP FUNCTION IF EXISTS plg_current_period_end_date(
    plg_subscription_tier_enum, DATE, DATE);

DROP TYPE IF EXISTS plg_playbook_type_enum;
DROP TYPE IF EXISTS plg_health_enum;
DROP TYPE IF EXISTS plg_subscription_tier_enum;
DROP TYPE IF EXISTS plg_task_value_type_enum;
DROP TYPE IF EXISTS plg_lifecycle_stage_enum;
