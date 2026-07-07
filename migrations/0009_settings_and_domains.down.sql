BEGIN;
DROP INDEX IF EXISTS site_domains_default_idx;
DROP TABLE IF EXISTS site_domains;
DROP TABLE IF EXISTS settings;
COMMIT;
